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
// session that owns DRM master on that VT.
//
// IMPORTANT: this template intentionally contains NO ExecStartPre and
// NO ExecStartPost lines. Live VM (2026-05-22) RTX 4090 evidence
// proved every shape of pre/post hook wedges the unit:
//
//   - ExecStartPre=/bin/systemctl stop getty@ttyN.service       hangs
//     systemd inside start-pre because systemctl-from-systemctl is a
//     re-entrant transaction.
//   - ExecStartPre=/bin/systemctl --no-block start user@UID.service
//     still wedges once systemd is in a degraded transaction state.
//   - ExecStartPre=/bin/mkdir / /bin/chown / /usr/bin/chvt also gets
//     stuck the moment the unit is in any "start-pre" hold.
//   - ExecStartPost=/usr/local/bin/clouddeploy-force-kwin-mode.sh
//     hangs the unit in start-post the same way; the helper polls
//     kscreen-doctor for up to 60s and systemd treats that as "not
//     done starting".
//
// All of that work is now done in Go from KWinSession.Run with
// bounded context timeouts BEFORE the systemctl start call. After
// validation, the mode helper is invoked separately from Go (also
// bounded) so it can never block service activation.
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
# Live-VM regression (2026-05-21): if WAYLAND_DISPLAY is set in the
# environment that LAUNCHES kwin_wayland, the kwin_wayland_wrapper
# logs "No backend specified, automatically choosing Wayland because
# WAYLAND_DISPLAY is set" and picks the NESTED Wayland backend
# instead of the --drm KMS backend, even when --drm is explicitly
# passed. The compositor then fails to open any DRM device.
# Validators (kscreen-doctor, qdbus) set WAYLAND_DISPLAY themselves
# after wayland-0 has appeared; we deliberately do NOT set it here.
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{{ .UID }}/bus
Environment=DISPLAY=:0
Environment=XDG_SESSION_TYPE=wayland
Environment=XDG_SESSION_CLASS=user
Environment=XDG_SESSION_DESKTOP=KDE
Environment=XDG_CURRENT_DESKTOP=KDE
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
# NVIDIA EGL/GBM bring-up. The EGL_PLATFORM=gbm +
# __EGL_EXTERNAL_PLATFORM_CONFIG_DIRS pair is what makes libEGL pick
# up the NVIDIA GBM external platform shim (15_nvidia_gbm.json)
# instead of falling back to mesa/llvmpipe when nvidia-egl-gbm is
# present but unmapped. Without it the live VM logs
# "couldn't open egl display" even with libEGL_nvidia.so.0 loadable.
Environment=GBM_BACKEND=nvidia-drm
Environment=EGL_PLATFORM=gbm
Environment=__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json
Environment=__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS=/usr/share/egl/egl_external_platform.d
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
{{ .PrivateHDREnv }}

ExecStart={{ .ExecStart }}

# Restart=no by design (live VM RTX 4090, 2026-05-22). The previous
# Restart=on-failure masked the underlying logind/DRM error behind a
# restart loop, which re-created /run/user/<uid>/wayland-0 transiently
# and produced false-positive "socket present" signals. With
# Restart=no, an actual KWin crash surfaces as "is-active" reporting
# "failed" and clouddeployctl monitor highlights it immediately.
# Operators who want a restart policy can override via:
#     systemctl edit kwin-realvt.service
Restart=no
TimeoutStartSec=30
KillMode=control-group

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

