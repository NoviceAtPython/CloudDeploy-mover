package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// DesktopPackagesName is the canonical state-key.
const DesktopPackagesName = "desktop_packages"

// DefaultDesktopPackages is the Milestone 4A minimum graphical-stack
// install list. The list is conservative: it gets us a working
// KWin Wayland compositor + the surrounding bits the runtime needs
// (D-Bus user session, xdg portals, PipeWire, kscreen-doctor). It
// intentionally does NOT pull in Plasma's full desktop UX -
// kwin_session brings up the compositor first; plasmashell can be
// layered on in a later milestone if needed.
//
// Versioned ordering matters: kwin-wayland MUST be present before
// plasma-workspace because plasma-workspace transitively pulls a
// pile of recommends and we want to lay the compositor down first.
var DefaultDesktopPackages = []string{
	// Wayland compositor + KWin runtime.
	"kwin-wayland",
	// Xwayland is required by Steam and many game launchers even
	// though the compositor itself is Wayland.
	"xwayland",
	"xauth",
	// Plasma 6 workspace bits (drag-in the right session pieces).
	"plasma-workspace",
	"plasma-desktop",
	// Usable desktop surface and expected KDE apps. A headless
	// streaming VM still needs a visible shell, file manager, terminal
	// and settings app so Sunshine captures more than an empty KWin
	// scene.
	"systemsettings",
	"dolphin",
	"konsole",
	"kate",
	"plasma-systemmonitor",
	"plasma-discover",
	"plasma-pa",
	"pavucontrol",
	"kio-extras",
	"ark",
	"gwenview",
	"okular",
	"kde-spectacle",
	"xdg-utils",
	// kscreen-doctor lives in the kde-plasma-desktop family. We pull
	// kscreen (the CLI) explicitly because drm_display_validate
	// shells out to it.
	"kscreen",
	// D-Bus user session - required for systemd --user under linger.
	"dbus-user-session",
	// Portals so cross-app file/screen sharing doesn't break under
	// Wayland. The -kde backend matches the KWin compositor.
	"xdg-desktop-portal",
	"xdg-desktop-portal-kde",
	// PipeWire (audio + screen capture) + WirePlumber session manager.
	// Sunshine wants both; even without Sunshine the desktop session
	// is unhappy without an audio server.
	"pipewire",
	"pipewire-pulse",
	"wireplumber",
	// Wayland + Qt6 client libraries.
	"wayland-utils",
	"qt6-wayland",
	// Qt 6.5+ kscreen-doctor pulls libxcb-cursor0 at runtime even
	// under QT_QPA_PLATFORM=wayland. Without it the binary aborts
	// before producing any output (the live VM hit this on 25.10).
	// Cheap to install; harmless under Wayland.
	"libxcb-cursor0",
	// Vulkan + Mesa diagnostics. Cheap to install, invaluable when
	// debugging "why doesn't this compositor see the GPU".
	"vulkan-tools",
	"mesa-utils",
	// drm-info: human-readable DRM probe; drm_display_validate uses
	// it as a secondary signal when kscreen-doctor output is sparse.
	"drm-info",
}

// DesktopPackages is the second Milestone 4A phase. It installs the
// minimum Wayland/KDE/PipeWire stack via apt.Transaction so the
// policy-rc.d guard covers the postinst service-start step. The
// install list defaults to DefaultDesktopPackages; tests can swap
// it via PackagesOverrideFn.
type DesktopPackages struct {
	// PackagesOverrideFn lets tests inject a smaller install list
	// (e.g. one that won't blow up on a developer-host apt cache
	// that doesn't have plasma-workspace). nil = use
	// DefaultDesktopPackages.
	PackagesOverrideFn func() []string
}

// Name implements Phase.
func (DesktopPackages) Name() string { return DesktopPackagesName }

