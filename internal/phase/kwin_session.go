package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	statepkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// KWinSessionName is the canonical state-key.
const KWinSessionName = "kwin_session"

// kwinUnitBaseName is the base name (no .service) the phase picks
// based on the configured compositor mode.
type kwinUnitChoice struct {
	UnitName       string // file name; what `systemctl is-active` checks
	UnitTemplate   string // template body
	BinaryDesc     string // "kwin_wayland --drm" / "startplasma-wayland" / etc., for logs
	BannerName     string // for the Description line
	IncludeChvt    bool   // whether the unit chvt's before launch
	IsRealVT       bool   // realvt backends use a TTY-bound systemd unit
	IsWeston       bool   // diagnostic fallback
	JournalProcess string // strings to grep KWin journal for (kwin_wayland) vs Plasma (kwin_wayland for plasma too)
}

// Compositor-mode -> service-name registry. The realvt unit names
// match v2 ("plasma-realvt.service" / "kwin-realvt.service") so an
// operator's muscle memory carries across.
var kwinUnitChoices = map[string]map[string]kwinUnitChoice{
	// session_backend -> compositor_mode -> choice.
	"realvt": {
		"plasma": {
			UnitName:       "plasma-realvt.service",
			BinaryDesc:     "startplasma-wayland (Plasma session, includes kwin_wayland)",
			BannerName:     "CloudDeploy Plasma Wayland session (real-VT)",
			IncludeChvt:    true,
			IsRealVT:       true,
			JournalProcess: "kwin_wayland",
		},
		"kwin": {
			UnitName:       "kwin-realvt.service",
			BinaryDesc:     "kwin_wayland --drm --no-lockscreen (real-VT)",
			BannerName:     "CloudDeploy KWin Wayland session (real-VT)",
			IncludeChvt:    true,
			IsRealVT:       true,
			JournalProcess: "kwin_wayland",
		},
	},
	"user": {
		"kwin": {
			UnitName:       "clouddeploy-kwin-wayland.service",
			BinaryDesc:     "dbus-run-session -- kwin_wayland --drm (user session)",
			BannerName:     "CloudDeploy KWin Wayland session (user)",
			JournalProcess: "kwin_wayland",
		},
		"plasma": {
			UnitName:       "clouddeploy-plasma-wayland.service",
			BinaryDesc:     "dbus-run-session -- startplasma-wayland (user session)",
			BannerName:     "CloudDeploy Plasma Wayland session (user)",
			JournalProcess: "kwin_wayland",
		},
	},
	"weston": {
		"weston": {
			UnitName:       "weston-kms-session.service",
			BinaryDesc:     "weston --backend=drm-backend.so (diagnostic)",
			BannerName:     "CloudDeploy Weston KMS session (diagnostic)",
			IsWeston:       true,
			JournalProcess: "weston",
		},
	},
}

// KWinUnitName is the LEGACY constant kept for backwards compatibility
// with the Milestone 4A "user" backend default. Phases / doctor / docs
// that hard-coded the old name still see the same string.
const KWinUnitName = "clouddeploy-kwin-wayland.service"

// KWinUnitDefaultPath is the legacy install path under
// session_backend=user. Real-VT and Weston backends use the
// session-specific names from kwinUnitChoices.
const KWinUnitDefaultPath = "/etc/systemd/system/" + KWinUnitName

