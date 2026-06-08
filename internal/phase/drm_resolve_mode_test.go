package phase

import "testing"

func TestResolveMode_ExactWins(t *testing.T) {
	c := KScreenConnector{Modes: []string{"3840x2160@60", "3840x2160@120"}}
	got, ok := c.ResolveMode("3840x2160@120")
	if !ok || got != "3840x2160@120" {
		t.Fatalf("exact match: got %q ok=%v want 3840x2160@120 true", got, ok)
	}
}

func TestResolveMode_ApproxCVTRefresh(t *testing.T) {
	// kscreen reports CVT reduced-blanking modes a hair under nominal.
	c := KScreenConnector{Modes: []string{"2560x1440@59.95", "2560x1440@119.997", "1920x1200@119.98"}}
	cases := []struct {
		want string
		out  string
	}{
		{"2560x1440@60", "2560x1440@59.95"},
		{"2560x1440@120", "2560x1440@119.997"},
		{"1920x1200@120", "1920x1200@119.98"},
	}
	for _, tc := range cases {
		got, ok := c.ResolveMode(tc.want)
		if !ok || got != tc.out {
			t.Errorf("ResolveMode(%q)=%q,%v want %q,true", tc.want, got, ok, tc.out)
		}
	}
}

func TestResolveMode_TruncatedIntegerRefresh(t *testing.T) {
	// Some kscreen builds truncate 119.997 -> "119".
	c := KScreenConnector{Modes: []string{"2560x1440@119"}}
	if got, ok := c.ResolveMode("2560x1440@120"); !ok || got != "2560x1440@119" {
		t.Errorf("truncated refresh: got %q,%v want 2560x1440@119,true", got, ok)
	}
}

func TestResolveMode_RejectsWrongResolutionAndFarRefresh(t *testing.T) {
	c := KScreenConnector{Modes: []string{"2560x1440@60", "1920x1080@144"}}
	if _, ok := c.ResolveMode("1920x1080@60"); ok {
		t.Errorf("1920x1080@60 should not match a 144Hz-only entry (refresh too far)")
	}
	if _, ok := c.ResolveMode("3840x2160@60"); ok {
		t.Errorf("3840x2160@60 should not match a different resolution")
	}
}

func TestModeResolvable_NilSafe(t *testing.T) {
	if modeResolvable(nil, "3840x2160@120") {
		t.Errorf("nil connector should not resolve any mode")
	}
}

func TestModeListResolvable(t *testing.T) {
	modes := []string{"1280x720@59.95", "1920x1080@120"}
	if !modeListResolvable(modes, "1280x720@60") {
		t.Errorf("1280x720@60 should resolve to the 59.95 entry")
	}
	if modeListResolvable(modes, "2560x1440@120") {
		t.Errorf("2560x1440@120 is absent and should not resolve")
	}
}
