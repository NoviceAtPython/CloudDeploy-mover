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
// the happy path. Tests override individual fields when they want to
// exercise a failure path.
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
		KWinDBusFn: func(context.Context, *Deps, string, string) (bool, string, error) {
			return true, "introspect OK", nil
		},
		SupportInformationFn: func(context.Context, *Deps, string, string) (string, error) {
			return "Output backend: DRM\nCompositing backend: OpenGL\nOutput: DP-1\n", nil
		},
		SocketWait:      2 * time.Second,
		SocketStability: 100 * time.Millisecond,
	}
}

// -----------------------------------------------------------------------------
// rendered unit text (default = realvt/kwin)
// -----------------------------------------------------------------------------

func TestRenderUnitText_RealVTKWinIsTheDefault(t *testing.T) {
	out := RenderUnitText("cloudgamer", "1001")
	for _, want := range []string{
		"User=cloudgamer",
		"XDG_RUNTIME_DIR=/run/user/1001",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus",
		"TTYPath=/dev/tty7",
		"PAMName=login",
		"TTYReset=yes",
		"TTYVHangup=yes",
		"TTYVTDisallocate=yes",
		"UtmpIdentifier=tty7",
		"UtmpMode=user",
		"SupplementaryGroups=video render input",
		"KWIN_DRM_DEVICES=/dev/dri/card1",
		"KWIN_DRM_NO_DIRECT_SCANOUT=1",
		"KWIN_FORCE_SW_CURSOR=1",
		"KWIN_USE_OVERLAYS=0",
		"GBM_BACKEND=nvidia-drm",
		"__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/10_nvidia.json",
		"__GLX_VENDOR_LIBRARY_NAME=nvidia",
		"chvt 7",
		"kwin_wayland --drm --socket wayland-0 --no-lockscreen",
		"clouddeploy-force-kwin-mode.sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered unit missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Environment=WAYLAND_DISPLAY=wayland-0") {
		t.Errorf("rendered unit must not set WAYLAND_DISPLAY before launching KWin:\n%s", out)
	}
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Errorf("rendered unit still contains template placeholders:\n%s", out)
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
		{"realvt", "kwin", "kwin_wayland --drm --socket wayland-0 --no-lockscreen"},
		{"user", "kwin", "dbus-run-session"},
		{"user", "plasma", "dbus-run-session"},
		{"weston", "weston", "weston --backend=drm-backend.so"},
	}
	for _, c := range cases {
		_, choice := chooseUnit(c.backend, c.mode)
		got := execStartFor(choice, "")
		if !strings.Contains(got, c.mustHave) {
			t.Errorf("execStartFor(%s/%s): got %q want substring %q",
				c.backend, c.mode, got, c.mustHave)
		}
	}
}

func TestExecStartFor_PatchedRuntimeBinOverridesKwin(t *testing.T) {
	_, choice := chooseUnit("realvt", "kwin")
	got := execStartFor(choice, "/opt/clouddeploy-kwin/bin/kwin_wayland")
	if !strings.Contains(got, "/opt/clouddeploy-kwin/bin/kwin_wayland") {
		t.Errorf("patched runtime bin should be used; got %q", got)
	}
}

// -----------------------------------------------------------------------------
// happy path: full strict success criteria
// -----------------------------------------------------------------------------

func TestKWinSession_HappyPath_RealVTKWin(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "kwin-realvt.service")

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

	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !strings.Contains(string(body), "TTYPath=/dev/tty7") {
		t.Errorf("real-VT unit body missing TTYPath=/dev/tty7:\n%s", string(body))
	}
	if !strings.Contains(string(body), "kwin_wayland --drm --socket wayland-0 --no-lockscreen") {
		t.Errorf("real-VT unit body missing direct KWin launch:\n%s", string(body))
	}
	if strings.Contains(string(body), "Environment=WAYLAND_DISPLAY=wayland-0") {
		t.Errorf("real-VT unit must not set WAYLAND_DISPLAY before launching KWin:\n%s", string(body))
	}

	wantSteps := [][]string{
		{"daemon-reload"},
		{"enable", "kwin-realvt.service"},
		{"start", "--no-block", "kwin-realvt.service"},
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
	if d["service_name"] != "kwin-realvt.service" {
		t.Errorf("service_name: got %v want kwin-realvt.service", d["service_name"])
	}
	if d["backend"] != "realvt" {
		t.Errorf("backend: got %v want realvt", d["backend"])
	}
	if d["compositor_mode"] != "kwin" {
		t.Errorf("compositor_mode: got %v want kwin", d["compositor_mode"])
	}
	if d["vt"] != 7 {
		t.Errorf("vt: got %v want 7", d["vt"])
	}
	// selected_drm_device is the RESOLVED value, not the raw profile
	// field. On this test host there's no /sys/class/drm tree, so
	// the resolver falls through to "fallback final" => card0.
	if got, _ := d["selected_drm_device"].(string); !strings.HasSuffix(got, "card0") && !strings.HasSuffix(got, "card1") {
		t.Errorf("selected_drm_device: got %v want a card path", d["selected_drm_device"])
	}
	if d["kwin_pid"] != 42 {
		t.Errorf("kwin_pid: got %v want 42", d["kwin_pid"])
	}
	if d["dbus_kwin_ok"] != true {
		t.Errorf("dbus_kwin_ok: got %v want true", d["dbus_kwin_ok"])
	}
	if d["support_information_drm_backend"] != true {
		t.Errorf("support_information_drm_backend: got %v want true", d["support_information_drm_backend"])
	}
}