// realVTUnitTemplate is the v2-parity body for the real-VT
// plasma/kwin sessions. The unit takes ownership of /dev/tty<KwinVT>,
// PAM logs the user in, and the compositor inherits the logind
// session that owns DRM master on that VT - which is exactly what
// the live VM was missing when it failed with "Failed to activate
// login1 session" + "No suitable DRM devices have been found".
const realVTUnitTemplate = `[Unit]
Description={{ .Description }}
After=systemd-logind.service systemd-user-sessions.service network-online.target
Wants=systemd-user-sessions.service
Conflicts=display-manager.service getty@tty{{ .KwinVT }}.service

[Service]
Type=simple
User={{ .User }}
Group={{ .User }}
SupplementaryGroups=video render input
PAMName=login
PermissionsStartOnly=true

# Anchor the session to a real VT. This is the v2 ingredient that
# makes logind hand DRM master to the compositor.
TTYPath=/dev/tty{{ .KwinVT }}
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=yes
TTYVTDisallocate=yes
UtmpIdentifier=tty{{ .KwinVT }}
UtmpMode=user
SyslogIdentifier=clouddeploy-{{ .CompositorMode }}

WorkingDirectory={{ .Home }}

# Environment - the lot. Logind sets some of these, but we set them
# all explicitly so a manual 'systemctl start <unit>' from a non-
# logind context (e.g. a serial console) still produces a working
# compositor.
Environment=HOME={{ .Home }}
Environment=USER={{ .User }}
Environment=LOGNAME={{ .User }}
Environment=XDG_RUNTIME_DIR=/run/user/{{ .UID }}
Environment=WAYLAND_DISPLAY=wayland-0
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{{ .UID }}/bus
Environment=XDG_SESSION_TYPE=wayland
Environment=XDG_SESSION_CLASS=user
Environment=XDG_SESSION_DESKTOP=KDE
Environment=XDG_CURRENT_DESKTOP=KDE
Environment=DESKTOP_SESSION=plasmawayland
Environment=KDE_FULL_SESSION=true
Environment=QT_QPA_PLATFORM=wayland
Environment=GDK_BACKEND=wayland,x11
Environment=MOZ_ENABLE_WAYLAND=1
# Steer KWin at the NVIDIA card the edid phase prepared.
Environment=KWIN_DRM_DEVICES={{ .KwinDRMDevice }}
# Live-VM-tested workarounds for NVIDIA on KWin 6.x:
#   no direct scanout on the forced/headless connector
#   force software cursor (NVIDIA cursor planes intermittently 0-by-0)
#   disable HW overlays (the NVIDIA driver's atomic check rejects
#   our overlay configurations on the forced output)
Environment=KWIN_DRM_NO_DIRECT_SCANOUT=1
Environment=KWIN_FORCE_SW_CURSOR=1
Environment=KWIN_USE_OVERLAYS=0
# NVIDIA EGL/GBM bring-up.
Environment=GBM_BACKEND=nvidia-drm
Environment=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
{{ .PrivateHDREnv }}

# Make sure /run/user/<uid> + linger are ready before we launch.
# The leading dash swallows non-fatal failures (the unit doesn't own
# the dirs - logind does - but a defensive mkdir is cheap).
ExecStartPre=-/bin/systemctl stop getty@tty{{ .KwinVT }}.service
ExecStartPre=-/bin/systemctl start user@{{ .UID }}.service
ExecStartPre=/bin/mkdir -p /run/user/{{ .UID }}
ExecStartPre=/bin/chown {{ .User }}:{{ .User }} /run/user/{{ .UID }}
ExecStartPre=/bin/chmod 0700 /run/user/{{ .UID }}
ExecStartPre=/usr/bin/chvt {{ .KwinVT }}
ExecStartPre=/bin/sh -c 'i=0; while [ $i -lt 30 ]; do /usr/bin/nvidia-smi >/dev/null 2>&1 && exit 0; sleep 1; i=$((i+1)); done; echo "warning: nvidia-smi never became ready; proceeding" >&2; exit 0'
ExecStart={{ .ExecStart }}

# Best-effort: force the configured DP-1 mode after KWin is up. The
# helper is installed by the kwin_session phase. A missing helper is
# logged but not fatal.
ExecStartPost=-/usr/local/bin/clouddeploy-force-kwin-mode.sh

Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// userBackendUnitTemplate is the Milestone 4A simple user-session
// unit. Kept for backwards compatibility but no longer the default;
// real-VT is the new default because user-session compositors hit
// "Failed to activate login1 session" on headless NVIDIA.
const userBackendUnitTemplate = `[Unit]
Description={{ .Description }}
After=systemd-user-sessions.service network-online.target
Wants=systemd-user-sessions.service

[Service]
Type=simple
User={{ .User }}
PAMName=login
PermissionsStartOnly=true

Environment=XDG_RUNTIME_DIR=/run/user/{{ .UID }}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{{ .UID }}/bus
Environment=WAYLAND_DISPLAY=wayland-0
Environment=QT_QPA_PLATFORM=wayland
Environment=KWIN_DRM_USE_EGL_STREAMS=0
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
Environment=GBM_BACKEND=nvidia-drm
Environment=MOZ_ENABLE_WAYLAND=1
{{ .PrivateHDREnv }}

ExecStartPre=/bin/mkdir -p /run/user/{{ .UID }}
ExecStartPre=/bin/chown {{ .User }}:{{ .User }} /run/user/{{ .UID }}

ExecStart={{ .ExecStart }}

Restart=on-failure
RestartSec=5

StandardOutput=journal
StandardError=journal
SyslogIdentifier=clouddeploy-kwin

[Install]
WantedBy=multi-user.target
`

// westonUnitTemplate is the diagnostic fallback so an operator can
// prove NVIDIA DRM/KMS works independently of KWin/logind.
const westonUnitTemplate = `[Unit]
Description={{ .Description }}
After=systemd-logind.service systemd-user-sessions.service network-online.target
Wants=systemd-user-sessions.service
Conflicts=display-manager.service getty@tty{{ .KwinVT }}.service

