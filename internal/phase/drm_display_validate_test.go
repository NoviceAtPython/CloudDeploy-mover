package phase

import (
	"context"
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