// Run implements Phase. Side effects: apt-get install via the
// shared transaction. DryRun is honored by apt.Transaction.
func (p DesktopPackages) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if shouldSkip(deps.State, DesktopPackagesName) {
		log.Info("phase desktop-packages: already done; skipping")
		return nil
	}
	prevStatus := deps.State.Get(DesktopPackagesName).Status
	deps.State.MarkRunning(DesktopPackagesName)
	_ = deps.PersistState()

	pkgs := DefaultDesktopPackages
	if p.PackagesOverrideFn != nil {
		pkgs = p.PackagesOverrideFn()
	}
	retryAfterInterruption := prevStatus == state.StatusRunning || startupRecoveredPhase(deps, DesktopPackagesName)
	details := map[string]any{
		"packages":                 pkgs,
		"requested_packages":       pkgs,
		"package_ct":               len(pkgs),
		"retry_after_interruption": retryAfterInterruption,
	}
	log.Info("phase desktop-packages: installing minimum Wayland/KDE/PipeWire stack",
		"package_ct", len(pkgs))
	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Install(ctx, pkgs)
	})
	if err != nil {
		details["err"] = err.Error()
		deps.State.MarkFailed(DesktopPackagesName, "apt install desktop packages", err, true)
		deps.State.Get(DesktopPackagesName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase desktop-packages: %w", err)
	}
	installed, missing := p.verifyInstalled(ctx, deps, pkgs)
	details["installed_packages"] = installed
	details["missing_packages"] = missing
	if len(missing) > 0 {
		err := fmt.Errorf("desktop package install finished but packages are still missing: %s", strings.Join(missing, ", "))
		details["err"] = err.Error()
		deps.State.MarkFailed(DesktopPackagesName, "desktop package verification failed", err, true)
		deps.State.Get(DesktopPackagesName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase desktop-packages: %w", err)
	}
	if err := p.markManual(ctx, deps, pkgs); err != nil {
		log.Warn("phase desktop-packages: apt-mark manual failed (non-fatal)",
			"err", err)
		details["apt_mark_manual_warning"] = err.Error()
	} else {
		details["apt_mark_manual"] = true
	}
	if !deps.DryRun {
		if err := ensureKDEOpenHelpers(); err != nil {
			log.Warn("phase desktop-packages: KDE open-helper shim install failed (non-fatal)", "err", err)
			details["kde_open_helper_warning"] = err.Error()
		} else {
			details["kde_open_helpers"] = true
		}
		desk := deps.Profile.EffectiveDesktop()
		if shortcuts, err := ensureCoreDesktopShortcuts(ctx, deps, desk.User); err != nil {
			log.Warn("phase desktop-packages: desktop shortcut install failed (non-fatal)", "err", err)
			details["desktop_shortcut_warning"] = err.Error()
		} else {
			details["desktop_shortcuts"] = shortcuts
		}
		if err := ensureGamingEnvironment(desk.User); err != nil {
			log.Warn("phase desktop-packages: gaming environment install failed (non-fatal)", "err", err)
			details["gaming_environment_warning"] = err.Error()
		} else {
			details["gaming_environment"] = true
		}
	}
	deps.State.MarkDone(DesktopPackagesName, details)
	_ = deps.PersistState()
	log.Info("phase desktop-packages: done", "package_ct", len(pkgs))
	return nil
}

func startupRecoveredPhase(deps *Deps, phaseName string) bool {
	if deps == nil || deps.State == nil || deps.State.StartupRecovery == nil {
		return false
	}
	for _, recovered := range deps.State.StartupRecovery.RecoveredPhases {
		if recovered == phaseName {
			return true
		}
	}
	return false
}

func (p DesktopPackages) verifyInstalled(ctx context.Context, deps *Deps, pkgs []string) ([]string, []string) {
	if deps.DryRun {
		return append([]string(nil), pkgs...), nil
	}
	installed := make([]string, 0, len(pkgs))
	var missing []string
	for _, pkg := range pkgs {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"dpkg-query", "-W", "-f=${Status}", pkg},
			LogFile: "-",
			Timeout: 15 * time.Second,
		})
		if res.Err == nil && strings.TrimSpace(res.Stdout) == "install ok installed" {
			installed = append(installed, pkg)
			continue
		}
		missing = append(missing, pkg)
	}
	return installed, missing
}

func (p DesktopPackages) markManual(ctx context.Context, deps *Deps, pkgs []string) error {
	if deps.DryRun || len(pkgs) == 0 {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    append([]string{"apt-mark", "manual"}, pkgs...),
		Sudo:    true,
		LogFile: "-",
		Timeout: 2 * time.Minute,
	})
	if res.Err != nil {
		return fmt.Errorf("apt-mark manual: %w", res.Err)
	}
	return nil
}