[Service]
Type=simple
User={{ .User }}
SupplementaryGroups=video render input
PAMName=login
PermissionsStartOnly=true

TTYPath=/dev/tty{{ .KwinVT }}
StandardInput=tty
StandardOutput=journal
StandardError=journal
TTYReset=yes
TTYVHangup=yes
TTYVTDisallocate=yes
UtmpIdentifier=tty{{ .KwinVT }}
UtmpMode=user
SyslogIdentifier=clouddeploy-weston

Environment=HOME={{ .Home }}
Environment=USER={{ .User }}
Environment=XDG_RUNTIME_DIR=/run/user/{{ .UID }}
Environment=WAYLAND_DISPLAY=wayland-0
Environment=XDG_SESSION_TYPE=wayland

ExecStartPre=-/bin/systemctl stop getty@tty{{ .KwinVT }}.service
ExecStartPre=/bin/mkdir -p /run/user/{{ .UID }}
ExecStartPre=/bin/chown {{ .User }}:{{ .User }} /run/user/{{ .UID }}
ExecStartPre=/usr/bin/chvt {{ .KwinVT }}
ExecStart={{ .ExecStart }}

Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// forceKwinModeScriptPath is where the phase installs the helper that
// nudges KWin to the configured DP-1 mode after the unit starts.
const forceKwinModeScriptPath = "/usr/local/bin/clouddeploy-force-kwin-mode.sh"

// forceKwinModeScriptBody is the helper. It loops kscreen-doctor for
// up to 60 seconds trying to set the configured mode. v2-parity.
const forceKwinModeScriptBody = `#!/usr/bin/env bash
# Installed by clouddeployctl phase kwin-session.
# Best-effort: force {{ .Connector }} to {{ .Resolution }}@{{ .Refresh }}
# after KWin is up. Failure is logged via systemd journal; the
# kwin_session phase has its own kscreen-doctor probe later.
set -u
exec 9>/run/clouddeploy-force-kwin-mode.lock
flock -n 9 || exit 0
USER_NAME="${CLOUDDEPLOY_KWIN_USER:-{{ .User }}}"
UID_NUM="${CLOUDDEPLOY_KWIN_UID:-{{ .UID }}}"
CONNECTOR="${CLOUDDEPLOY_KWIN_CONNECTOR:-{{ .Connector }}}"
MODE="${CLOUDDEPLOY_KWIN_MODE:-{{ .Resolution }}@{{ .Refresh }}}"
end=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "${end}" ]; do
    if timeout 20s runuser -u "${USER_NAME}" -- env \
        XDG_RUNTIME_DIR="/run/user/${UID_NUM}" \
        WAYLAND_DISPLAY=wayland-0 \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${UID_NUM}/bus" \
        QT_QPA_PLATFORM=wayland \
        XDG_CURRENT_DESKTOP=KDE \
        XDG_SESSION_TYPE=wayland \
        kscreen-doctor "output.${CONNECTOR}.mode.${MODE}" "output.${CONNECTOR}.scale.1" >/dev/null 2>&1; then
        echo "clouddeploy-force-kwin-mode: ${CONNECTOR} -> ${MODE}, scale=1"
        exit 0
    fi
    sleep 2
done
echo "clouddeploy-force-kwin-mode: gave up after 60s (mode may be unavailable)" >&2
exit 0
`

// KWinSession is the Milestone 4B phase. It writes the right systemd
// unit for the configured session_backend / compositor_mode, runs
// `systemctl daemon-reload + enable + start`, then verifies the
// compositor actually came up and STAYED up:
//
//  1. systemctl is-active <unit> == "active"
//  2. The wayland socket exists AND survives a stability window
//     (default 10s) - the v3 4A regression marked the phase done
//     when the socket appeared once and then died in the
//     compositor's restart loop.
//  3. The KWin / Plasma / Weston process is alive in the systemd
//     unit's cgroup (MainPID).
//  4. The unit's recent journal does NOT contain any of the
//     well-known fatal signatures ("No suitable DRM devices have
//     been found", "Failed to activate /org/freedesktop/login1
//     session", "failed to open drm device", "status=1/FAILURE").
type KWinSession struct {
	SysfsRoot  string
	DevDriRoot string
	// UnitPath overrides the default install path for tests.
	UnitPath string

	// SystemctlFn lets tests stub out `systemctl <verb> [...]`.
	// nil = runner.
	SystemctlFn func(ctx context.Context, deps *Deps, args ...string) error

	// SystemctlIsActiveFn returns ("active", true), ("activating", true)
	// etc. nil = runner.
	SystemctlIsActiveFn func(ctx context.Context, deps *Deps, unit string) (string, error)

	// SystemctlShowMainPIDFn returns the MainPID for the unit. 0 means
	// "no process". nil = runner.
	SystemctlShowMainPIDFn func(ctx context.Context, deps *Deps, unit string) (int, error)

	// JournalRecentFn returns the last N lines of the unit's journal.
	// Used to scan for fatal signatures. nil = runner.
	JournalRecentFn func(ctx context.Context, deps *Deps, unit string, lines int) (string, error)

	// WaylandSocketFn lets tests stub the socket-exists probe. nil =
	// real os.Stat. Called multiple times during the stability window.
	WaylandSocketFn func(uid string) bool

	// SocketWait caps how long the phase waits for wayland-0 to
	// first appear. 0 = 30s default.
	SocketWait time.Duration

	// SocketStability is how long the socket must persist after
	// first appearance. 0 = 10s default. v2 regression: a transient
	// socket from a compositor restart loop should NOT pass.
	SocketStability time.Duration

	// HelperPath overrides /usr/local/bin/clouddeploy-force-kwin-mode.sh
	// for tests.
	HelperPath string
}

