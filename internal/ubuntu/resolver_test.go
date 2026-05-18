package ubuntu

import (
	"strings"
	"testing"
)

func TestParseOSSelectionPolicy(t *testing.T) {
	cases := map[string]OSSelectionPolicy{
		"":                  OSPolicyLatestCompatible,
		"latest-compatible": OSPolicyLatestCompatible,
		"exact":             OSPolicyExact,
		"min-version":       OSPolicyMinVersion,
		"any":               OSPolicyAny,
	}
	for in, want := range cases {
		got, err := ParseOSSelectionPolicy(in)
		if err != nil {
			t.Errorf("ParseOSSelectionPolicy(%q): err=%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseOSSelectionPolicy(%q): got %q want %q", in, got, want)
		}
	}
	if _, err := ParseOSSelectionPolicy("zoinks"); err == nil {
		t.Error("ParseOSSelectionPolicy should reject unknown value")
	}
}

func TestResolveOSTarget_LatestCompatible_PicksFirstSupported(t *testing.T) {
	// 26.04 has a placeholder codename but is NOT in the v3 supported
	// list yet; the resolver must skip it and select 25.10.
	res := ResolveOSTarget(OSResolverInputs{
		CurrentVersion: "24.04",
		Candidates:     []string{"26.04", "25.10", "24.04"},
		Policy:         OSPolicyLatestCompatible,
		AcceptNonLTS:   true,
	})
	if res.Selected != "25.10" {
		t.Errorf("Selected: got %q want 25.10 (26.04 not validated yet)", res.Selected)
	}
	if res.SelectedCodename != "questing" {
		t.Errorf("SelectedCodename: got %q want questing", res.SelectedCodename)
	}
	if len(res.Tried) < 2 {
		t.Errorf("Tried should record both 26.04 and 25.10; got %v", res.Tried)
	}
	if r := res.RejectedReasons()["26.04"]; !strings.Contains(r, "supported list") {
		t.Errorf("RejectedReasons[26.04] should explain unsupported; got %q", r)
	}
}

func TestResolveOSTarget_LatestCompatible_FallsThroughToLTS(t *testing.T) {
	// AcceptNonLTS=false means 25.10 is rejected. 26.04 not supported.
	// Resolver should fall through to 24.04 (LTS + supported).
	res := ResolveOSTarget(OSResolverInputs{
		CurrentVersion: "24.04",
		Candidates:     []string{"26.04", "25.10", "24.04"},
		Policy:         OSPolicyLatestCompatible,
		AcceptNonLTS:   false,
	})
	if res.Selected != "24.04" {
		t.Errorf("Selected: got %q want 24.04 (LTS fallback)", res.Selected)
	}
	if r := res.RejectedReasons()["25.10"]; !strings.Contains(r, "non-LTS") {
		t.Errorf("RejectedReasons[25.10] should explain non-LTS gate; got %q", r)
	}
}

func TestResolveOSTarget_MinVersionFiltersOlder(t *testing.T) {
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"24.04", "25.10"},
		Policy:       OSPolicyMinVersion,
		MinVersion:   "25.10",
		AcceptNonLTS: true,
	})
	if res.Selected != "25.10" {
		t.Errorf("Selected: got %q want 25.10 (min_version filter)", res.Selected)
	}
	if r := res.RejectedReasons()["24.04"]; !strings.Contains(r, "below min_version") {
		t.Errorf("RejectedReasons[24.04] should explain min_version filter; got %q", r)
	}
}

func TestResolveOSTarget_Exact_RequiresExactlyOne(t *testing.T) {
	// Two-entry candidates list with policy=exact must reject.
	res := ResolveOSTarget(OSResolverInputs{
		Candidates: []string{"24.04", "25.10"},
		Policy:     OSPolicyExact,
	})
	if res.Selected != "" {
		t.Errorf("policy=exact with 2 candidates should reject; got Selected=%q", res.Selected)
	}
}

func TestResolveOSTarget_Exact_SingleCandidatePasses(t *testing.T) {
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"25.10"},
		Policy:       OSPolicyExact,
		AcceptNonLTS: true,
	})
	if res.Selected != "25.10" {
		t.Errorf("policy=exact single candidate should select it; got %q", res.Selected)
	}
}

func TestResolveOSTarget_Any_AcceptsUnsupported(t *testing.T) {
	// policy=any skips the supported-list gate.
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"26.04"},
		Policy:       OSPolicyAny,
		AcceptNonLTS: true,
	})
	if res.Selected != "26.04" {
		t.Errorf("policy=any should accept unsupported 26.04; got %q", res.Selected)
	}
}

func TestResolveOSTarget_ProbeOverridesGates(t *testing.T) {
	// Probe rejects 25.10 even though it would otherwise pass.
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"25.10", "24.04"},
		Policy:       OSPolicyLatestCompatible,
		AcceptNonLTS: true,
		Probe: func(v string) (bool, string) {
			if v == "25.10" {
				return false, "nvidia-cuda-toolkit not yet built for questing"
			}
			return true, ""
		},
	})
	if res.Selected != "24.04" {
		t.Errorf("probe should reject 25.10; expected 24.04, got %q", res.Selected)
	}
	if r := res.RejectedReasons()["25.10"]; !strings.Contains(r, "package probe rejected") {
		t.Errorf("RejectedReasons should explain probe rejection; got %q", r)
	}
}

func TestResolveOSTarget_NoCandidatesReturnsEmpty(t *testing.T) {
	res := ResolveOSTarget(OSResolverInputs{})
	if res.Selected != "" {
		t.Errorf("empty candidates: got Selected=%q want empty", res.Selected)
	}
}

func TestResolveOSTarget_AllRejectedReturnsEmpty(t *testing.T) {
	// 26.04 (unsupported) + 25.10 with non-LTS gate off.
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"26.04", "25.10"},
		Policy:       OSPolicyLatestCompatible,
		AcceptNonLTS: false,
	})
	if res.Selected != "" {
		t.Errorf("all-rejected: got Selected=%q want empty", res.Selected)
	}
	if len(res.Tried) != 2 {
		t.Errorf("Tried should record both rejections; got %v", res.Tried)
	}
}

func TestResolveOSTarget_UnknownCodenameRejected(t *testing.T) {
	res := ResolveOSTarget(OSResolverInputs{
		Candidates:   []string{"99.99", "24.04"},
		Policy:       OSPolicyLatestCompatible,
		AcceptNonLTS: true,
	})
	if res.Selected != "24.04" {
		t.Errorf("Selected: got %q want 24.04 (99.99 unknown)", res.Selected)
	}
	if r := res.RejectedReasons()["99.99"]; !strings.Contains(r, "unknown") {
		t.Errorf("RejectedReasons[99.99] should explain unknown codename; got %q", r)
	}
}

func TestCompareUbuntuVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"24.04", "25.10", -1},
		{"25.10", "24.04", 1},
		{"25.10", "25.10", 0},
		{"26.04", "25.10", 1},
		{"24.04", "26.04", -1},
	}
	for _, c := range cases {
		if got := compareUbuntuVersion(c.a, c.b); got != c.want {
			t.Errorf("compareUbuntuVersion(%q,%q): got %d want %d", c.a, c.b, got, c.want)
		}
	}
}