// forceKwinModeScriptBody is the v2-parity helper. After KWin is up
// it pushes the configured mode AND - for HDR profiles - enables
// hdr/wcg on the connector via kscreen-doctor. On failure it dumps
// rich diagnostics (org.kde.KWin presence, supportInformation
// status, kscreen-doctor exit, DP-1 mode list, nested-Wayland-poison
// signature) so the operator can tell WHY the mode push didn't take.
//
// Live VM (2026-05-21): manual `kscreen-doctor "output.1.mode.2"
// "output.1.hdr.enable" "output.1.wcg.enable"` produced
// "HDR: enabled, Wide Color Gamut: enabled, 3840x2160@120 selected"
// once KWin was talking on DBus. This script automates exactly that.
const forceKwinModeScriptBody = `#!/usr/bin/env bash
# Installed by clouddeployctl phase kwin-session.
# Force {{ .Connector }} to {{ .Resolution }}@{{ .Refresh }} (and HDR/WCG
# when the profile has HDR on) once KWin is up. Failure is logged via
# systemd journal; the kwin_session phase has its own validators.
set -u
exec 9>/run/clouddeploy-force-kwin-mode.lock
flock -n 9 || exit 0

USER_NAME="${CLOUDDEPLOY_KWIN_USER:-{{ .User }}}"
UID_NUM="${CLOUDDEPLOY_KWIN_UID:-{{ .UID }}}"
CONNECTOR="${CLOUDDEPLOY_KWIN_CONNECTOR:-{{ .Connector }}}"
MODE="${CLOUDDEPLOY_KWIN_MODE:-{{ .Resolution }}@{{ .Refresh }}}"
HDR="${CLOUDDEPLOY_KWIN_HDR:-{{ .HDR }}}"
KSCREEN="${CLOUDDEPLOY_KSCREEN_DOCTOR:-$(command -v kscreen-doctor || true)}"
QDBUS="${CLOUDDEPLOY_QDBUS:-$(command -v qdbus6 || command -v qdbus || command -v qdbus-qt5 || true)}"

as_user() {
    timeout 20s runuser -u "${USER_NAME}" -- env \
        XDG_RUNTIME_DIR="/run/user/${UID_NUM}" \
        WAYLAND_DISPLAY=wayland-0 \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${UID_NUM}/bus" \
        QT_QPA_PLATFORM=wayland \
        XDG_CURRENT_DESKTOP=KDE \
        XDG_SESSION_TYPE=wayland \
        "$@"
}

log() { echo "clouddeploy-force-kwin-mode: $*"; }

if [ -z "${KSCREEN}" ]; then
    log "kscreen-doctor not found in PATH"
    exit 1
fi

end=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "${end}" ]; do
    out="$(as_user "${KSCREEN}" -o 2>&1 || true)"
    output_id=""
    mode_id=""
    target_res="${MODE%@*}"
    target_refresh="${MODE#*@}"
    in_output=0
    while IFS= read -r line; do
        if [[ "${line}" =~ ^Output: ]]; then
            in_output=0
            # Common Plasma 6 shape: "Output: 1 DP-1 ..."
            if echo "${line}" | grep -Eq "^Output:[[:space:]]+[0-9]+[[:space:]]+${CONNECTOR}([[:space:]]|$)"; then
                output_id="$(echo "${line}" | awk '{print $2}')"
                in_output=1
            fi
            continue
        fi
        [ "${in_output}" = "1" ] || continue
        echo "${line}" | grep -q 'Modes:' || continue
        modes_line="${line#*Modes:}"
        # Plasma 6.4 shape: "1:3840x2160@60!  2:3840x2160@120*"
        # Older shape: "1!  3840x2160@120, 2  1920x1080@60"
        tokens="$(echo "${modes_line}" | grep -oE '[0-9]+[:!][[:space:]]*[0-9]+x[0-9]+@[0-9.]+' || true)"
        if [ -z "${tokens}" ]; then
            tokens="$(echo "${modes_line}" | tr ',' '\n' || true)"
        fi
        while IFS= read -r entry; do
            m="$(echo "${entry}" | grep -oE '[0-9]+x[0-9]+@[0-9.]+' | head -n1 || true)"
            id="$(echo "${entry}" | grep -oE '^[[:space:]]*[0-9]+' | tr -d '[:space:]' || true)"
            [ -n "${m}" ] && [ -n "${id}" ] || continue
            if [ "${m}" = "${MODE}" ] || { [ "${target_refresh}" = "120" ] && echo "${m}" | grep -Eq "^${target_res}@119(\.|$)|^${target_res}@120(\.|$)"; }; then
                mode_id="${id}"
                break
            fi
        done <<< "${tokens}"
    done <<< "${out}"

    if [ -n "${output_id}" ] && [ -n "${mode_id}" ]; then
        args=( "output.${output_id}.enable" "output.${output_id}.mode.${mode_id}" "output.${output_id}.scale.1" "output.${output_id}.position.0,0" )
    else
        # Last-resort compatibility with older kscreen-doctor builds
        # that accepted connector/mode strings directly.
        args=( "output.${CONNECTOR}.mode.${MODE}" "output.${CONNECTOR}.scale.1" )
    fi
    if [ "${HDR}" = "true" ] || [ "${HDR}" = "1" ]; then
        if [ -n "${output_id}" ]; then
            args+=( "output.${output_id}.hdr.enable" "output.${output_id}.wcg.enable" )
        else
            args+=( "output.${CONNECTOR}.hdr.enable" "output.${CONNECTOR}.wcg.enable" )
        fi
    fi
    if as_user "${KSCREEN}" "${args[@]}" >/dev/null 2>&1; then
        log "${CONNECTOR} -> ${MODE} scale=1 hdr=${HDR} output_id=${output_id:-unknown} mode_id=${mode_id:-unknown}"
        exit 0
    fi
    sleep 2
done

# Diagnostics: print everything an operator needs to see WHY this
# failed. Prefixed with "diag:" so journalctl -g 'diag:' filters cleanly.
log "gave up after 60s; collecting diagnostics"

# 1. Is org.kde.KWin even there?
if [ -z "${QDBUS}" ]; then
    log "diag: no qdbus/qdbus6 command available"
elif as_user "${QDBUS}" --session org.kde.KWin /KWin org.freedesktop.DBus.Introspectable.Introspect >/dev/null 2>&1; then
    log "diag: org.kde.KWin DBus introspect OK"
else
    log "diag: org.kde.KWin MISSING on session bus - kwin_session readiness was incomplete"
fi

# 2. supportInformation: backend kind?
support=""
if [ -n "${QDBUS}" ]; then
    support="$(as_user "${QDBUS}" --session org.kde.KWin /KWin org.kde.KWin.supportInformation 2>&1 || true)"
fi
if echo "${support}" | grep -q 'Output backend: DRM'; then
    log "diag: KWin supportInformation reports Output backend: DRM"
elif echo "${support}" | grep -A3 '^Output backend$' | grep -q 'Name: DRM'; then
    log "diag: KWin supportInformation reports Output backend / Name: DRM"
elif echo "${support}" | grep -q 'Output backend:'; then
    log "diag: KWin supportInformation reports NON-DRM backend: $(echo "${support}" | grep 'Output backend:' | head -n1)"
else
    log "diag: supportInformation did not return an Output backend line (qdbus timeout?)"
fi

# 3. kscreen-doctor -o: does the connector exist? does the mode exist?
out="$(as_user "${KSCREEN}" -o 2>&1 || true)"
if echo "${out}" | grep -q "Output:.*${CONNECTOR}"; then
    log "diag: kscreen-doctor sees ${CONNECTOR}"
    if echo "${out}" | grep -q "${MODE}"; then
        log "diag: kscreen-doctor advertises ${MODE} on ${CONNECTOR}"
    else
        log "diag: kscreen-doctor does NOT advertise ${MODE} on ${CONNECTOR} - mode may be unavailable"
    fi
else
    log "diag: kscreen-doctor does NOT see ${CONNECTOR}"
fi

# 4. Nested-Wayland-poison signature in the kwin unit's journal.
if journalctl -u kwin-realvt.service -u plasma-realvt.service -u clouddeploy-kwin-wayland.service -n 200 --no-pager 2>/dev/null | grep -q 'automatically choosing Wayland because WAYLAND_DISPLAY is set'; then
    log "diag: KWin chose NESTED Wayland backend (WAYLAND_DISPLAY was set pre-launch) - private HDR will not work"
fi

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

	// SystemctlShowSubStateFn returns the unit SubState. nil = runner.
	SystemctlShowSubStateFn func(ctx context.Context, deps *Deps, unit string) string

	// JournalRecentFn returns the last N lines of the unit's journal.
	// Used to scan for fatal signatures. nil = runner.
	JournalRecentFn func(ctx context.Context, deps *Deps, unit string, lines int) (string, error)

	// KWinDBusFn probes the session bus for org.kde.KWin as the
	// headless user. Returns (true, "") when the service is owned.
	// Live-VM evidence: a "shallow" success run had service=active
	// + wayland-0 present but no DBus name; kscreen-doctor then
	// hung. nil = real `qdbus --session org.kde.KWin /KWin /
	// org.freedesktop.DBus.Introspectable.Introspect` via the
	// runner.
	KWinDBusFn func(ctx context.Context, deps *Deps, user, uid string) (bool, string, error)

	// DBusProbeWait bounds the soft org.kde.KWin diagnostic probe
	// after the hard compositor gates pass. 0 = production default
	// (at least 60s); negative = single best-effort probe.
	DBusProbeWait time.Duration

	// SupportInformationFn runs qdbus org.kde.KWin /KWin
	// supportInformation as the headless user and returns the dump.
	// The phase asserts the output contains "Output backend: DRM"
	// (or a fallback "Compositor backend") before marking done.
	// nil = real runner call.
	SupportInformationFn func(ctx context.Context, deps *Deps, user, uid string) (string, error)

	// WaylandSocketFn lets tests stub the socket-exists probe. nil =
	// real os.Stat. Called multiple times during the stability window.
	WaylandSocketFn func(uid string) bool

	// SocketWait caps how long the phase waits for wayland-0 to
	// first appear. 0 = 360s default.
	SocketWait time.Duration

	// GateProgressExtension extends SocketWait once when KWin is
	// active/running with a stable MainPID and a clean current
	// journal, but PAM/logind or the wayland socket are late. 0 =
	// 120s default for production-sized waits; negative disables.
	GateProgressExtension time.Duration

	// SocketStability is how long the socket must persist after
	// first appearance. 0 = 10s default. v2 regression: a transient
	// socket from a compositor restart loop should NOT pass.
	SocketStability time.Duration

	// HelperPath overrides /usr/local/bin/clouddeploy-force-kwin-mode.sh
	// for tests.
	HelperPath string

	// LoginctlSessionsFn overrides the `loginctl list-sessions
	// --no-legend` call used by the unified gate poll to verify the
	// headless user has an active session on the expected seat+tty.
	// nil = real runner call. Tests inject deterministic output.
	LoginctlSessionsFn func(ctx context.Context, deps *Deps) (string, error)

	// HealthProbeFn overrides the per-tick probe inside
	// awaitKWinHealthy. nil = the production probe that combines
	// systemctl is-active / show MainPID + ActiveEnter / journalForInvocation /
	// loginctlSessionMatchesSeatTTY / socketExists. Tests use this to
	// simulate "socket appears at tick 5".
	HealthProbeFn kwinHealthProbeFn

	// StablePIDWindow overrides how long requireStableMainPID samples
	// the unit's MainPID before declaring the compositor stable.
	// 0 means "30s floor or SocketStability, whichever is larger".
	// Tests inject a short duration so the 30s minimum doesn't
	// dominate the test run.
	StablePIDWindow time.Duration

	// StablePIDInterval overrides how often requireStableMainPID
	// re-samples. 0 = 2s default.
	StablePIDInterval time.Duration

	// LateAdoptionGrace controls the retry window after a gate-poll
	// timeout. This is a final safety net for hosts where PAM/logind
	// publishes the session just after the poll returns. 0 = 120s
	// default for production-sized waits; negative disables.
	LateAdoptionGrace time.Duration

	// SkipAdoption disables the "is an existing kwin-realvt session
	// already healthy?" probe at the top of Run. Tests that don't
	// stub the runner for `systemctl show ... ActiveEnterTimestamp`
	// can set this true to skip the probe entirely.
	SkipAdoption bool

	// SkipExternalPrep disables the in-Go prep step that used to be
	// ExecStartPre. Tests set this true so they don't need to stub
	// every prep command (loginctl/udevadm/chvt/...).
	SkipExternalPrep bool

	// SkipModeHelper disables the post-validation
	// clouddeploy-force-kwin-mode.sh invocation. Tests that don't
	// install the helper script set this true.
	SkipModeHelper bool

	// KillKWinFn overrides the best-effort pkill used only during
	// recovery from an active KWin whose wayland socket was unlinked.
	// nil = production runner call.
	KillKWinFn func(ctx context.Context, deps *Deps, user string) error

	// UnitActiveSecondsFn overrides the systemd ActiveEnter monotonic
	// probe used by the adoption check. nil = runner.
	UnitActiveSecondsFn func(ctx context.Context, deps *Deps, unit string) int

	// healthProbeState carries cross-tick stability counters for the
	// unified gate poll. Initialized lazily inside defaultHealthProbe;
	// unexported so callers don't touch it directly.
	healthProbeState *kwinHealthProbeState
}

