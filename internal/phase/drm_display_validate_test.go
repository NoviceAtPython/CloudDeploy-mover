package phase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// -----------------------------------------------------------------------------
// parser
// -----------------------------------------------------------------------------

func TestParseKScreenDoctor_MinimalDP1HappyPath(t *testing.T) {
	out := ParseKScreenDoctor(`Output: 1 DP-1
        enabled
        Modes: 1!  3840x2160@120, 2  1920x1080@60
`)
	if len(out.Connectors) != 1 {
		t.Fatalf("connector count: got %d want 1 (%v)", len(out.Connectors), out.Connectors)
	}
	c := out.Connectors[0]
	if c.Name != "DP-1" {
		t.Errorf("name: got %q want DP-1", c.Name)
	}
	if !c.Enabled {
		t.Errorf("enabled: got false want true")
	}
	if c.CurrentMode != "3840x2160@120" {
		t.Errorf("current mode: got %q want 3840x2160@120", c.CurrentMode)
	}
	if len(c.Modes) != 2 {
		t.Fatalf("modes: got %v", c.Modes)
	}
	if !c.HasMode("3840x2160@120") {
		t.Errorf("HasMode(3840x2160@120): got false")
	}
	if !c.HasMode("1920x1080@60") {
		t.Errorf("HasMode(1920x1080@60): got false")
	}
}

func TestParseKScreenDoctor_LivePlasma645Modes(t *testing.T) {
	out := ParseKScreenDoctor(`Output: 1 DP-1
        enabled
        Modes:  1:3840x2160@60!  2:3840x2160@120*  3:3840x2160@60
        HDR: enabled
        Wide Color Gamut: enabled
`)
	c := out.FindConnector("DP-1")
	if c == nil {
		t.Fatalf("DP-1 not parsed: %#v", out.Connectors)
	}
	if got, want := c.CurrentMode, "3840x2160@120"; got != want {
		t.Fatalf("current mode: got %q want %q", got, want)
	}
	if !c.HDR || !c.WCG {
		t.Fatalf("HDR/WCG: got hdr=%v wcg=%v want both true", c.HDR, c.WCG)
	}
	wantModes := []string{"3840x2160@60", "3840x2160@120", "3840x2160@60"}
	if len(c.Modes) != len(wantModes) {
		t.Fatalf("modes: got %v want %v", c.Modes, wantModes)
	}
	for i, want := range wantModes {
		if c.Modes[i] != want {
			t.Errorf("mode[%d]: got %q want %q", i, c.Modes[i], want)
		}
	}
}

// TestParseKScreenDoctor_StripsANSIEscapes is the live-VM regression
// test. Plasma 6.4.x kscreen-doctor emits ANSI color escapes even on
// a pipe; the parser must strip them before matching. With the bug
// the test would fail with `saw []` even though "DP-1" + "HDR:
// enabled" + "Wide Color Gamut: enabled" + 3840x2160@120 are all
// present.
func TestParseKScreenDoctor_StripsANSIEscapes(t *testing.T) {
	raw := "\x1b[01;34mOutput:\x1b[0;0m 1 DP-1 hdmi enabled connected priority 1 1\n" +
		"        Modes: \x1b[01;33m1:3840x2160@60!\x1b[0;0m \x1b[01;33m2:\x1b[01;32m3840x2160@120*\x1b[0;0m\n" +
		"        \x1b[01;33mHDR:\x1b[0;0m enabled\n" +
		"        \x1b[01;33mWide Color Gamut:\x1b[0;0m enabled\n"
	out := ParseKScreenDoctor(raw)
	if len(out.Connectors) != 1 {
		t.Fatalf("got %d connectors, want 1: %#v", len(out.Connectors), out.Connectors)
	}
	c := out.Connectors[0]
	if c.Name != "DP-1" {
		t.Errorf("connector name: got %q want DP-1", c.Name)
	}
	if !c.Enabled {
		t.Errorf("enabled (from inline Output:-header token): got false, want true")
	}
	if !c.HasMode("3840x2160@120") {
		t.Errorf("modes must contain 3840x2160@120; got %v", c.Modes)
	}
	if c.CurrentMode != "3840x2160@120" {
		t.Errorf("current mode: got %q want 3840x2160@120 (the '*' marker)", c.CurrentMode)
	}
	if !c.HDR {
		t.Errorf("HDR: got false, want true")
	}
	if !c.WCG {
		t.Errorf("WCG: got false, want true")
	}
}

