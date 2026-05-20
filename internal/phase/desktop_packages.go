package phase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
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
	// Plasma 6 workspace bits (drag-in the right session pieces).
	"plasma-workspace",
	"plasma-desktop",
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
	deps.State.MarkRunning(DesktopPackagesName)
	_ = deps.PersistState()

	pkgs := DefaultDesktopPackages
	if p.PackagesOverrideFn != nil {
		pkgs = p.PackagesOverrideFn()
	}
	details := map[string]any{
		"packages":   pkgs,
		"package_ct": len(pkgs),
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
	deps.State.MarkDone(DesktopPackagesName, details)
	_ = deps.PersistState()
	log.Info("phase desktop-packages: done", "package_ct", len(pkgs))
	return nil
}