// Name implements Phase.
func (KWinSession) Name() string { return KWinSessionName }

// fatalJournalSignatures is the set of "this compositor is dead"
// signals the live VM postmortems identified.
//
// The kwin_wayland_wrapper line is the 2026-05-21 catch: KWin
// silently switched to a nested Wayland backend instead of --drm
// because the unit's environment had WAYLAND_DISPLAY set. The new
// kwin_session.go template no longer sets that env, but if anything
// upstream slips it back in (an operator-edited unit, a future
// regression), this signature short-circuits "everything looks fine"
// before we mark the phase done on a non-KMS compositor.
var fatalJournalSignatures = []string{
	"No suitable DRM devices have been found",
	"failed to open drm device",
	"Failed to activate /org/freedesktop/login1/session",
	"status=1/FAILURE",
	"automatically choosing Wayland because WAYLAND_DISPLAY is set",
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
	// HDR is set when the active profile has display.hdr=true. The
	// force-kwin-mode helper uses this to decide whether to also
	// push `output.<id>.hdr.enable` + `output.<id>.wcg.enable`.
	HDR bool
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
	out = strings.ReplaceAll(out, "{{ .HDR }}", fmt.Sprintf("%v", in.HDR))
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
//
// The direct-kwin real-VT path is what the live VM proved out
// end-to-end (DP-1 3840x2160@120 + private HDR + Wide Color Gamut).
// `--socket wayland-0` pins the socket name so the validators can
// connect deterministically; KWin would default to wayland-0 anyway,
// but pinning makes the intent obvious in `ps` / `systemctl status`.
// The Xwayland flags are load-bearing for Steam, Discord and many
// Windows-game launchers under Proton. Without them, the compositor
// and Sunshine can work while every X11 app exits with "Unable to
// open display".
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
		return runtimeBin + " --drm --xwayland --xwayland-display :0 --socket wayland-0 --no-lockscreen"
	}
	return "/usr/bin/dbus-run-session -- " + runtimeBin + " --drm --xwayland --xwayland-display :0 --socket wayland-0 --no-lockscreen"
}