func ensureKDEOpenHelpers() error {
	shims := map[string]string{
		"/usr/local/bin/kfmclient": `#!/usr/bin/env bash
exec /usr/bin/kde-open "$@"
`,
		"/usr/local/bin/kde-open6": `#!/usr/bin/env bash
exec /usr/bin/kde-open "$@"
`,
		"/usr/local/bin/kioclient6": `#!/usr/bin/env bash
exec /usr/bin/kioclient "$@"
`,
	}
	if err := os.MkdirAll("/usr/local/bin", 0o755); err != nil {
		return err
	}
	for path, body := range shims {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ensureCoreDesktopShortcuts(ctx context.Context, deps *Deps, user string) ([]string, error) {
	return ensureDesktopShortcuts(ctx, deps, user, []string{
		"org.kde.dolphin.desktop",
		"systemsettings.desktop",
		"org.kde.konsole.desktop",
		"google-chrome.desktop",
	})
}

const gamingEnvironmentBody = `# Managed by CloudDeploy v3.
# Native PlayStation controller path for Proton games that support DualSense.
PROTON_ENABLE_HIDRAW=1
SDL_JOYSTICK_HIDAPI_PS5=1
SDL_JOYSTICK_HIDAPI_PS4=1
`

func ensureGamingEnvironment(user string) error {
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("desktop user is empty")
	}
	dir := filepath.Join("/home", user, ".config", "environment.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "90-clouddeploy-playstation-controller.conf")
	if err := os.WriteFile(path, []byte(gamingEnvironmentBody), 0o644); err != nil {
		return err
	}
	return chownHomePathToUser(user, path)
}

// chownHomePathToUser repairs ownership after root-run deploy code creates
// directories under a user's home via os.MkdirAll. It chowns every path
// component from /home/<user> down to (and including) target to that user.
//
// Without this, os.MkdirAll run as root leaves the created parents (notably
// ~/.config and ~/.local/share/applications) owned by root, which silently
// blocks the user's own apps from writing their config/data - Chrome, Steam,
// JDownloader, etc. then fail to start and never draw a window, and the
// desktop menu can't refresh its cache so app icons go missing.
func chownHomePathToUser(user, target string) error {
	u, err := osuser.Lookup(user)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	home := filepath.Clean(filepath.Join("/home", user))
	target = filepath.Clean(target)
	rel, err := filepath.Rel(home, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		// target is not under the home dir; chown it directly.
		return os.Chown(target, uid, gid)
	}
	p := home
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		p = filepath.Join(p, part)
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func ensureDesktopShortcuts(ctx context.Context, deps *Deps, user string, desktopFiles []string) ([]string, error) {
	if strings.TrimSpace(user) == "" {
		return nil, fmt.Errorf("desktop user is empty")
	}
	desktopDir := filepath.Join("/home", user, "Desktop")
	if err := os.MkdirAll(desktopDir, 0o755); err != nil {
		return nil, err
	}
	var installed []string
	for _, name := range desktopFiles {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		body, err := readDesktopEntry(user, name)
		if err != nil {
			continue
		}
		if filepath.Base(name) == "steam.desktop" {
			body = wrapSteamDesktopEntry(body)
		}
		dst := filepath.Join(desktopDir, filepath.Base(name))
		if err := os.WriteFile(dst, body, 0o755); err != nil {
			return installed, err
		}
		installed = append(installed, filepath.Base(name))
	}
	if len(installed) > 0 {
		_ = run(ctx, deps, "", []string{"chown", "-R", user + ":" + user, desktopDir}, time.Minute, true)
	}
	return installed, nil
}

func readDesktopEntry(user, name string) ([]byte, error) {
	for _, dir := range []string{
		"/usr/share/applications",
		"/var/lib/flatpak/exports/share/applications",
		filepath.Join("/home", user, ".local/share/flatpak/exports/share/applications"),
	} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return body, nil
		}
	}
	return nil, fmt.Errorf("desktop entry %s not found", name)
}

func ensureSteamDesktopOverride(user string) error {
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("desktop user is empty")
	}
	body, err := readDesktopEntry(user, "steam.desktop")
	if err != nil {
		return err
	}
	dir := filepath.Join("/home", user, ".local", "share", "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, "steam.desktop")
	if err := os.WriteFile(dst, wrapSteamDesktopEntry(body), 0o644); err != nil {
		return err
	}
	return chownHomePathToUser(user, dst)
}

const steamPlayStationEnvPrefix = "env PROTON_ENABLE_HIDRAW=1 SDL_JOYSTICK_HIDAPI_PS5=1 SDL_JOYSTICK_HIDAPI_PS4=1 "

func wrapSteamDesktopEntry(body []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "Exec="):
			cmd := strings.TrimPrefix(line, "Exec=")
			if !strings.HasPrefix(cmd, steamPlayStationEnvPrefix) {
				lines[i] = "Exec=" + steamPlayStationEnvPrefix + cmd
			}
		case strings.HasPrefix(line, "Name="):
			lines[i] = "Name=Steam"
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out)
}