// Name implements Phase.
func (KWinSession) Name() string { return KWinSessionName }

// fatalJournalSignatures is the set of "this compositor is dead"
// signals the live VM postmortem identified.
var fatalJournalSignatures = []string{
	"No suitable DRM devices have been found",
	"failed to open drm device",
	"Failed to activate /org/freedesktop/login1/session",
	"status=1/FAILURE",
}

// templateInputs is the bag of substitutions the unit templates take.
type templateInputs struct {
	User           string
	Home           string
	UID            string
	KwinVT         int
	Description    string
	CompositorMode string
	KwinDRMDevice  string
	ExecStart      string
	PrivateHDREnv  string
	Connector      string
	Resolution     string
	Refresh        int
}

func (in templateInputs) apply(tpl string) string {
	out := tpl
	out = strings.ReplaceAll(out, "{{ .User }}", in.User)
	out = strings.ReplaceAll(out, "{{ .Home }}", in.Home)
	out = strings.ReplaceAll(out, "{{ .UID }}", in.UID)
	out = strings.ReplaceAll(out, "{{ .KwinVT }}", fmt.Sprintf("%d", in.KwinVT))
	out = strings.ReplaceAll(out, "{{ .Description }}", in.Description)
	out = strings.ReplaceAll(out, "{{ .CompositorMode }}", in.CompositorMode)
	out = strings.ReplaceAll(out, "{{ .KwinDRMDevice }}", in.KwinDRMDevice)
	out = strings.ReplaceAll(out, "{{ .ExecStart }}", in.ExecStart)
	out = strings.ReplaceAll(out, "{{ .PrivateHDREnv }}", in.PrivateHDREnv)
	out = strings.ReplaceAll(out, "{{ .Connector }}", in.Connector)
	out = strings.ReplaceAll(out, "{{ .Resolution }}", in.Resolution)
	out = strings.ReplaceAll(out, "{{ .Refresh }}", fmt.Sprintf("%d", in.Refresh))
	return out
}

// chooseUnit picks the (template, choice) pair for the resolved
// backend/compositor pair. Falls back to the user/kwin pair if the
// configured combination isn't registered (defensive; ValidateProfile
// already rejects bad values).
func chooseUnit(backend, compositorMode string) (string, kwinUnitChoice) {
	tbl, ok := kwinUnitChoices[backend]
	if !ok {
		tbl = kwinUnitChoices["user"]
	}
	choice, ok := tbl[compositorMode]
	if !ok {
		for _, c := range tbl {
			choice = c
			break
		}
	}
	switch {
	case choice.IsWeston:
		return westonUnitTemplate, choice
	case choice.IsRealVT:
		return realVTUnitTemplate, choice
	}
	return userBackendUnitTemplate, choice
}

// UnitNameForSession returns the systemd service name the
// kwin_session phase will use for a backend/compositor pair.
func UnitNameForSession(backend, compositorMode string) string {
	_, choice := chooseUnit(backend, compositorMode)
	return choice.UnitName
}

// execStartFor returns the ExecStart command for a given choice.
func execStartFor(choice kwinUnitChoice, runtimeBin string) string {
	if strings.TrimSpace(runtimeBin) == "" {
		runtimeBin = "/usr/bin/kwin_wayland"
	}
	switch {
	case choice.IsWeston:
		return "/usr/bin/weston --backend=drm-backend.so"
	case strings.Contains(choice.UnitName, "plasma"):
		// plasma-realvt + clouddeploy-plasma-wayland both run the
		// full Plasma session, which brings up kwin_wayland for us.
		if choice.IsRealVT {
			return "/usr/bin/startplasma-wayland"
		}
		return "/usr/bin/dbus-run-session -- /usr/bin/startplasma-wayland"
	}
	// kwin-only paths.
	if choice.IsRealVT {
		return runtimeBin + " --drm --no-lockscreen"
	}
	return "/usr/bin/dbus-run-session -- " + runtimeBin + " --drm --no-lockscreen"
}