// RenderUnitText is exported so tests + doctor kwin can show the
// rendered unit body without touching disk. Uses the validated
// Milestone-4 default (realvt + direct kwin_wayland).
func RenderUnitText(user, uid string) string {
	in := templateInputs{
		User:           user,
		Home:           "/home/" + user,
		UID:            uid,
		KwinVT:         7,
		Description:    "CloudDeploy KWin Wayland session (real-VT)",
		CompositorMode: "kwin",
		KwinDRMDevice:  "/dev/dri/card1",
		ExecStart:      "/usr/bin/kwin_wayland --drm --xwayland --xwayland-display :0 --socket wayland-0 --no-lockscreen",
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

	hdr := false
	if deps.Profile != nil {
		hdr = deps.Profile.Display.HDR
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
		HDR:           hdr,
	}

	socketPath := waylandSocketPath(uid)
	selected := KWinSelectedPath{
		User:       desk.User,
		UID:        uid,
		Seat:       "seat0",
		TTY:        fmt.Sprintf("/dev/tty%d", desk.KwinVT),
		DRMCard:    resolvedDRM,
		RenderNode: RenderNodeForCard(resolvedDRM),
		Socket:     socketPath,
		Service:    choice.UnitName,
	}
	recordKWinSelectedDetails(details, selected)

	// 1. Adoption/recovery BEFORE any unit rewrite, helper rewrite,
	// or stale socket cleanup. Live VM regression: removing
	// /run/user/<uid>/wayland-0 while kwin_wayland was already
	// running left KWin alive on DBus with only wayland-0.lock
	// (deleted) open, so new clients could never connect.
	if !deps.DryRun && choice.IsRealVT && !p.SkipAdoption {
		adopt := p.probeKWinAdoption(ctx, deps, choice.UnitName, uid, 30*time.Second)
		recordKWinAdoptionDetails(details, "kwin_adopt", adopt)
		if adopt.Adoptable {
			log.Info("phase kwin-session: adopting existing healthy kwin session; skipping unit regeneration",
				"unit", choice.UnitName, "main_pid", adopt.MainPID,
				"uptime_s", adopt.UptimeSeconds)
			selected.MainPID = adopt.MainPID
			recordKWinSelectedDetails(details, selected)
			details["kwin_pid"] = adopt.MainPID
			details["service_active"] = true
			details["wayland_socket_ok"] = true
			details["adopted_existing_session"] = true
			deps.State.MarkDone(KWinSessionName, details)
			_ = deps.PersistState()
			return nil
		}
		if p.shouldRecoverUnlinkedKWinSocket(ctx, deps, choice.UnitName, desk.User, uid, selected.TTY, adopt, details) {
			details["kwin_failure_category"] = KWinFailSocketUnlinked
			details["kwin_socket_unlinked"] = true
			recovery := p.recoverUnlinkedKWinSocket(ctx, deps, choice.UnitName, desk.User, uid, log)
			details["kwin_socket_unlinked_recovery"] = recovery
			if recovery.Err != "" {
				log.Warn("phase kwin-session: unlinked socket recovery reported warning",
					"unit", choice.UnitName, "err", recovery.Err)
			}
		}
	}

	unitBody := in.apply(tplBody)

	// 2. Write the unit (and the force-kwin-mode helper for real-VT).
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

	// 3. Stale-config cleanup. Live VM evidence (2026-05-21): a
	// rerun of the deploy started KWin, plasmashell, and Sunshine,
	// but wayland-info reported "no monitors available" because the
	// previous boot's KScreen/output config had pinned DP-1 to a
	// mode KWin no longer believed was available. Wiping
	// ~/.local/share/kscreen and the orphan wayland socket(s) was
	// the fix. Idempotent and gated on "first launch OR previous
	// failure" so an operator's hand-tuned kwinrc survives.
	protectWaylandSockets := p.activeMainPID(ctx, deps, choice.UnitName) > 0
	details["stale_wayland_socket_cleanup_protected"] = protectWaylandSockets
	cleaned := cleanStaleKDEConfigIfNeeded(deps, desk.User, uid, protectWaylandSockets)
	if len(cleaned) > 0 {
		details["stale_kde_config_removed"] = cleaned
	}

	// 4. External prep. Every step that used to be ExecStartPre runs
	// here, in Go, with a per-step context timeout. Live VM evidence
	// (2026-05-22): blocking systemctl/chvt/mkdir lines inside the
	// unit wedge it; the same commands run from outside the unit
	// finish in milliseconds.
	if !deps.DryRun && choice.IsRealVT && !p.SkipExternalPrep {
		disc := KWinSessionDiscovery{
			User:            desk.User,
			UID:             uid,
			HomeDir:         "/home/" + desk.User,
			XDGRuntimeDir:   "/run/user/" + uid,
			DRMCard:         resolvedDRM,
			DRMCardReason:   drmReason,
			RenderNode:      RenderNodeForCard(resolvedDRM),
			TTY:             fmt.Sprintf("/dev/tty%d", desk.KwinVT),
			TTYReason:       fmt.Sprintf("profile.desktop.kwin_vt=%d", desk.KwinVT),
			Seat:            "seat0",
			ForcedConnector: connector,
		}
		details["kwin_session_discovery"] = disc
		prep := kwinExternalPrep(ctx, deps, disc, log)
		details["kwin_external_prep"] = prep
	}

	// 5. daemon-reload + enable + start.
	steps := []struct {
		name string
		args []string
	}{
		{"daemon-reload", []string{"daemon-reload"}},
		{"enable", []string{"enable", choice.UnitName}},
		// --no-block returns immediately. The compositor may take 10s+
		// to claim DRM master + bind the wayland socket; the readiness
		// poll below is the real "is it up?" gate. Without --no-block
		// the 30s runner timeout would kill systemctl while it was
		// still waiting for the unit to enter the "active" state.
		{"start", []string{"start", "--no-block", choice.UnitName}},
	}
	for _, s := range steps {
		if err := p.systemctl(ctx, deps, s.args...); err != nil {
			details["err"] = err.Error()
			details["failed_step"] = s.name
			details["kwin_failure_category"] = classifyKWinFailure(err, "", p.showSubState(ctx, deps, choice.UnitName))
			deps.State.MarkFailed(KWinSessionName, "systemctl "+s.name, err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: systemctl %s: %w", s.name, err)
		}
	}
	details["systemctl_enabled"] = true
	details["systemctl_started"] = true

	// 3. Selected runtime tuple. Persisted BEFORE the unified
	// gate poll so a failure leaves the operator a complete record
	// of what we targeted, not just an "is socket present?" boolean.
	wait := p.SocketWait
	if wait == 0 {
		wait = defaultKWinGateWait
	}
	progressExtension := p.GateProgressExtension
	if progressExtension == 0 {
		if wait >= 60*time.Second {
			progressExtension = defaultKWinGateProgressExtension
		}
	} else if progressExtension < 0 {
		progressExtension = 0
	}
	lateAdoptionGrace := p.LateAdoptionGrace
	if lateAdoptionGrace == 0 {
		if wait >= 60*time.Second {
			lateAdoptionGrace = defaultKWinLateAdoptionGrace
		}
	} else if lateAdoptionGrace < 0 {
		lateAdoptionGrace = 0
	}
	stability := p.SocketStability
	if stability == 0 {
		stability = 10 * time.Second
	}
	recordKWinSelectedDetails(details, selected)
	details["kwin_gate_wait_seconds"] = int(wait / time.Second)
	details["kwin_gate_progress_extension_seconds"] = int(progressExtension / time.Second)
	details["kwin_late_adoption_grace_seconds"] = int(lateAdoptionGrace / time.Second)
	details["kwin_gate_stability_seconds"] = int(stability / time.Second)
	deps.State.Get(KWinSessionName).Details = details
	_ = deps.PersistState()

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

	// 4. Unified gate poll. Replaces the old serial sequence
	// (waitForSocket → stability → is-active → MainPID → journal),
	// which fatal'd on socket-wait=30s when the live VM's KWin
	// became healthy at second 35. The unified loop polls every
	// gate together every probeInterval and only declares failure
	// when the WHOLE window elapses without all gates passing.
	probeInterval := p.StablePIDInterval
	if probeInterval == 0 {
		probeInterval = 2 * time.Second
	}
	stabilitySeconds := int(stability / time.Second)
	if stabilitySeconds < 1 {
		stabilitySeconds = 1
	}
	probe := p.HealthProbeFn
	if probe == nil {
		probe = (&p).defaultHealthProbe
	}
	lastGatePersist := time.Time{}
	observeGate := func(s KWinGateSample) {
		recordKWinGateDetails(details, &selected, s)
		// Persist periodically so `clouddeployctl monitor` can show
		// slow-start progress while the phase is still running.
		now := time.Now()
		if lastGatePersist.IsZero() || now.Sub(lastGatePersist) >= 10*time.Second || s.WaitExtended {
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			lastGatePersist = now
		}
	}
	sample, gateErr := awaitKWinHealthy(
		ctx, deps, probe, observeGate,
		choice.UnitName, desk.User, uid, selected.Seat, selected.TTY,
		wait, progressExtension, probeInterval, stabilitySeconds, true,
	)
	recordKWinGateDetails(details, &selected, sample)

	if gateErr != nil {
		// 5. Self-heal: the timed window elapsed without
		// AllGatesPass, but the machine may have become healthy
		// JUST after we gave up. Re-run adoption with min uptime 0;
		// if it now says "adoptable", mark done instead of failing.
		// This is the live VM RTX 4090 2026-05-22 fix: a 30s timeout
		// failed even though the service became fully healthy at
		// second 35.
		log.Warn("phase kwin-session: gate poll timed out; trying late adoption", "err", gateErr, "grace", lateAdoptionGrace)
		var adopt KWinAdoptCheck
		attempts := 0
		lateDeadline := time.Now().Add(lateAdoptionGrace)
		for {
			attempts++
			adopt = p.probeKWinAdoption(ctx, deps, choice.UnitName, uid, 0)
			details["kwin_late_adopt_attempts"] = attempts
			details["kwin_late_adopt_active_state"] = adopt.ActiveState
			details["kwin_late_adopt_sub_state"] = adopt.SubState
			details["kwin_late_adopt_main_pid"] = adopt.MainPID
			details["kwin_late_adopt_socket_present"] = adopt.SocketPresent
			details["kwin_late_adopt_journal_fatals"] = adopt.JournalFatals
			details["kwin_late_adopt_adoptable"] = adopt.Adoptable
			// Late-adoption journal must be invocation-scoped or it
			// inherits the pre-restart fatal markers we already saw.
			if adopt.Adoptable {
				invID := p.readUnitInvocationID(ctx, deps, choice.UnitName)
				activeEnter := p.readUnitActiveEnterTimestamp(ctx, deps, choice.UnitName)
				currJrn, _ := p.journalForInvocation(ctx, deps, choice.UnitName, invID, activeEnter, 200)
				adopt.JournalFatals = scanFatalSignatures(currJrn)
				if len(adopt.JournalFatals) > 0 {
					adopt.Adoptable = false
					details["kwin_late_adopt_invocation_fatals"] = adopt.JournalFatals
				}
			}
			// Restart-loop protection. The probe above only samples
			// MainPID once. If the unit is restart-looping the PID
			// will change between samples; verify by re-reading the
			// PID after a short delay and refusing late adoption if
			// they differ. The verification interval is the same as
			// the gate-poll probe interval (default 2s) so the total
			// added latency is bounded.
			if adopt.Adoptable {
				verifyInterval := probeInterval
				if verifyInterval <= 0 {
					verifyInterval = 2 * time.Second
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(verifyInterval):
				}
				second, err := p.mainPID(ctx, deps, choice.UnitName)
				details["kwin_late_adopt_verify_pid"] = second
				if err != nil || second != adopt.MainPID || second <= 0 {
					adopt.Adoptable = false
					details["kwin_late_adopt_restart_loop"] = true
					log.Warn("phase kwin-session: late adopt rejected; MainPID changed between samples (restart loop)",
						"first", adopt.MainPID, "second", second, "err", err)
				}
			}
			if adopt.Adoptable || lateAdoptionGrace <= 0 || time.Now().After(lateDeadline) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(probeInterval):
			}
		}
		if adopt.Adoptable {
			log.Info("phase kwin-session: late adoption succeeded after slow start",
				"unit", choice.UnitName, "main_pid", adopt.MainPID, "socket_present", adopt.SocketPresent)
			selected.MainPID = adopt.MainPID
			selected.InvocationID = p.readUnitInvocationID(ctx, deps, choice.UnitName)
			selected.ServiceStartTime = p.readUnitActiveEnterTimestamp(ctx, deps, choice.UnitName)
			details["kwin_selected"] = selected
			details["kwin_pid"] = adopt.MainPID
			details["service_active"] = true
			details["wayland_socket_ok"] = true
			details["adopted_after_late_start"] = true
			details["kwin_failure_category"] = ""
			deps.State.MarkDone(KWinSessionName, details)
			_ = deps.PersistState()
			return nil
		}
		// Late adoption also failed → produce a categorized error.
		details["err"] = gateErr.Error()
		details["kwin_failure_category"] = classifyKWinFailure(gateErr, strings.Join(sample.JournalFatals, "\n"), sample.SubState)
		if sample.StartProgressing() {
			details["kwin_failure_category"] = KWinFailSessionStartSlow
			if !sample.SocketPresent || !sample.SessionOnSeat {
				details["kwin_failure_category"] = KWinFailSocketSessionLate
			}
		}
		// Specific category for the "service active but socket missing
		// after window" shape, which is what the live VM hit.
		if sample.ActiveState == "active" && sample.MainPID > 0 && !sample.SocketPresent && !sample.StartProgressing() {
			details["kwin_failure_category"] = KWinFailWaylandSocketMissing
		}
		if sample.ActiveState == "active" && sample.MainPID > 0 && sample.SocketPresent && len(sample.JournalFatals) == 0 && !sample.SessionOnSeat && !sample.StartProgressing() {
			details["kwin_failure_category"] = KWinFailSessionNotOnSeatTTY
		}
		reason := "kwin gate poll did not converge"
		if details["kwin_failure_category"] == KWinFailSocketSessionLate {
			reason = KWinFailSocketSessionLate
		} else if details["kwin_failure_category"] == KWinFailSessionStartSlow {
			reason = KWinFailSessionStartSlow
		}
		deps.State.MarkFailed(KWinSessionName, reason, gateErr, true)
		deps.State.Get(KWinSessionName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase kwin-session: %w", gateErr)
	}

	// Gate poll converged. The unified poll already verified the
	// hard kwin_session gates:
	// active=true + MainPID>0 + socket present + invocation journal
	// clean + session on expected seat/tty + MainPID stable for >=
	// stability seconds. DBus/supportInformation are useful
	// diagnostics for later mode-setting phases, but live Plasma 6
	// evidence shows org.kde.KWin can appear late or under a
	// different bus shape; keep them soft here.
	pid := sample.MainPID
	invID := sample.InvocationID
	activeEnter := sample.ActiveEnter
	journalLog, _ := p.journalForInvocation(ctx, deps, choice.UnitName, invID, activeEnter, 200)

	// 8. KWin DBus probe: best-effort only. Later phases that need
	// org.kde.KWin (mode helper / DRM display validation) can retry
	// with their own timeout and diagnostics.
	if !choice.IsWeston {
		dbusWait := p.DBusProbeWait
		if dbusWait == 0 {
			dbusWait = stability + 45*time.Second
			if dbusWait < 60*time.Second {
				dbusWait = 60 * time.Second
			}
		} else if dbusWait < 0 {
			dbusWait = 0
		}
		dbusDeadline := time.Now().Add(dbusWait)
		var (
			dbusOK   bool
			dbusInfo string
			dbusErr  error
		)
		for {
			dbusOK, dbusInfo, dbusErr = p.kwinDBus(ctx, deps, desk.User, uid)
			if dbusOK {
				break
			}
			if dbusWait <= 0 || !time.Now().Before(dbusDeadline) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1 * time.Second):
			}
		}
		details["dbus_kwin_ok"] = dbusOK
		if dbusInfo != "" {
			details["dbus_kwin_info"] = dbusInfo
		}
		if dbusErr != nil {
			details["dbus_kwin_error"] = dbusErr.Error()
		}
		details["kwin_dbus_available"] = dbusOK
		details["kwin_dbus_degraded"] = !dbusOK
		if !dbusOK {
			details["kwin_dbus_warning"] = fmt.Sprintf("org.kde.KWin not present on %s's session bus after %s; compositor hard gates passed; last probe: %v (info=%q)",
				desk.User, dbusWait, dbusErr, dbusInfo)
		} else {
			// 9. supportInformation: best-effort diagnostic. The
			// mode/display validators can make this stricter later.
			support, supErr := p.supportInformation(ctx, deps, desk.User, uid)
			if supErr != nil {
				details["support_information_ok"] = false
				details["support_information_error"] = supErr.Error()
			} else {
				details["support_information_bytes"] = len(support)
				details["support_information_ok"] = true
				connectorOK := connector == "" || strings.Contains(support, connector)
				details["support_information_connector_ok"] = connectorOK
				backend := parseKWinOutputBackend(support)
				details["support_information_output_backend"] = backend
				nestedBackend := isNestedKWinBackend(backend) || strings.Contains(journalLog, "automatically choosing Wayland because WAYLAND_DISPLAY is set")
				details["support_information_nested_backend"] = nestedBackend
				drmBackendOK := strings.EqualFold(backend, "DRM")
				inferredDRM := false
				if !drmBackendOK && !nestedBackend && connectorOK && choice.IsRealVT && strings.Contains(in.ExecStart, "--drm") && resolvedDRM != "" {
					inferredDRM = true
				}
				details["support_information_drm_backend"] = drmBackendOK
				details["support_information_drm_backend_inferred"] = inferredDRM
				if nestedBackend || (!drmBackendOK && !inferredDRM) {
					details["support_information_warning"] = fmt.Sprintf("supportInformation does not prove DRM backend. Parsed backend=%q inferred_drm=%v nested=%v tail=%q",
						backend, inferredDRM, nestedBackend, lastLines(support, 6))
				}
				if !connectorOK {
					details["support_information_connector_warning"] = fmt.Sprintf("supportInformation does not mention forced connector %q; tail: %q",
						connector, lastLines(support, 8))
				}
			}
		}
	}

	// 10. Run the mode helper from Go (NOT via ExecStartPost). At
	// this point the service is validated; if the helper hangs, we
	// kill it after 90s and mark mode-helper-timeout but DO NOT
	// fail the phase. Live VM evidence (2026-05-22): an
	// ExecStartPost mode helper hung the unit in start-post; running
	// it from here can never block service activation.
	if choice.IsRealVT && !p.SkipModeHelper {
		modeOK, modeInfo := p.runModeHelperBounded(ctx, deps, log)
		details["mode_helper_ok"] = modeOK
		details["mode_helper_info"] = modeInfo
	}

	deps.State.MarkDone(KWinSessionName, details)
	_ = deps.PersistState()
	log.Info("phase kwin-session: done",
		"user", desk.User, "uid", uid, "unit", choice.UnitName,
		"backend", desk.SessionBackend, "compositor_mode", desk.CompositorMode,
		"pid", pid, "socket", socketPath)
	return nil
}

