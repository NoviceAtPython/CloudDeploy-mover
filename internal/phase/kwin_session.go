package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// KWinSessionName is the canonical state-key.
const KWinSessionName = "kwin_session"

// KWinUnitName is the system-level systemd unit the phase installs.
const KWinUnitName = "clouddeploy-kwin-wayland.service"

// KWinUnitDefaultPath is the canonical install path.
const KWinUnitDefaultPath = "/etc/systemd/system/" + KWinUnitName

// kwinUnitTemplate is the rendered unit body. Placeholders are
// substituted by KWinSession.renderUnit().
const kwinUnitTemplate = `[Unit]
Description=CloudDeploy KWin Wayland session ({{ .User }})
After=systemd-user-sessions.service network-online.target
Wants=systemd-user-sessions.service

[Service]
Type=simple
User={{ .User }}
PAMName=login

# Wayland needs a writable XDG_RUNTIME_DIR. systemd-logind creates
# /run/user/{{ .UID }} when linger is on; we point WAYLAND/DBus at it.
Environment=XDG_RUNTIME_DIR=/run/user/{{ .UID }}
Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{{ .UID }}/bus
Environment=WAYLAND_DISPLAY=wayland-0
Environment=QT_QPA_PLATFORM=wayland
# NVIDIA Wayland prefers the EGL/GBM path, not the old EGLStreams.
Environment=KWIN_DRM_USE_EGL_STREAMS=0
# Generic NVIDIA Wayland hints.
Environment=__GLX_VENDOR_LIBRARY_NAME=nvidia
Environment=GBM_BACKEND=nvidia-drm
Environment=MOZ_ENABLE_WAYLAND=1

# Pre-step: make sure the systemd-user manager is up + the runtime
# dir's permissions look right. logind owns /run/user/<uid> in
# practice, but a defensive mkdir is cheap.
ExecStartPre=/bin/mkdir -p /run/user/{{ .UID }}
ExecStartPre=/bin/chown {{ .User }}:{{ .User }} /run/user/{{ .UID }}

ExecStart=/usr/bin/dbus-run-session -- /usr/bin/kwin_wayland --drm --no-lockscreen

Restart=on-failure
RestartSec=5

StandardOutput=journal
StandardError=journal
SyslogIdentifier=clouddeploy-kwin

[Install]
WantedBy=multi-user.target
`

// KWinSession is the fourth Milestone 4A phase. It writes a system-
// level systemd service for the headless user that brings up the
// KWin Wayland compositor against the forced connector. The phase:
//
//  1. Renders the unit from kwinUnitTemplate using profile.Desktop.User
//     + the UID recorded by headless_user.
//  2. Writes /etc/systemd/system/clouddeploy-kwin-wayland.service
//     atomically.
//  3. systemctl daemon-reload + enable + start (gated by DryRun).
//  4. Polls /run/user/<uid>/wayland-0 for up to 30s. The socket
//     appearing is the canonical "compositor is alive" signal.
//
// The phase NEVER starts plasmashell; the first goal is the
// compositor itself. Later milestones can layer Plasma's UX.
type KWinSession struct {
	// UnitPath overrides KWinUnitDefaultPath for tests.
	UnitPath string

	// SystemctlFn lets tests stub out `systemctl <verb> [...]`.
	// nil = runner.
	SystemctlFn func(ctx context.Context, deps *Deps, args ...string) error

	// WaylandSocketFn lets tests stub the socket-exists probe. nil
	// = real os.Stat with retry.
	WaylandSocketFn func(uid string) bool

	// SocketWait caps how long the phase waits for wayland-0. 0 =
	// 30s default.
	SocketWait time.Duration
}

// Name implements Phase.
func (KWinSession) Name() string { return KWinSessionName }

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
	details := map[string]any{
		"user": desk.User,
	}

	// Pull UID from headless_user's recorded state.
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

	// 1. Render + write the unit.
	body := p.renderUnit(desk.User, uid)
	details["unit_path"] = p.unitPath()
	details["unit_bytes"] = len(body)

	if !deps.DryRun {
		if err := p.writeUnit(body); err != nil {
			details["err"] = err.Error()
			deps.State.MarkFailed(KWinSessionName, "write systemd unit", err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: %w", err)
		}
	}

	// 2. daemon-reload + enable + start.
	steps := []struct {
		name string
		args []string
	}{
		{"daemon-reload", []string{"daemon-reload"}},
		{"enable", []string{"enable", KWinUnitName}},
		{"start", []string{"start", KWinUnitName}},
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

	// 3. Wait for the Wayland socket.
	wait := p.SocketWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	socketPath := waylandSocketPath(uid)
	details["wayland_socket_path"] = socketPath
	if !deps.DryRun {
		ok := p.waitForSocket(ctx, uid, wait)
		details["wayland_socket_ok"] = ok
		if !ok {
			err := fmt.Errorf("wayland socket %s did not appear within %s", socketPath, wait)
			details["err"] = err.Error()
			deps.State.MarkFailed(KWinSessionName, "wayland socket missing", err, true)
			deps.State.Get(KWinSessionName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase kwin-session: %w", err)
		}
	} else {
		// DryRun: pretend the socket is up so the apply pipeline
		// can demo end-to-end.
		details["wayland_socket_ok"] = true
	}

	deps.State.MarkDone(KWinSessionName, details)
	_ = deps.PersistState()
	log.Info("phase kwin-session: done",
		"user", desk.User, "uid", uid, "unit", KWinUnitName,
		"socket", socketPath)
	return nil
}

// renderUnit substitutes {{ .User }} and {{ .UID }} placeholders.
func (p KWinSession) renderUnit(user, uid string) string {
	out := kwinUnitTemplate
	out = strings.ReplaceAll(out, "{{ .User }}", user)
	out = strings.ReplaceAll(out, "{{ .UID }}", uid)
	return out
}

// RenderUnitText is exported so tests + doctor kwin can show the
// rendered unit body without touching disk.
func RenderUnitText(user, uid string) string {
	return KWinSession{}.renderUnit(user, uid)
}

func (p KWinSession) unitPath() string {
	if p.UnitPath != "" {
		return p.UnitPath
	}
	return KWinUnitDefaultPath
}

func (p KWinSession) writeUnit(body string) error {
	path := p.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("kwin-session: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("kwin-session: write %s: %w", path, err)
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

func (p KWinSession) waitForSocket(ctx context.Context, uid string, max time.Duration) bool {
	socketProbe := p.WaylandSocketFn
	if socketProbe == nil {
		socketProbe = func(u string) bool {
			_, err := os.Stat(waylandSocketPath(u))
			return err == nil
		}
	}
	deadline := time.Now().Add(max)
	for {
		if socketProbe(uid) {
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
