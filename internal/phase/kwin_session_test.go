package phase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func kwinDeps(t *testing.T) *Deps {
	t.Helper()
	deps := newDeps(t, desktopProfile(), nil)
	// kwin-session honors DryRun: it skips unit writes + synthesizes
	// wayland_socket_ok=true. Tests want the real code paths, so
	// flip DryRun off; SystemctlFn / WaylandSocketFn are stubbed
	// so we still don't touch real systemctl.
	deps.DryRun = false
	// Seed headless_user state so kwin-session can read the UID.
	deps.State.MarkDone(HeadlessUserName, map[string]any{
		"user": "cloudgamer",
		"uid":  "1001",
		"gid":  "1001",
		"home": "/home/cloudgamer",
	})
	return deps
}

func TestRenderUnitText_HasUserAndUIDSubstituted(t *testing.T) {
	out := RenderUnitText("cloudgamer", "1001")
	for _, want := range []string{
		"User=cloudgamer",
		"XDG_RUNTIME_DIR=/run/user/1001",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus",
		"WAYLAND_DISPLAY=wayland-0",
		"kwin_wayland --drm --no-lockscreen",
		"SyslogIdentifier=clouddeploy-kwin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered unit missing %q in:\n%s", want, out)
		}
	}
	// Placeholder bodies must have been fully substituted.
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Errorf("rendered unit still contains template placeholders:\n%s", out)
	}
}

func TestKWinSession_WritesUnitEnablesStartsAndWaitsForSocket(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "clouddeploy-kwin-wayland.service")

	systemctlCalls := [][]string{}
	socketCalls := 0

	ph := KWinSession{
		UnitPath: unitPath,
		SystemctlFn: func(_ context.Context, _ *Deps, args ...string) error {
			systemctlCalls = append(systemctlCalls, append([]string(nil), args...))
			return nil
		},
		WaylandSocketFn: func(uid string) bool {
			socketCalls++
			// Pretend the socket comes up after the second probe.
			return socketCalls > 1
		},
		SocketWait: 5 * time.Second,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(KWinSessionName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}

	// Unit file written.
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(body), "User=cloudgamer") {
		t.Errorf("unit body missing User=cloudgamer:\n%s", string(body))
	}

	// systemctl steps in order.
	wantSteps := [][]string{
		{"daemon-reload"},
		{"enable", KWinUnitName},
		{"start", KWinUnitName},
	}
	if len(systemctlCalls) != len(wantSteps) {
		t.Fatalf("systemctl calls: got %v want %v", systemctlCalls, wantSteps)
	}
	for i, want := range wantSteps {
		if !equalStrings(systemctlCalls[i], want) {
			t.Errorf("systemctl step %d: got %v want %v", i, systemctlCalls[i], want)
		}
	}

	d := deps.State.Get(KWinSessionName).Details
	if d["wayland_socket_ok"] != true {
		t.Errorf("wayland_socket_ok: got %v want true", d["wayland_socket_ok"])
	}
	if d["unit_path"] != unitPath {
		t.Errorf("unit_path: got %v want %q", d["unit_path"], unitPath)
	}
	if d["uid"] != "1001" {
		t.Errorf("uid: got %v want 1001", d["uid"])
	}
}

func TestKWinSession_FailsWhenSocketNeverAppears(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := KWinSession{
		UnitPath: unitPath,
		SystemctlFn: func(context.Context, *Deps, ...string) error {
			return nil
		},
		WaylandSocketFn: func(string) bool { return false },
		SocketWait:      1 * time.Second,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when socket never appears")
	}
	if !strings.Contains(err.Error(), "wayland socket") {
		t.Errorf("error should mention wayland socket: %v", err)
	}
	if got := deps.State.Get(KWinSessionName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestKWinSession_FailsWhenSystemctlEnableFails(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := KWinSession{
		UnitPath: unitPath,
		SystemctlFn: func(_ context.Context, _ *Deps, args ...string) error {
			if len(args) > 0 && args[0] == "enable" {
				return errors.New("simulated systemctl enable failure")
			}
			return nil
		},
		WaylandSocketFn: func(string) bool { return true },
		SocketWait:      time.Second,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when enable fails")
	}
	d := deps.State.Get(KWinSessionName).Details
	if d["failed_step"] != "enable" {
		t.Errorf("failed_step: got %v want enable", d["failed_step"])
	}
}

func TestKWinSession_FailsWithoutUID(t *testing.T) {
	deps := newDeps(t, desktopProfile(), nil)
	// Intentionally do NOT seed headless_user state.
	ph := KWinSession{UnitPath: filepath.Join(t.TempDir(), "u.service")}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when uid missing")
	}
	if !strings.Contains(err.Error(), "uid") {
		t.Errorf("error should mention uid: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