// kwinDBus checks the headless user's session bus for org.kde.KWin.
// Returns (true, "", nil) on success. The string return is a short
// diagnostic line for state.Details["dbus_kwin_info"].
func (p KWinSession) kwinDBus(ctx context.Context, deps *Deps, user, uid string) (bool, string, error) {
	if p.KWinDBusFn != nil {
		return p.KWinDBusFn(ctx, deps, user, uid)
	}
	if deps.DryRun {
		return true, "dry-run synthesized", nil
	}
	qdbus, qerr := resolveQDBus()
	if qerr != nil {
		return false, "", qerr
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"sudo", "-u", user,
			"env",
			"XDG_RUNTIME_DIR=/run/user/" + uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
			qdbus.Path, "--session", "org.kde.KWin", "/KWin",
			"org.freedesktop.DBus.Introspectable.Introspect",
		},
		Sudo:    true,
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	if res.Err != nil {
		return false, lastLines(res.Stderr, 2), res.Err
	}
	return true, "introspect OK via " + qdbus.Path, nil
}

// supportInformation runs `qdbus org.kde.KWin /KWin org.kde.KWin.supportInformation`.
// Returns the full dump string. The phase searches it for the
// "Output backend: DRM" line.
func (p KWinSession) supportInformation(ctx context.Context, deps *Deps, user, uid string) (string, error) {
	if p.SupportInformationFn != nil {
		return p.SupportInformationFn(ctx, deps, user, uid)
	}
	if deps.DryRun {
		// Synthesize a passing dump so apply --dry-run can demo end-to-end.
		return "Output backend: DRM\nCompositing backend: OpenGL\nOutput: DP-1\n", nil
	}
	qdbus, qerr := resolveQDBus()
	if qerr != nil {
		return "", qerr
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"sudo", "-u", user,
			"env",
			"XDG_RUNTIME_DIR=/run/user/" + uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
			qdbus.Path, "--session", "org.kde.KWin", "/KWin", "org.kde.KWin.supportInformation",
		},
		Sudo:    true,
		Timeout: 30 * time.Second,
	})
	if res.Err != nil {
		return res.Stdout, fmt.Errorf("qdbus supportInformation: %w (stderr=%q)",
			res.Err, lastLines(res.Stderr, 3))
	}
	return res.Stdout, nil
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