// -----------------------------------------------------------------------------
// strict success criteria: each new gate gets a test
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

func TestKWinSession_FatalJournalSignatureFailsFatal(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.JournalRecentFn = func(context.Context, *Deps, string, int) (string, error) {
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

func TestKWinSession_FailsWhenSystemctlStartFails(t *testing.T) {
	deps := kwinDeps(t)
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SystemctlFn = func(_ context.Context, _ *Deps, args ...string) error {
		if len(args) > 0 && args[0] == "start" {
			return errors.New("simulated systemctl start failure")
		}
		return nil
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when start fails")
	}
	d := deps.State.Get(KWinSessionName).Details
	if d["failed_step"] != "start" {
		t.Errorf("failed_step: got %v want start", d["failed_step"])
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

// -----------------------------------------------------------------------------
// patched-KWin private HDR env is gated by kwin_patch=done
// -----------------------------------------------------------------------------

func TestKWinSession_PrivateHDREnv_OnlyWhenKwinPatchDone(t *testing.T) {
	deps := kwinDeps(t)
	deps.Profile.KWin.PatchedHDR = true

	// kwin_patch absent => env not injected.
	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run (no kwin_patch): %v", err)
	}
	body, _ := os.ReadFile(unitPath)
	if strings.Contains(string(body), "KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1") {
		t.Errorf("private HDR env injected without kwin_patch=done:\n%s", string(body))
	}

	// kwin_patch done => env injected on the next run.
	deps2 := kwinDeps(t)
	deps2.Profile.KWin.PatchedHDR = true
	deps2.State.MarkDone(KWinPatchName, map[string]any{
		"patched_hdr":  true,
		"install_mode": "packages",
	})
	unitPath2 := filepath.Join(t.TempDir(), "u.service")
	ph2 := happyKwin(unitPath2)
	if err := ph2.Run(context.Background(), deps2); err != nil {
		t.Fatalf("Run (kwin_patch done): %v", err)
	}
	body2, _ := os.ReadFile(unitPath2)
	if !strings.Contains(string(body2), "KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR=1") {
		t.Errorf("private HDR env should be injected when kwin_patch=done:\n%s", string(body2))
	}
}

// -----------------------------------------------------------------------------
// force-kwin-mode helper write
// -----------------------------------------------------------------------------

func TestKWinSession_WritesForceKwinModeHelper(t *testing.T) {
	deps := kwinDeps(t)
	deps.Profile.Display.ForcedConnector = "DP-1"
	deps.Profile.Display.Resolution = "3840x2160"
	deps.Profile.Display.Refresh = 120

	unitPath := filepath.Join(t.TempDir(), "u.service")
	helperPath := filepath.Join(t.TempDir(), "clouddeploy-force-kwin-mode.sh")
	ph := happyKwin(unitPath)
	ph.HelperPath = helperPath

	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"DP-1",
		"3840x2160@120",
		"kscreen-doctor",
		"QT_QPA_PLATFORM=wayland",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("helper missing %q:\n%s", want, string(body))
		}
	}
	d := deps.State.Get(KWinSessionName).Details
	if d["force_kwin_helper"] != helperPath {
		t.Errorf("force_kwin_helper detail: got %v want %q", d["force_kwin_helper"], helperPath)
	}
}

// -----------------------------------------------------------------------------
// scanFatalSignatures pinning
// -----------------------------------------------------------------------------

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

// -----------------------------------------------------------------------------
// DRM resolver (helper) + resolver integration with Run
// -----------------------------------------------------------------------------

func setupResolverFS(t *testing.T) (sysfs, devdri string) {
	t.Helper()
	tempDir := t.TempDir()
	sysfs = filepath.Join(tempDir, "sys")
	devdri = filepath.Join(tempDir, "dev", "dri")
	if err := os.MkdirAll(sysfs, 0o755); err != nil {
		t.Fatalf("mkdir sysfs: %v", err)
	}
	if err := os.MkdirAll(devdri, 0o755); err != nil {
		t.Fatalf("mkdir devdri: %v", err)
	}
	for _, d := range []string{"card0", "card1", "card2"} {
		if err := os.MkdirAll(filepath.Join(sysfs, "class/drm", d, "device"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return sysfs, devdri
}

func TestKWinSession_resolveDRMDevice_ExplicitWins(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	s := KWinSession{SysfsRoot: sysfs, DevDriRoot: devdri}
	res, reason := s.resolveDRMDevice("/dev/dri/card2", true, "")
	if res != "/dev/dri/card2" {
		t.Errorf("explicit request: got %q want /dev/dri/card2", res)
	}
	if !strings.Contains(reason, "explicitly") {
		t.Errorf("explicit reason: got %q", reason)
	}
}

func TestKWinSession_resolveDRMDevice_DefaultedNotExplicit(t *testing.T) {
	// Live-VM regression: EffectiveDesktop fills empty kwin_drm_device
	// with /dev/dri/card1, so explicitness MUST be carried separately.
	// With explicit=false the resolver MUST NOT short-circuit even if
	// the requested path matches the default.
	sysfs, devdri := setupResolverFS(t)
	// Mark card0 as the DP-1 owner so the resolver picks it.
	if err := os.MkdirAll(filepath.Join(sysfs, "class/drm/card0/card0-DP-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := KWinSession{SysfsRoot: sysfs, DevDriRoot: devdri}
	res, reason := s.resolveDRMDevice("/dev/dri/card1", false, "DP-1")
	if res != filepath.Join(devdri, "card0") {
		t.Errorf("non-explicit + DP-1 on card0: got %q want card0", res)
	}
	if !strings.Contains(reason, "matched forced_connector") {
		t.Errorf("reason should explain match: %q", reason)
	}
}

func TestKWinSession_resolveDRMDevice_DP1OnCard1(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	if err := os.MkdirAll(filepath.Join(sysfs, "class/drm/card1/card1-DP-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := KWinSession{SysfsRoot: sysfs, DevDriRoot: devdri}
	res, _ := s.resolveDRMDevice("", false, "DP-1")
	if res != filepath.Join(devdri, "card1") {
		t.Errorf("DP-1 on card1: got %q want card1", res)
	}
}

func TestKWinSession_resolveDRMDevice_NVIDIAVendorFallback(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	if err := os.WriteFile(filepath.Join(sysfs, "class/drm/card0/device/vendor"),
		[]byte("0x10de\n"), 0o644); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	s := KWinSession{SysfsRoot: sysfs, DevDriRoot: devdri}
	res, reason := s.resolveDRMDevice("", false, "")
	if res != filepath.Join(devdri, "card0") {
		t.Errorf("NVIDIA vendor on card0: got %q want card0", res)
	}
	if !strings.Contains(reason, "nvidia") {
		t.Errorf("reason should mention nvidia: %q", reason)
	}
}

func TestKWinSession_resolveDRMDevice_FallbackFinalCard0(t *testing.T) {
	tempDir := t.TempDir()
	sysfs := filepath.Join(tempDir, "sys")
	devdri := filepath.Join(tempDir, "dev", "dri")
	if err := os.MkdirAll(sysfs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(devdri, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No /sys/class/drm tree, no /dev/dri/card1 -> "fallback final".
	s := KWinSession{SysfsRoot: sysfs, DevDriRoot: devdri}
	res, reason := s.resolveDRMDevice("", false, "")
	if res != filepath.Join(devdri, "card0") {
		t.Errorf("fallback: got %q want card0", res)
	}
	if !strings.Contains(reason, "fallback final") {
		t.Errorf("reason should explain final fallback: %q", reason)
	}
}

// -----------------------------------------------------------------------------
// integration: resolver flows through Run -> rendered unit
// -----------------------------------------------------------------------------

func TestKWinSession_Run_UsesResolverForKwinDRMDevices_DP1OnCard0(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	if err := os.MkdirAll(filepath.Join(sysfs, "class/drm/card0/card0-DP-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deps := kwinDeps(t)
	deps.Profile.Display.ForcedConnector = "DP-1"
	// Leave Desktop.KwinDRMDevice empty so EffectiveDesktop defaults
	// it to /dev/dri/card1, which the resolver should override to
	// card0 based on the DP-1 sysfs match.
	deps.Profile.Desktop.KwinDRMDevice = ""

	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SysfsRoot = sysfs
	ph.DevDriRoot = devdri

	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(unitPath)
	want := "KWIN_DRM_DEVICES=" + filepath.Join(devdri, "card0")
	if !strings.Contains(string(body), want) {
		t.Errorf("rendered unit should contain %q; got:\n%s", want, string(body))
	}
	d := deps.State.Get(KWinSessionName).Details
	if d["kwin_drm_device_explicit"] != false {
		t.Errorf("kwin_drm_device_explicit: got %v want false", d["kwin_drm_device_explicit"])
	}
	if got, _ := d["kwin_drm_device_resolved"].(string); !strings.HasSuffix(got, "card0") {
		t.Errorf("kwin_drm_device_resolved: got %v want a card0 path", d["kwin_drm_device_resolved"])
	}
	if got, _ := d["kwin_drm_device_reason"].(string); !strings.Contains(got, "DP-1") {
		t.Errorf("kwin_drm_device_reason: got %v want a DP-1 match", d["kwin_drm_device_reason"])
	}
}

func TestKWinSession_Run_UsesResolverForKwinDRMDevices_DP1OnCard1(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	if err := os.MkdirAll(filepath.Join(sysfs, "class/drm/card1/card1-DP-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deps := kwinDeps(t)
	deps.Profile.Display.ForcedConnector = "DP-1"
	deps.Profile.Desktop.KwinDRMDevice = ""

	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SysfsRoot = sysfs
	ph.DevDriRoot = devdri

	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(unitPath)
	want := "KWIN_DRM_DEVICES=" + filepath.Join(devdri, "card1")
	if !strings.Contains(string(body), want) {
		t.Errorf("rendered unit should contain %q; got:\n%s", want, string(body))
	}
}

func TestKWinSession_Run_NVIDIAVendorFallbackInUnit(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	// No DP-1 nodes anywhere; NVIDIA vendor exposed on card0.
	if err := os.WriteFile(filepath.Join(sysfs, "class/drm/card0/device/vendor"),
		[]byte("0x10de\n"), 0o644); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	deps := kwinDeps(t)
	deps.Profile.Display.ForcedConnector = "DP-1"
	deps.Profile.Desktop.KwinDRMDevice = ""

	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SysfsRoot = sysfs
	ph.DevDriRoot = devdri

	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(unitPath)
	want := "KWIN_DRM_DEVICES=" + filepath.Join(devdri, "card0")
	if !strings.Contains(string(body), want) {
		t.Errorf("rendered unit should contain %q (NVIDIA vendor fallback); got:\n%s", want, string(body))
	}
}

func TestKWinSession_Run_ExplicitProfileOverrideBypassesResolver(t *testing.T) {
	sysfs, devdri := setupResolverFS(t)
	// DP-1 sysfs would otherwise route the resolver to card0.
	if err := os.MkdirAll(filepath.Join(sysfs, "class/drm/card0/card0-DP-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deps := kwinDeps(t)
	deps.Profile.Display.ForcedConnector = "DP-1"
	// Operator explicitly pins /dev/dri/card2 - resolver MUST honor it.
	deps.Profile.Desktop.KwinDRMDevice = "/dev/dri/card2"

	unitPath := filepath.Join(t.TempDir(), "u.service")
	ph := happyKwin(unitPath)
	ph.SysfsRoot = sysfs
	ph.DevDriRoot = devdri

	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := os.ReadFile(unitPath)
	if !strings.Contains(string(body), "KWIN_DRM_DEVICES=/dev/dri/card2") {
		t.Errorf("rendered unit should contain explicit override KWIN_DRM_DEVICES=/dev/dri/card2; got:\n%s", string(body))
	}
	d := deps.State.Get(KWinSessionName).Details
	if d["kwin_drm_device_explicit"] != true {
		t.Errorf("kwin_drm_device_explicit: got %v want true", d["kwin_drm_device_explicit"])
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
