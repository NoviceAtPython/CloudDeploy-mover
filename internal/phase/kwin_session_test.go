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

// happyKwin returns a KWinSession with every dependency stubbed for
// the happy path: short waits, socket always present, is-active=
// "active", MainPID=42, empty journal. Tests override individual
// fields when they want to exercise a failure path.
func happyKwin(unitPath string) KWinSession {
	return KWinSession{
		UnitPath:        unitPath,
		SystemctlFn:     func(context.Context, *Deps, ...string) error { return nil },
		WaylandSocketFn: func(string) bool { return true },
		SystemctlIsActiveFn: func(context.Context, *Deps, string) (string, error) {
			return "active", nil
		},
		SystemctlShowMainPIDFn: func(context.Context, *Deps, string) (int, error) {
			return 42, nil
		},
		JournalRecentFn: func(context.Context, *Deps, string, int) (string, error) {
			return "kwin_core: starting up\nDP-1 enabled\n", nil
		},
		SocketWait:      2 * time.Second,
		SocketStability: 100 * time.Millisecond,
	}
}

// -----------------------------------------------------------------------------
// rendered unit text (default = realvt/plasma)
// -----------------------------------------------------------------------------

func TestRenderUnitText_RealVTPlasmaIsTheDefault(t *testing.T) {
	out := RenderUnitText("cloudgamer", "1001")
	for _, want := range []string{
		"User=cloudgamer",
		"XDG_RUNTIME_DIR=/run/user/1001",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus",
		"WAYLAND_DISPLAY=wayland-0",
		// v2-parity real-VT keys.
		"TTYPath=/dev/tty7",
		"PAMName=login",
		"PermissionsStartOnly=true",
		"TTYReset=yes",
		"TTYVHangup=yes",
		"TTYVTDisallocate=yes",
		"UtmpIdentifier=tty7",
		"UtmpMode=user",
		"SupplementaryGroups=video render input",
		// NVIDIA-specific env the live-VM postmortem demands.
		"KWIN_DRM_DEVICES=/dev/dri/card1",
		"KWIN_DRM_NO_DIRECT_SCANOUT=1",
		"KWIN_FORCE_SW_CURSOR=1",
		"KWIN_USE_OVERLAYS=0",
		"GBM_BACKEND=nvidia-drm",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json",
		"__GLX_VENDOR_LIBRARY_NAME=nvidia",
		// chvt + Plasma binary.
		"chvt 7",
		"startplasma-wayland",
		// force-kwin-mode helper.
		"clouddeploy-force-kwin-mode.sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered unit missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Errorf("rendered unit still contains template placeholders:\n%s", out)
	}
}

func TestForceKwinModeScript_GuardsConcurrencyAndForcesScale(t *testing.T) {
	in := templateInputs{
		User:       "cloudgamer",
		UID:        "1001",
		Connector:  "DP-1",
		Resolution: "3840x2160",
		Refresh:    120,
	}
	out := in.apply(forceKwinModeScriptBody)
	for _, want := range []string{
		"flock -n 9",
		"timeout 20s runuser -u",
		"output.${CONNECTOR}.scale.1",
		"3840x2160@120",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("force helper missing %q in:\n%s", want, out)
		}
	}
}

// -----------------------------------------------------------------------------
// chooseUnit + execStartFor
// -----------------------------------------------------------------------------

func TestChooseUnit_RealVTPlasma(t *testing.T) {
	_, c := chooseUnit("realvt", "plasma")
	if c.UnitName != "plasma-realvt.service" {
		t.Errorf("unit name: got %q want plasma-realvt.service", c.UnitName)
	}
	if !c.IsRealVT {
		t.Errorf("IsRealVT should be true for realvt backend")
	}
}

func TestChooseUnit_RealVTKwin(t *testing.T) {
	_, c := chooseUnit("realvt", "kwin")
	if c.UnitName != "kwin-realvt.service" {
		t.Errorf("unit name: got %q want kwin-realvt.service", c.UnitName)
	}
}

func TestChooseUnit_UserBackendBackwardsCompat(t *testing.T) {
	_, c := chooseUnit("user", "kwin")
	if c.UnitName != KWinUnitName {
		t.Errorf("unit name: got %q want %q", c.UnitName, KWinUnitName)
	}
	if c.IsRealVT {
		t.Errorf("user backend should NOT mark IsRealVT")
	}
}

func TestChooseUnit_Weston(t *testing.T) {
	_, c := chooseUnit("weston", "weston")
	if !c.IsWeston {
		t.Errorf("weston backend should mark IsWeston")
	}
	if c.UnitName != "weston-kms-session.service" {
		t.Errorf("unit name: got %q want weston-kms-session.service", c.UnitName)
	}
}

func TestExecStartFor_PicksRightBinary(t *testing.T) {
	cases := []struct {
		backend  string
		mode     string
		mustHave string
	}{
		{"realvt", "plasma", "startplasma-wayland"},
		{"realvt", "kwin", "kwin_wayland --drm --no-lockscreen"},
		{"user", "kwin", "dbus-run-session"},
		{"user", "plasma", "dbus-run-session"},
		{"weston", "weston", "weston --backend=drm-backend.so"},
	}
	for _, c := range cases {
		_, choice := chooseUnit(c.backend, c.mode)
		got := execStartFor(choice)
		if !strings.Contains(got, c.mustHave) {
			t.Errorf("execStartFor(%s/%s): got %q want substring %q",
				c.backend, c.mode, got, c.mustHave)
		}
	}
}