func firstLineContaining(s, needle string) string {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func parseKWinOutputBackend(support string) string {
	lines := strings.Split(support, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		for _, prefix := range []string{"output backend:", "backend:", "platform:"} {
			if strings.HasPrefix(lower, prefix) {
				v := strings.TrimSpace(line[len(prefix):])
				if v != "" {
					return normalizeKWinBackend(v)
				}
			}
		}
		if strings.EqualFold(line, "Output backend") || strings.EqualFold(line, "Backend") || strings.EqualFold(line, "Platform") {
			for j := i + 1; j < len(lines) && j < i+8; j++ {
				next := strings.TrimSpace(strings.TrimRight(lines[j], "\r"))
				if next == "" {
					break
				}
				if strings.HasPrefix(strings.ToLower(next), "name:") {
					return normalizeKWinBackend(strings.TrimSpace(next[len("name:"):]))
				}
			}
		}
	}
	return ""
}

func normalizeKWinBackend(v string) string {
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	fields := strings.Fields(v)
	if len(fields) > 0 {
		v = fields[0]
	}
	switch strings.ToLower(v) {
	case "drm", "kms":
		return "DRM"
	case "wayland", "nestedwayland", "nested-wayland":
		return "Wayland"
	default:
		return v
	}
}

func isNestedKWinBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "wayland", "nestedwayland", "nested-wayland", "x11":
		return true
	}
	return false
}