// RenderUnitText is exported so tests + doctor kwin can show the
// rendered unit body without touching disk. Uses the default backend
// (realvt/plasma) when nothing is specified.
func RenderUnitText(user, uid string) string {
	in := templateInputs{
		User:           user,
		Home:           "/home/" + user,
		UID:            uid,
		KwinVT:         7,
		Description:    "CloudDeploy Plasma Wayland session (real-VT)",
		CompositorMode: "plasma",
		KwinDRMDevice:  "/dev/dri/card1",
		ExecStart:      "/usr/bin/startplasma-wayland",
	}
	return in.apply(realVTUnitTemplate)
}

// Run implements Phase.
func (p KWinSession) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if shouldSkip(deps.State, KWinSessionName) {
		log.Info("phase kwin-session: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(KWinSessionName)
	_ = deps.PersistState()

	desk := deps.Profile.EffectiveDesktop()
	// Resolve the DRM card BEFORE rendering the template. The
	// profile's defaulted /dev/dri/card1 must not bypass the resolver
	// just because EffectiveDesktop filled it in. We thread the raw
	// `deps.Profile.Desktop.KwinDRMDevice` (pre-defaulting) through
	// as an explicitness signal so the resolver can tell "operator
	// explicitly pinned this" apart from "defaulted to card1".
	drmRequested := desk.KwinDRMDevice
	drmExplicit := false
	if deps.Profile != nil {
		drmExplicit = strings.TrimSpace(deps.Profile.Desktop.KwinDRMDevice) != ""
	}
	resolvedDRM, drmReason := p.resolveDRMDevice(drmRequested, drmExplicit, strings.TrimSpace(deps.Profile.Display.ForcedConnector))
	details := map[string]any{
		"user":                      desk.User,
		"backend":                   desk.SessionBackend,
		"compositor_mode":           desk.CompositorMode,
		"vt":                        desk.KwinVT,
		"kwin_drm_device_requested": drmRequested,
		"kwin_drm_device_explicit":  drmExplicit,
		"kwin_drm_device_resolved":  resolvedDRM,
		"kwin_drm_device_reason":    drmReason,
		// `selected_drm_device` mirrors the resolved value so older
		// dashboards / docs that read this key keep working.
		"selected_drm_device": resolvedDRM,
	}

	uid := uidFromState(deps)
	if uid == "" {
		err := fmt.Errorf("kwin-session needs uid recorded by phase headless-user; nothing found in state.Details[\"uid\"]")
		details["err"] = err.Error()
		deps.State.MarkFailed(KWinSessionName, "missing uid in state", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}
	details["uid"] = uid

	connector := ""
	resolution := ""
	refresh := 0
	if deps.Profile != nil {
		connector = strings.TrimSpace(deps.Profile.Display.ForcedConnector)
		resolution = strings.TrimSpace(deps.Profile.Display.Resolution)
		refresh = deps.Profile.Display.Refresh
	}

	tplBody, choice := chooseUnit(desk.SessionBackend, desk.CompositorMode)
	details["service_name"] = choice.UnitName
	details["binary"] = choice.BinaryDesc
	kwinCfg := deps.Profile.EffectiveKWin()
	runtimeBin := strings.TrimSpace(kwinCfg.RuntimeBin)
	kwinPatchDone := false
	if ph := deps.State.Phases[KWinPatchName]; ph != nil && ph.Status == statepkg.StatusDone {
		kwinPatchDone = true
		if runtimeBin == "" {
			if marker, err := kwinpkg.ReadMarker(kwinpkg.DefaultMarkerPath); err == nil {
				runtimeBin = strings.TrimSpace(marker.RuntimeBin)
			}
		}
	}
	privateHDREnv := ""
	if kwinCfg.PatchedHDR && kwinPatchDone {
		privateHDREnv = strings.Join([]string{
			"Environment=KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1",
			"Environment=KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR_MODESET_PLANE_PROPS=1",
		}, "\n")
	}
	details["patched_hdr_requested"] = kwinCfg.PatchedHDR
	details["kwin_patch_done"] = kwinPatchDone
	details["patched_hdr_env"] = privateHDREnv != ""
	if runtimeBin != "" {
		details["runtime_bin"] = runtimeBin
	}

	in := templateInputs{
		User:           desk.User,
		Home:           "/home/" + desk.User,
		UID:            uid,
		KwinVT:         desk.KwinVT,
		Description:    choice.BannerName,
		CompositorMode: desk.CompositorMode,
		// Rendered unit ALWAYS uses the resolved DRM device, not the
		// raw profile field. Live VM regression: a host where
		// /dev/dri/card1 didn't expose DP-1 got the default card1
		// path baked into the unit and KWin then died with
		// "No suitable DRM devices have been found". The resolver
		// picks the card whose DP-1 sysfs node actually exists.
		KwinDRMDevice: resolvedDRM,
		ExecStart:     execStartFor(choice, runtimeBin),
		PrivateHDREnv: privateHDREnv,
		Connector:     connector,
		Resolution:    resolution,
		Refresh:       refresh,
	}
	unitBody := in.apply(tplBody)

	// 1. Write the unit (and the force-kwin-mode helper for real-VT).
	unitPath := p.unitPath(choice.UnitName)
	details["unit_path"] = unitPath
	details["unit_bytes"] = len(unitBody)
	if !deps.DryRun {
		if err := p.writeUnit(unitPath, unitBody); err != nil {
			details["err"] = err.Error()
			deps.State.MarkFailed(KWinSessionName, "write systemd unit", err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: %w", err)
		}
		// Install the helper only when the real-VT unit references it.
		if choice.IsRealVT && connector != "" && resolution != "" && refresh > 0 {
			if err := p.writeHelper(in); err != nil {
				log.Warn("phase kwin-session: failed to install force-kwin-mode helper (non-fatal)",
					"err", err)
				details["force_kwin_helper_warning"] = err.Error()
			} else {
				details["force_kwin_helper"] = p.helperPath()
			}
		}
	}

	// 2. daemon-reload + enable + start.
	steps := []struct {
		name string
		args []string
	}{
		{"daemon-reload", []string{"daemon-reload"}},
		{"enable", []string{"enable", choice.UnitName}},
		{"start", []string{"start", choice.UnitName}},
	}
	for _, s := range steps {
		if err := p.systemctl(ctx, deps, s.args...); err != nil {
			details["err"] = err.Error()
			details["failed_step"] = s.name
			deps.State.MarkFailed(KWinSessionName, "systemctl "+s.name, err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: systemctl %s: %w", s.name, err)
		}
	}
	details["systemctl_enabled"] = true
	details["systemctl_started"] = true

	// 3. First socket appearance.
	wait := p.SocketWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	stability := p.SocketStability
	if stability == 0 {
		stability = 10 * time.Second
	}
	socketPath := waylandSocketPath(uid)
	details["wayland_socket_path"] = socketPath

	if deps.DryRun {
		details["wayland_socket_ok"] = true
		details["service_active"] = true
		details["socket_stable_seconds"] = int(stability / time.Second)
		details["dry_run"] = true
		deps.State.MarkDone(KWinSessionName, details)
		_ = deps.PersistState()
		log.Info("phase kwin-session: done (dry-run)",
			"unit", choice.UnitName, "socket", socketPath)
		return nil
	}

	if ok := p.waitForSocket(ctx, uid, wait); !ok {
		err := fmt.Errorf("wayland socket %s did not appear within %s after `systemctl start %s`", socketPath, wait, choice.UnitName)
		details["err"] = err.Error()
		details["wayland_socket_ok"] = false
		deps.State.MarkFailed(KWinSessionName, "wayland socket missing", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}

	// 4. Stability window: the socket has to STAY there. Live VM
	// regression: KWin briefly created the socket and then died in
	// a restart loop because the user-session backend couldn't open
	// /dev/dri/card*. We poll once per second; if the socket
	// disappears OR is-active stops reporting "active", we fail.
	deadline := time.Now().Add(stability)
	stableStart := time.Now()
	for time.Now().Before(deadline) {
		if !p.socketExists(uid) {
			err := fmt.Errorf("wayland socket %s disappeared during the %s stability window (compositor restart loop?)",
				socketPath, stability)
			details["err"] = err.Error()
			details["wayland_socket_ok"] = false
			details["socket_stable_seconds"] = int(time.Since(stableStart) / time.Second)
			deps.State.MarkFailed(KWinSessionName, "wayland socket transient", err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	details["wayland_socket_ok"] = true
	details["socket_stable_seconds"] = int(stability / time.Second)

	// 5. systemctl is-active must say "active".
	state, err := p.isActive(ctx, deps, choice.UnitName)
	details["service_active_state"] = state
	if err != nil {
		details["service_active"] = false
		details["err"] = err.Error()
		deps.State.MarkFailed(KWinSessionName, "systemctl is-active failed", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}
	if state != "active" {
		err := fmt.Errorf("`systemctl is-active %s` returned %q (want active); the compositor is in a restart loop", choice.UnitName, state)
		details["service_active"] = false
		details["err"] = err.Error()
		deps.State.MarkFailed(KWinSessionName, "service not active after stability window", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}
	details["service_active"] = true

	// 6. MainPID must be a real, alive process.
	pid, perr := p.mainPID(ctx, deps, choice.UnitName)
	details["kwin_pid"] = pid
	if perr != nil {
		log.Warn("phase kwin-session: could not read MainPID (non-fatal)",
			"unit", choice.UnitName, "err", perr)
	}
	if pid <= 0 {
		err := fmt.Errorf("systemd reports MainPID=%d for %s after the stability window", pid, choice.UnitName)
		details["err"] = err.Error()
		deps.State.MarkFailed(KWinSessionName, "compositor process missing after start", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}

	// 7. Scan the recent journal for fatal signatures. Even if the
	// socket survived + is-active is good, a restart loop in
	// progress might still show "Failed to activate login1 session"
	// in the last few seconds.
	journalLog, jerr := p.journalRecent(ctx, deps, choice.UnitName, 200)
	if jerr != nil {
		log.Warn("phase kwin-session: could not read journal (non-fatal)",
			"unit", choice.UnitName, "err", jerr)
	}
	if hits := scanFatalSignatures(journalLog); len(hits) > 0 {
		err := fmt.Errorf("recent journal for %s contains fatal signature(s): %v",
			choice.UnitName, hits)
		details["err"] = err.Error()
		details["journal_fatal_hits"] = hits
		deps.State.MarkFailed(KWinSessionName, "fatal journal signature after start", err, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", err)
	}

	deps.State.MarkDone(KWinSessionName, details)
	_ = deps.PersistState()
	log.Info("phase kwin-session: done",
		"user", desk.User, "uid", uid, "unit", choice.UnitName,
		"backend", desk.SessionBackend, "compositor_mode", desk.CompositorMode,
		"pid", pid, "socket", socketPath)
	return nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func (p KWinSession) unitPath(unitName string) string {
	if p.UnitPath != "" {
		return p.UnitPath
	}
	return filepath.Join("/etc/systemd/system", unitName)
}

func (p KWinSession) helperPath() string {
	if p.HelperPath != "" {
		return p.HelperPath
	}
	return forceKwinModeScriptPath
}

func (p KWinSession) writeUnit(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("kwin-session: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("kwin-session: write %s: %w", path, err)
	}
	return nil
}

func (p KWinSession) writeHelper(in templateInputs) error {
	path := p.helperPath()
	body := in.apply(forceKwinModeScriptBody)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("kwin-session: mkdir helper dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return fmt.Errorf("kwin-session: write helper %s: %w", path, err)
	}
	return nil
}

func (p KWinSession) systemctl(ctx context.Context, deps *Deps, args ...string) error {
	if p.SystemctlFn != nil {
		return p.SystemctlFn(ctx, deps, args...)
	}
	if deps.DryRun {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    append([]string{"systemctl"}, args...),
		Sudo:    true,
		Timeout: 30 * time.Second,
	})
	if res.Err != nil {
		return fmt.Errorf("systemctl %s: %w (stderr=%q)",
			strings.Join(args, " "), res.Err, lastLines(res.Stderr, 3))
	}
	return nil
}

func (p KWinSession) isActive(ctx context.Context, deps *Deps, unit string) (string, error) {
	if p.SystemctlIsActiveFn != nil {
		return p.SystemctlIsActiveFn(ctx, deps, unit)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"systemctl", "is-active", unit},
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	state := strings.TrimSpace(res.Stdout)
	// systemctl is-active returns 0 only for "active". Anything else
	// returns non-zero with the state on stdout - that's expected.
	if state == "" && res.Err != nil {
		return "", fmt.Errorf("systemctl is-active %s: %w", unit, res.Err)
	}
	return state, nil
}

func (p KWinSession) mainPID(ctx context.Context, deps *Deps, unit string) (int, error) {
	if p.SystemctlShowMainPIDFn != nil {
		return p.SystemctlShowMainPIDFn(ctx, deps, unit)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"systemctl", "show", unit, "-p", "MainPID", "--value"},
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	if res.Err != nil {
		return 0, fmt.Errorf("systemctl show MainPID: %w", res.Err)
	}
	pid := 0
	for _, c := range strings.TrimSpace(res.Stdout) {
		if c < '0' || c > '9' {
			pid = 0
			break
		}
		pid = pid*10 + int(c-'0')
	}
	return pid, nil
}

func (p KWinSession) journalRecent(ctx context.Context, deps *Deps, unit string, lines int) (string, error) {
	if p.JournalRecentFn != nil {
		return p.JournalRecentFn(ctx, deps, unit, lines)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"journalctl", "-u", unit, "-n", fmt.Sprintf("%d", lines), "--no-pager"},
		LogFile: "-",
		Timeout: 15 * time.Second,
	})
	if res.Err != nil {
		return res.Stdout, fmt.Errorf("journalctl: %w", res.Err)
	}
	return res.Stdout, nil
}

func (p KWinSession) waitForSocket(ctx context.Context, uid string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if p.socketExists(uid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (p KWinSession) socketExists(uid string) bool {
	if p.WaylandSocketFn != nil {
		return p.WaylandSocketFn(uid)
	}
	_, err := os.Stat(waylandSocketPath(uid))
	return err == nil
}

// waylandSocketPath is the canonical path the compositor publishes
// the WAYLAND_DISPLAY=wayland-0 socket at.
func waylandSocketPath(uid string) string {
	return "/run/user/" + uid + "/wayland-0"
}

// uidFromState reads state.Details["uid"] off the headless_user
// phase. Returns "" when missing or non-string.
func uidFromState(deps *Deps) string {
	hu := deps.State.Get(HeadlessUserName)
	if hu == nil {
		return ""
	}
	v, ok := hu.Details["uid"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// scanFatalSignatures returns any FatalJournalSignatures substrings
// found in `journalText`. Empty slice means "clean".
func scanFatalSignatures(journalText string) []string {
	if journalText == "" {
		return nil
	}
	var hits []string
	for _, sig := range fatalJournalSignatures {
		if strings.Contains(journalText, sig) {
			hits = append(hits, sig)
		}
	}
	return hits
}

// resolveDRMDevice picks the /dev/dri/card* node the compositor
// should bind to.
//
// Inputs:
//
//   - requested:         what the profile (after EffectiveDesktop's
//     defaulting) would render into the unit.
//   - requestedExplicit: true only when the operator's YAML had a
//     non-empty `desktop.kwin_drm_device` (the
//     Run method threads the raw profile field
//     through to distinguish "explicit card1"
//     from "defaulted card1").
//   - forcedConnector:   profile.display.forced_connector (e.g. DP-1).
//
// Resolution order:
//
//  1. If the operator explicitly pinned a path, honor it verbatim.
//     The whole point of `desktop.kwin_drm_device:` is operator
//     override; the resolver MUST NOT second-guess it.
//  2. Otherwise, walk /sys/class/drm/card*: prefer the card whose
//     forced_connector connector node exists.
//  3. Otherwise, prefer the card whose device/vendor is NVIDIA
//     (0x10de) - this is the v2-parity bias on hosts that expose a
//     second non-NVIDIA card (e.g. NVIDIA + onboard AST).
//  4. Otherwise, /dev/dri/card1 if it exists, else /dev/dri/card0.
//
// Returns (path, reason) so the phase can log + record both. The
// reason is human-readable and shows up in state.Details under
// kwin_drm_device_reason for `state show` / `doctor kwin`.
func (p KWinSession) resolveDRMDevice(requested string, requestedExplicit bool, forcedConnector string) (string, string) {
	if requestedExplicit && strings.TrimSpace(requested) != "" {
		return requested, "explicitly requested via profile.desktop.kwin_drm_device"
	}

	sysfs := p.SysfsRoot
	if sysfs == "" {
		sysfs = "/sys"
	}
	devdri := p.DevDriRoot
	if devdri == "" {
		devdri = "/dev/dri"
	}

	cards, _ := filepath.Glob(filepath.Join(sysfs, "class/drm/card*"))

	if forcedConnector != "" {
		for _, cardPath := range cards {
			name := filepath.Base(cardPath)
			if strings.Contains(name, "-") {
				continue
			}
			connPath := filepath.Join(cardPath, fmt.Sprintf("%s-%s", name, forcedConnector))
			if _, err := os.Stat(connPath); err == nil {
				return filepath.Join(devdri, name), "matched forced_connector " + forcedConnector
			}
		}
	}

	for _, cardPath := range cards {
		name := filepath.Base(cardPath)
		if strings.Contains(name, "-") {
			continue
		}
		vendorByte, err := os.ReadFile(filepath.Join(cardPath, "device", "vendor"))
		if err == nil && strings.Contains(strings.ToLower(string(vendorByte)), "0x10de") {
			return filepath.Join(devdri, name), "nvidia gpu detected"
		}
	}

	if _, err := os.Stat(filepath.Join(devdri, "card1")); err == nil {
		return filepath.Join(devdri, "card1"), "fallback card1 present"
	}
	return filepath.Join(devdri, "card0"), "fallback final"
}