// -----------------------------------------------------------------------------
// happy path: full strict success criteria
// -----------------------------------------------------------------------------

func TestKWinSession_HappyPath_RealVTPlasma(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "plasma-realvt.service")

	systemctlCalls := [][]string{}
	ph := happyKwin(unitPath)
	ph.SystemctlFn = func(_ context.Context, _ *Deps, args ...string) error {
		systemctlCalls = append(systemctlCalls, append([]string(nil), args...))
		return nil
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
	if !strings.Contains(string(body), "TTYPath=/dev/tty7") {
		t.Errorf("real-VT unit body missing TTYPath=/dev/tty7:\n%s", string(body))
	}
	if !strings.Contains(string(body), "startplasma-wayland") {
		t.Errorf("real-VT unit body missing startplasma-wayland:\n%s", string(body))
	}

	// systemctl steps target the realvt unit name.
	wantSteps := [][]string{
		{"daemon-reload"},
		{"enable", "plasma-realvt.service"},
		{"start", "plasma-realvt.service"},
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
	if d["service_active"] != true {
		t.Errorf("service_active: got %v want true", d["service_active"])
	}
	if d["service_active_state"] != "active" {
		t.Errorf("service_active_state: got %v want active", d["service_active_state"])
	}
	if d["service_name"] != "plasma-realvt.service" {
		t.Errorf("service_name: got %v want plasma-realvt.service", d["service_name"])
	}
	if d["backend"] != "realvt" {
		t.Errorf("backend: got %v want realvt", d["backend"])
	}
	if d["compositor_mode"] != "plasma" {
		t.Errorf("compositor_mode: got %v want plasma", d["compositor_mode"])
	}
	if d["vt"] != 7 {
		t.Errorf("vt: got %v want 7", d["vt"])
	}
	if d["selected_drm_device"] != "/dev/dri/card1" {
		t.Errorf("selected_drm_device: got %v want /dev/dri/card1", d["selected_drm_device"])
	}
	if d["kwin_pid"] != 42 {
		t.Errorf("kwin_pid: got %v want 42", d["kwin_pid"])
	}
}

// -----------------------------------------------------------------------------
// stricter success criteria: each new gate gets a test
// -----------------------------------------------------------------------------

func TestKWinSession_TransientSocketFailsFatal(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")

	socketChecks := 0
	ph := happyKwin(unitPath)
	ph.SocketStability = 3 * time.Second
	ph.WaylandSocketFn = func(string) bool {
		socketChecks++
		// First probe true (waitForSocket); subsequent stability
		// checks return false to simulate KWin's restart loop.
		return socketChecks <= 1
	}

	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when socket disappears during stability window")
	}
	if !strings.Contains(err.Error(), "stability window") {
		t.Errorf("error should mention the stability window: %v", err)
	}
	if got := deps.State.Get(KWinSessionName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestKWinSession_NotActiveAfterStability_FailsFatal(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SystemctlIsActiveFn = func(context.Context, *Deps, string) (string, error) {
		return "activating", nil
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when is-active != active")
	}
	if !strings.Contains(err.Error(), "is-active") || !strings.Contains(err.Error(), "activating") {
		t.Errorf("error should report the actual is-active value: %v", err)
	}
}

func TestKWinSession_MainPIDZeroFailsFatal(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SystemctlShowMainPIDFn = func(context.Context, *Deps, string) (int, error) {
		return 0, nil
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when MainPID is zero")
	}
	if !strings.Contains(err.Error(), "MainPID=0") {
		t.Errorf("error should mention MainPID=0: %v", err)
	}
}

func TestKWinSession_FatalJournalSignatureFailsFatal(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.JournalRecentFn = func(context.Context, *Deps, string, int) (string, error) {
		// Exact substring from the live VM postmortem.
		return "kwin_wayland_drm: No suitable DRM devices have been found\n", nil
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when journal contains a known-bad signature")
	}
	if !strings.Contains(err.Error(), "fatal signature") {
		t.Errorf("error should mention fatal signature: %v", err)
	}
	d := deps.State.Get(KWinSessionName).Details
	hits, _ := d["journal_fatal_hits"].([]string)
	if len(hits) == 0 || !strings.Contains(hits[0], "DRM devices") {
		t.Errorf("journal_fatal_hits should record the matched signature; got %v", hits)
	}
}

func TestKWinSession_FailsWhenSocketNeverAppears(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.WaylandSocketFn = func(string) bool { return false }
	ph.SocketWait = 500 * time.Millisecond

	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when socket never appears")
	}
	if !strings.Contains(err.Error(), "wayland socket") {
		t.Errorf("error should mention wayland socket: %v", err)
	}
}

func TestKWinSession_FailsWhenSystemctlEnableFails(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SystemctlFn = func(_ context.Context, _ *Deps, args ...string) error {
		if len(args) > 0 && args[0] == "enable" {
			return errors.New("simulated systemctl enable failure")
		}
		return nil
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

// scanFatalSignatures is exercised indirectly above; explicit unit
// test to lock the live-VM signatures.
func TestScanFatalSignatures(t *testing.T) {
	if got := scanFatalSignatures(""); len(got) != 0 {
		t.Errorf("empty journal: got %v want empty", got)
	}
	if got := scanFatalSignatures("kwin_wayland_drm: failed to open drm device at /dev/dri/card1"); len(got) != 1 {
		t.Errorf("matched signature: got %v want one entry", got)
	}
	if got := scanFatalSignatures("Failed to activate /org/freedesktop/login1/session/_42 session"); len(got) != 1 {
		t.Errorf("login1 signature: got %v want one entry", got)
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