func recordKWinGateDetails(details map[string]any, selected *KWinSelectedPath, sample KWinGateSample) {
	if details == nil {
		return
	}
	details["kwin_last_gate_sample"] = sample
	details["wayland_socket_ok"] = sample.SocketPresent
	details["service_active_state"] = sample.ActiveState
	details["service_active"] = sample.ActiveState == "active"
	details["socket_stable_seconds"] = sample.StablePIDFor
	details["kwin_sub_state"] = sample.SubState
	details["kwin_invocation_id"] = sample.InvocationID
	details["kwin_gate_wait_extended"] = sample.WaitExtended
	details["kwin_session_on_expected_seat_tty"] = sample.SessionOnSeat
	details["kwin_pid"] = sample.MainPID
	if sample.MainPID > 0 {
		if selected != nil {
			selected.MainPID = sample.MainPID
			selected.InvocationID = sample.InvocationID
			selected.ServiceStartTime = sample.ActiveEnter
			details["kwin_selected"] = *selected
		}
	}
	if len(sample.JournalFatals) > 0 {
		details["journal_fatal_hits"] = sample.JournalFatals
	} else {
		delete(details, "journal_fatal_hits")
	}
}

func recordKWinSelectedDetails(details map[string]any, selected KWinSelectedPath) {
	if details == nil {
		return
	}
	details["kwin_selected"] = selected
	details["kwin_user"] = selected.User
	details["kwin_uid"] = selected.UID
	details["kwin_seat"] = selected.Seat
	details["kwin_tty"] = selected.TTY
	details["kwin_drm_card"] = selected.DRMCard
	details["kwin_render_node"] = selected.RenderNode
	details["kwin_socket"] = selected.Socket
	details["kwin_service"] = selected.Service
	details["wayland_socket_path"] = selected.Socket
	if selected.MainPID > 0 {
		details["kwin_pid"] = selected.MainPID
	}
	if selected.InvocationID != "" {
		details["kwin_invocation_id"] = selected.InvocationID
	}
}

func recordKWinAdoptionDetails(details map[string]any, prefix string, adopt KWinAdoptCheck) {
	if details == nil {
		return
	}
	if prefix == "" {
		prefix = "kwin_adopt"
	}
	details[prefix+"_active_state"] = adopt.ActiveState
	details[prefix+"_sub_state"] = adopt.SubState
	details[prefix+"_main_pid"] = adopt.MainPID
	details[prefix+"_socket_present"] = adopt.SocketPresent
	details[prefix+"_uptime_seconds"] = adopt.UptimeSeconds
	details[prefix+"_journal_fatals"] = adopt.JournalFatals
	details[prefix+"_adoptable"] = adopt.Adoptable
}