func TestStripANSI_CommonEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[01;34mOutput:\x1b[0;0m DP-1", "Output: DP-1"},
		{"\x1b[0mreset only", "reset only"},
		{"a\x1b[31mbb\x1b[0mc", "abbc"},
		{"", ""},
		// Sanity: no ESC in input => StripANSI is a no-op fast path.
		{"no escapes at all", "no escapes at all"},
	}
	for _, c := range cases {
		got := StripANSI(c.in)
		if got != c.want {
			t.Errorf("StripANSI(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestParseKScreenDoctor_MultipleConnectors(t *testing.T) {
	out := ParseKScreenDoctor(`Output: 1 DP-1
        enabled
        Modes: 1!  3840x2160@120
Output: 2 HDMI-A-1
        disabled
        Modes: 1  1920x1080@60
`)
	if len(out.Connectors) != 2 {
		t.Fatalf("connector count: got %d want 2", len(out.Connectors))
	}
	if c := out.FindConnector("DP-1"); c == nil || !c.Enabled {
		t.Errorf("DP-1 must be enabled; got %v", c)
	}
	if c := out.FindConnector("HDMI-A-1"); c == nil || c.Enabled {
		t.Errorf("HDMI-A-1 must be disabled; got %v", c)
	}
}

func TestParseResolution(t *testing.T) {
	cases := []struct {
		in   string
		w, h int
	}{
		{"3840x2160", 3840, 2160},
		{" 1920x1080 ", 1920, 1080},
		{"bad", 0, 0},
		{"", 0, 0},
		{"3840xfoo", 0, 0},
	}
	for _, c := range cases {
		w, h := ParseResolution(c.in)
		if w != c.w || h != c.h {
			t.Errorf("ParseResolution(%q): got (%d,%d) want (%d,%d)", c.in, w, h, c.w, c.h)
		}
	}
}

func TestFormatMode(t *testing.T) {
	if got := FormatMode(3840, 2160, 120); got != "3840x2160@120" {
		t.Errorf("got %q want 3840x2160@120", got)
	}
	if got := FormatMode(0, 2160, 120); got != "" {
		t.Errorf("invalid width: got %q want empty", got)
	}
}

// -----------------------------------------------------------------------------
// phase
// -----------------------------------------------------------------------------

func drmDeps(t *testing.T) *Deps {
	t.Helper()
	deps := newDeps(t, runtimeProfile(), nil)
	deps.State.MarkDone(HeadlessUserName, map[string]any{
		"user": "cloudgamer", "uid": "1001",
	})
	return deps
}

func writeDRMConnector(t *testing.T, root, card, connector, status, enabled string, modes []string) {
	t.Helper()
	drmRoot := filepath.Join(root, "class", "drm")
	connDir := filepath.Join(drmRoot, card+"-"+connector)
	cardDeviceDir := filepath.Join(drmRoot, card, "device")
	if err := os.MkdirAll(connDir, 0o755); err != nil {
		t.Fatalf("mkdir connector: %v", err)
	}
	// We want to test symlinks, so instead of creating a directory if test wants to mock that
	// wait, testing symlinks in Go Windows can be hard because it needs elevated privileges.
	// But it's fine, the actual parsing is tested simply by ensuring formatting or checking IsDir.
	if err := os.MkdirAll(cardDeviceDir, 0o755); err != nil {
		t.Fatalf("mkdir card device: %v", err)
	}
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	mustWrite(filepath.Join(connDir, "status"), status+"\n")
	mustWrite(filepath.Join(connDir, "enabled"), enabled+"\n")
	mustWrite(filepath.Join(connDir, "modes"), strings.Join(modes, "\n")+"\n")
	mustWrite(filepath.Join(cardDeviceDir, "vendor"), "0x10de\n")
}

func TestDRMDisplayValidate_HappyPath(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(_ context.Context, _ *Deps, user, uid string) (string, error) {
			if user != "cloudgamer" || uid != "1001" {
				t.Errorf("kscreen called with user=%q uid=%q", user, uid)
			}
			return `Output: 1 DP-1
        enabled
        Modes: 1!  3840x2160@120, 2  1920x1080@60
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(DRMDisplayValidateName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["connector"] != "DP-1" {
		t.Errorf("connector: got %v want DP-1", d["connector"])
	}
	if d["enabled"] != true {
		t.Errorf("enabled: got %v want true", d["enabled"])
	}
	if d["selected_mode"] != "3840x2160@120" {
		t.Errorf("selected_mode: got %v want 3840x2160@120", d["selected_mode"])
	}
	if d["expected_mode"] != "3840x2160@120" {
		t.Errorf("expected_mode: got %v want 3840x2160@120", d["expected_mode"])
	}
}

func TestDRMDisplayValidate_ConnectorMissingFailsFatal(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 HDMI-A-1
        enabled
        Modes: 1!  1920x1080@60
`, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when forced connector is absent")
	}
	if !strings.Contains(err.Error(), "DP-1") {
		t.Errorf("error should mention DP-1: %v", err)
	}
	if got := deps.State.Get(DRMDisplayValidateName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestDRMDisplayValidate_ConnectorDisabledFailsFatal(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        disabled
        Modes: 1!  3840x2160@120
`, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when forced connector is disabled")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error should mention disabled state: %v", err)
	}
}

func TestDRMDisplayValidate_KScreenDisabledSysfsEnabledNoUsabilityProofFails(t *testing.T) {
	deps := drmDeps(t)
	sysfs := t.TempDir()
	writeDRMConnector(t, sysfs, "card0", "DP-1", "connected", "enabled", []string{"3840x2160"})
	ph := DRMDisplayValidate{
		SysfsRoot: sysfs,
		WaylandSocketFn: func(string) bool {
			return true
		},
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        disabled
        Modes: 1:3840x2160@60 2:3840x2160@120
`, nil
		},
		KScreenApplyFn: func(context.Context, *Deps, string, string, []string) (string, error) {
			return "applying config failed! The driver rejected the output configuration", errors.New("rejected")
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when KScreen disabled and sysfs proof is unverified")
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["drm_error_category"] != DRMFailSysfsEnabledKScreenDisabled {
		t.Fatalf("drm_error_category: got %v want %s", d["drm_error_category"], DRMFailSysfsEnabledKScreenDisabled)
	}
	if d["kscreen_rejected_output_config"] != true {
		t.Fatalf("expected rejected-output repair diagnostic: %v", d)
	}
}

func TestDRMDisplayValidate_KScreenDisabledSysfsEnabledWithUsabilityProofPasses(t *testing.T) {
	deps := drmDeps(t)
	deps.DryRun = false
	sysfs := t.TempDir()
	writeDRMConnector(t, sysfs, "card0", "DP-1", "connected", "enabled", []string{"3840x2160"})
	ph := DRMDisplayValidate{
		SysfsRoot: sysfs,
		KernelLogFn: func(context.Context, *Deps) string {
			return ""
		},
		WaylandSocketFn: func(string) bool {
			return true
		},
		KWinDBusFn: func(context.Context, *Deps, string, string) (bool, string, error) {
			return true, "introspect OK", nil
		},
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        disabled
        Modes: 1:3840x2160@60 2:3840x2160@120
`, nil
		},
		KScreenApplyFn: func(context.Context, *Deps, string, string, []string) (string, error) {
			return "applying config failed! The driver rejected the output configuration", errors.New("rejected")
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("expected mixed sysfs+wayland proof to pass: %v", err)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["verification_method"] != DRMVerifiedSysfsWayland {
		t.Fatalf("verification_method: got %v want %s", d["verification_method"], DRMVerifiedSysfsWayland)
	}
	if d["hdr_wcg_unverified_warning"] != true {
		t.Fatalf("HDR profile should record unverified HDR/WCG warning under mixed-source proof: %v", d)
	}
}

func TestDRMDisplayValidate_KScreenDisabledSysfsDisabledFails(t *testing.T) {
	deps := drmDeps(t)
	deps.DryRun = false
	sysfs := t.TempDir()
	writeDRMConnector(t, sysfs, "card0", "DP-1", "connected", "disabled", []string{"3840x2160"})
	ph := DRMDisplayValidate{
		SysfsRoot:       sysfs,
		KernelLogFn:     func(context.Context, *Deps) string { return "" },
		WaylandSocketFn: func(string) bool { return true },
		KWinDBusFn: func(context.Context, *Deps, string, string) (bool, string, error) {
			return true, "introspect OK", nil
		},
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        disabled
        Modes: 1:3840x2160@60 2:3840x2160@120
`, nil
		},
		KScreenApplyFn: func(context.Context, *Deps, string, string, []string) (string, error) {
			return "applying config failed! The driver rejected the output configuration", errors.New("rejected")
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when both KScreen and sysfs report disabled")
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["drm_error_category"] != DRMFailKScreenSysfsStateMismatch {
		t.Fatalf("drm_error_category: got %v want %s", d["drm_error_category"], DRMFailKScreenSysfsStateMismatch)
	}
}

func TestDRMDisplayValidate_DiscoversConnectedDP2WhenNoForcedConnector(t *testing.T) {
	deps := drmDeps(t)
	deps.Profile.Display.ForcedConnector = ""
	deps.DryRun = false
	sysfs := t.TempDir()
	writeDRMConnector(t, sysfs, "card0", "DP-2", "connected", "enabled", []string{"3840x2160"})
	ph := DRMDisplayValidate{
		SysfsRoot:       sysfs,
		KernelLogFn:     func(context.Context, *Deps) string { return "" },
		WaylandSocketFn: func(string) bool { return true },
		KWinDBusFn: func(context.Context, *Deps, string, string) (bool, string, error) {
			return true, "introspect OK", nil
		},
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 2 DP-2
        enabled
        Modes: 1:3840x2160@120*
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("expected DP-2 discovery to pass: %v", err)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["selected_connector"] != "DP-2" {
		t.Fatalf("selected_connector: got %v want DP-2", d["selected_connector"])
	}
}

func TestDRMDisplayValidate_ExpectedModeMissingFailsFatal(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1!  1920x1080@60
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when expected mode missing")
	}
	if !strings.Contains(err.Error(), "expected mode") {
		t.Errorf("error should mention expected mode: %v", err)
	}
}

func TestDRMDisplayValidate_StrictHDR4K120UnavailableBut4K60AvailableFails(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1:3840x2160@60*
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected strict profile to fail when 4K120 is unavailable")
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["drm_error_category"] != DRMFailExpectedModeMissing {
		t.Fatalf("drm_error_category: got %v want %s", d["drm_error_category"], DRMFailExpectedModeMissing)
	}
}

func TestDRMDisplayValidate_FlexibleFallback4K60RecordsFallback(t *testing.T) {
	deps := drmDeps(t)
	deps.Profile.Display.AllowFallback = true
	deps.Profile.Display.AllowLowerRefreshFallback = true
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1:3840x2160@60*
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("expected flexible lower-refresh fallback to pass: %v", err)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["drm_error_category"] != DRMDisplayTargetFallbackApplied {
		t.Fatalf("drm_error_category: got %v want %s", d["drm_error_category"], DRMDisplayTargetFallbackApplied)
	}
	if d["display_target_effective"] != "hdr_3840x2160_60" {
		t.Fatalf("display_target_effective: got %v", d["display_target_effective"])
	}
}

func TestDRMDisplayValidate_HDRMissingFlexibleSDRRecordsFallback(t *testing.T) {
	deps := drmDeps(t)
	deps.Profile.Display.AllowFallback = true
	deps.Profile.Display.AllowSDRFallback = true
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1:3840x2160@120*
`, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("expected flexible SDR fallback to pass: %v", err)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["drm_error_category"] != DRMDisplayTargetFallbackApplied {
		t.Fatalf("drm_error_category: got %v want %s", d["drm_error_category"], DRMDisplayTargetFallbackApplied)
	}
	if d["display_target_effective"] != "sdr_3840x2160_120" {
		t.Fatalf("display_target_effective: got %v", d["display_target_effective"])
	}
}

func TestDRMDisplayValidate_RecordsNvidiaDumbAllocationWarning(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KernelLogFn: func(context.Context, *Deps) string {
			return "NVRM: Failed to allocate NvKmsKapiMemory for dumb object of size 33177600"
		},
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1:3840x2160@120*
        HDR: enabled
        Wide Color Gamut: enabled
`, nil
		},
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("warning alone should not fail happy output: %v", err)
	}
	d := deps.State.Get(DRMDisplayValidateName).Details
	if d["nvidia_dumb_create_alloc_warning"] != true {
		t.Fatalf("expected nvidia dumb allocation warning detail: %v", d)
	}
}

func TestDRMDisplayValidate_HDRMissingFailsFatal(t *testing.T) {
	deps := drmDeps(t)
	ph := DRMDisplayValidate{
		KScreenFn: func(context.Context, *Deps, string, string) (string, error) {
			return `Output: 1 DP-1
        enabled
        Modes: 1!  3840x2160@120
`, nil
		},
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when HDR/WCG are missing for HDR profile")
	}
	if !strings.Contains(err.Error(), "HDR profile") {
		t.Errorf("error should mention HDR profile: %v", err)
	}
}

func TestDRMDisplayValidate_NoUIDFailsFatal(t *testing.T) {
	deps := newDeps(t, runtimeProfile(), nil)
	// Intentionally no headless_user state.
	ph := DRMDisplayValidate{}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal when uid missing")
	}
}
func TestDiscoverSysfsConnectors_Normalization(t *testing.T) {
temp := t.TempDir()
p := DRMDisplayValidate{SysfsRoot: temp}

drmRoot := filepath.Join(temp, "class", "drm")
os.MkdirAll(filepath.Join(drmRoot, "card0-DP-1"), 0o755)
os.MkdirAll(filepath.Join(drmRoot, "card1-DP-2"), 0o755)
os.MkdirAll(filepath.Join(drmRoot, "card0-HDMI-A-1"), 0o755)
os.MkdirAll(filepath.Join(drmRoot, "card0-eDP-1"), 0o755)
os.MkdirAll(filepath.Join(drmRoot, "DP-1"), 0o755)

	connectors, _, _ := p.discoverSysfsConnectors()
find := func(sysfs string) *DRMConnectorState {
for i := range connectors {
if connectors[i].SysfsBasename == sysfs {
return &connectors[i]
}
}
return nil
}
cases := map[string]string{
"card0-DP-1": "DP-1",
"card1-DP-2": "DP-2",
"card0-HDMI-A-1": "HDMI-A-1",
"card0-eDP-1": "eDP-1",
}
for sysfs, wantConn := range cases {
c := find(sysfs)
if c == nil {
t.Errorf("discoverSysfsConnectors missed %q", sysfs)
continue
}
if c.Name != wantConn {
t.Errorf("sysfs %q normalized to %q, want %q", sysfs, c.Name, wantConn)
}
}
}

func TestFormatDiscoveredSysfs(t *testing.T) {
    list := []DRMConnectorState{
{
SysfsBasename: "card0-DP-1",
Name: "DP-1",
Status: "connected",
Enabled: "enabled",
Modes: []string{"3840x2160@120", "1920x1080@60", "800x600", "640x480"},
},
}
got := formatDiscoveredSysfs(list)
    want := "[card0-DP-1 normalized=DP-1 status=connected enabled=enabled modes=[3840x2160@120 1920x1080@60 800x600 ...]]"
if got != want {
t.Errorf("got %q, want %q", got, want)
}
}
func TestDiscoverSysfsConnectors_Empty(t *testing.T) {
temp := t.TempDir()
p := DRMDisplayValidate{SysfsRoot: temp}

drmRoot := filepath.Join(temp, "class", "drm")
os.MkdirAll(drmRoot, 0o755)

connectors, names, err := p.discoverSysfsConnectors()
if err != nil {
t.Fatalf("unexpected error: %v", err)
}
if connectors == nil {
t.Fatalf("expected empty slice, got nil connectors")
}
if names == nil {
t.Fatalf("expected empty slice, got nil names")
}
if len(connectors) != 0 || len(names) != 0 {
t.Fatalf("expected 0 connectors and names, got %d and %d", len(connectors), len(names))
}
}