type KWinSocketRecovery struct {
	StopAttempted      bool     `json:"stop_attempted"`
	StopErr            string   `json:"stop_err,omitempty"`
	KillAttempted      bool     `json:"kill_attempted"`
	KillErr            string   `json:"kill_err,omitempty"`
	RemovedSocketPaths []string `json:"removed_socket_paths,omitempty"`
	Err                string   `json:"err,omitempty"`
}

func (p KWinSession) shouldRecoverUnlinkedKWinSocket(ctx context.Context, deps *Deps, unit, user, uid, tty string, adopt KWinAdoptCheck, details map[string]any) bool {
	if adopt.ActiveState != "active" || adopt.MainPID <= 0 || adopt.SocketPresent || len(adopt.JournalFatals) > 0 {
		return false
	}
	if adopt.SubState != "" && adopt.SubState != "running" {
		return false
	}
	sessionOK := p.loginctlSessionMatchesSeatTTY(ctx, deps, user, uid, "seat0", tty, adopt.MainPID)
	dbusOK, dbusInfo, dbusErr := p.kwinDBus(ctx, deps, user, uid)
	if details != nil {
		details["kwin_socket_unlinked_candidate"] = true
		details["kwin_socket_unlinked_session_ok"] = sessionOK
		details["kwin_socket_unlinked_dbus_ok"] = dbusOK
		if dbusInfo != "" {
			details["kwin_socket_unlinked_dbus_info"] = dbusInfo
		}
		if dbusErr != nil {
			details["kwin_socket_unlinked_dbus_error"] = dbusErr.Error()
		}
	}
	return sessionOK && dbusOK
}

func (p KWinSession) recoverUnlinkedKWinSocket(ctx context.Context, deps *Deps, unit, user, uid string, log *slog.Logger) KWinSocketRecovery {
	out := KWinSocketRecovery{}
	out.StopAttempted = true
	if err := p.systemctl(ctx, deps, "stop", unit); err != nil {
		out.StopErr = err.Error()
		out.Err = err.Error()
	}
	out.KillAttempted = true
	if err := p.killKWin(ctx, deps, user); err != nil {
		out.KillErr = err.Error()
		if out.Err == "" {
			out.Err = err.Error()
		}
	}
	out.RemovedSocketPaths = cleanWaylandRuntimeSockets(filepath.Join("/run/user", uid), false)
	if log != nil {
		log.Warn("phase kwin-session: recovered active KWin with unlinked wayland socket",
			"unit", unit, "user", user, "uid", uid,
			"stop_err", out.StopErr, "kill_err", out.KillErr,
			"removed", out.RemovedSocketPaths)
	}
	return out
}

func (p KWinSession) killKWin(ctx context.Context, deps *Deps, user string) error {
	if p.KillKWinFn != nil {
		return p.KillKWinFn(ctx, deps, user)
	}
	if p.SystemctlFn != nil {
		return nil
	}
	if deps == nil || deps.Runner == nil {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"pkill", "-u", user, "-x", "kwin_wayland"},
		Sudo:    true,
		LogFile: "-",
		Timeout: 10 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil && res.ExitCode != 1 {
		return res.Err
	}
	return nil
}

func (p KWinSession) activeMainPID(ctx context.Context, deps *Deps, unit string) int {
	state, err := p.isActive(ctx, deps, unit)
	if err != nil || state != "active" {
		return 0
	}
	pid, err := p.mainPID(ctx, deps, unit)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
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

// staleKDEConfigPaths is the v2-derived list of per-user KDE
// config/cache directories that, when carried over from a previous
// boot, cause KWin/plasmashell to come up with no wl_output or to
// pin an unavailable mode. Empty string entries are skipped at
// runtime. Relative to the headless user's $HOME.
var staleKDEConfigPaths = []string{
	".local/share/kscreen",
	".config/kwinoutputconfig.json",
	".cache/kscreen",
	".cache/kscreend",
	".cache/plasmashell",
	".cache/plasma-systemmonitor",
}

// cleanStaleKDEConfigIfNeeded removes per-user KDE/KScreen caches and
// stale wayland sockets BEFORE the kwin-realvt unit starts, but only
// when the previous kwin_session run failed (or this is the first
// run). The "operator pinned kwinrc" case is preserved by never
// touching ~/.config/kwinrc itself - only the output-config sidecars
// and KScreen cache.
//
// Returns the list of paths actually removed (for state.Details).
// Idempotent and best-effort: errors are swallowed so a missing path
// never prevents the start.
func cleanStaleKDEConfigIfNeeded(deps *Deps, user, uid string, protectWaylandSockets bool) []string {
	if deps == nil || deps.DryRun {
		return nil
	}
	if !shouldCleanStaleKDEConfig(deps) {
		return nil
	}
	home := "/home/" + user
	var removed []string
	for _, rel := range staleKDEConfigPaths {
		if rel == "" {
			continue
		}
		full := filepath.Join(home, rel)
		if _, err := os.Stat(full); err != nil {
			continue
		}
		if err := os.RemoveAll(full); err == nil {
			removed = append(removed, full)
		}
	}
	// Orphan wayland sockets from a previous compositor run. KWin
	// will recreate wayland-0 the moment it owns DRM master; an
	// orphan socket with no listener confuses Sunshine + wayland-info.
	if uid != "" {
		removed = append(removed, cleanWaylandRuntimeSockets(filepath.Join("/run/user", uid), protectWaylandSockets)...)
	}
	return removed
}

func cleanWaylandRuntimeSockets(runtimeDir string, protectWaylandSockets bool) []string {
	if protectWaylandSockets {
		return nil
	}
	var removed []string
	entries, _ := os.ReadDir(runtimeDir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "wayland-") {
			continue
		}
		full := filepath.Join(runtimeDir, name)
		if err := os.Remove(full); err == nil {
			removed = append(removed, full)
		}
	}
	return removed
}

// shouldCleanStaleKDEConfig returns true when the prior kwin_session
// phase status is anything OTHER than terminal-done. That covers:
//   - first ever run (no phase entry)
//   - last run failed (we want the clean slate)
//   - last run was pending/running (interrupted; same)
//
// When the prior run completed successfully, we leave the operator's
// configs alone - they may have tuned kscreen output IDs by hand.
func shouldCleanStaleKDEConfig(deps *Deps) bool {
	if deps == nil || deps.State == nil {
		return true
	}
	prev := deps.State.Get(KWinSessionName)
	if prev == nil {
		return true
	}
	switch prev.Status {
	case statepkg.StatusDone:
		return false
	default:
		return true
	}
}
