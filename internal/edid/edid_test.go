package edid

import (
	"strings"
	"testing"
)

func TestSelectFilename_UniversalHDR(t *testing.T) {
	// Every supported HDR mode resolves to the one universal HDR blob.
	for _, refresh := range []int{60, 120} {
		for _, res := range SupportedResolutions() {
			got := SelectFilename(res, refresh, true)
			if got != UniversalHDR {
				t.Errorf("SelectFilename(%q,%d,true)=%q want %q", res, refresh, got, UniversalHDR)
			}
		}
	}
}

func TestSelectFilename_UniversalSDR(t *testing.T) {
	for _, refresh := range []int{60, 120} {
		for _, res := range SupportedResolutions() {
			got := SelectFilename(res, refresh, false)
			if got != UniversalSDR {
				t.Errorf("SelectFilename(%q,%d,false)=%q want %q", res, refresh, got, UniversalSDR)
			}
		}
	}
}

func TestSelectFilename_NewMidRangeModes(t *testing.T) {
	// The modes the old single-mode SKUs never advertised.
	cases := []struct {
		res     string
		refresh int
		hdr     bool
		want    string
	}{
		{"2560x1440", 120, true, UniversalHDR},
		{"2560x1440", 60, false, UniversalSDR},
		{"1920x1200", 120, false, UniversalSDR},
		{"1920x1200", 120, true, UniversalHDR},
		{"1280x720", 60, false, UniversalSDR},
	}
	for _, c := range cases {
		if got := SelectFilename(c.res, c.refresh, c.hdr); got != c.want {
			t.Errorf("SelectFilename(%q,%d,%v)=%q want %q", c.res, c.refresh, c.hdr, got, c.want)
		}
	}
}

func TestSelectFilename_CaseInsensitiveResolution(t *testing.T) {
	if got := SelectFilename("3840X2160", 120, true); got != UniversalHDR {
		t.Errorf("uppercase 'X' should still resolve; got %q", got)
	}
}

func TestSelectFilename_UnsupportedYieldsEmpty(t *testing.T) {
	cases := []struct {
		res     string
		refresh int
	}{
		{"2560x1440", 240}, // 1440p240 not expressible (pixel clock > 655MHz, no VIC)
		{"3440x1440", 120}, // ultrawide, unsupported resolution
		{"3840x2160", 0},   // refresh not set
		{"800x600", 60},    // below the matrix
		{"", 120},          // empty resolution
	}
	for _, c := range cases {
		if got := SelectFilename(c.res, c.refresh, false); got != "" {
			t.Errorf("SelectFilename(%q,%d,false) should be empty; got %q", c.res, c.refresh, got)
		}
	}
}

func TestSupportsMode(t *testing.T) {
	if !SupportsMode("1920x1080", 120) {
		t.Errorf("1920x1080@120 should be supported")
	}
	if !IsSupportedResolution("2560x1440") {
		t.Errorf("2560x1440 should be a supported resolution")
	}
	if IsSupportedResolution("1024x768") {
		t.Errorf("1024x768 should not be a supported resolution")
	}
	if IsSupportedRefresh(144) {
		t.Errorf("144 should not be a supported refresh")
	}
	if SupportsMode("1920x1080", 30) {
		t.Errorf("1920x1080@30 should not be supported")
	}
	// High-refresh extras: feasible within the EDID detailed-timing limit.
	for _, m := range []struct {
		res     string
		refresh int
	}{{"1920x1080", 144}, {"1920x1080", 240}, {"2560x1440", 144}} {
		if !SupportsMode(m.res, m.refresh) {
			t.Errorf("%s@%d should be supported (high-refresh extra)", m.res, m.refresh)
		}
	}
	// Not expressible in a forced EDID (pixel clock > 655MHz and no CTA VIC).
	for _, m := range []struct {
		res     string
		refresh int
	}{{"2560x1440", 240}, {"3840x2160", 144}, {"3840x2160", 240}} {
		if SupportsMode(m.res, m.refresh) {
			t.Errorf("%s@%d should NOT be supported (exceeds EDID timing)", m.res, m.refresh)
		}
	}
}

func TestFilenameForUniversal(t *testing.T) {
	if FilenameFor("virtual-universal-hdr") != UniversalHDR {
		t.Errorf("FilenameFor(virtual-universal-hdr)=%q want %q", FilenameFor("virtual-universal-hdr"), UniversalHDR)
	}
	if FilenameFor("virtual-universal-sdr") != UniversalSDR {
		t.Errorf("FilenameFor(virtual-universal-sdr)=%q want %q", FilenameFor("virtual-universal-sdr"), UniversalSDR)
	}
}

func TestPlanGrubArgs_Tokens(t *testing.T) {
	a := PlanGrubArgs("DP-1", "virtual-4k120-hdr.bin", []string{"DP-2", "DP-3"})
	toks := a.Tokens()
	want := []string{
		"drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin",
		"video=DP-1:e",
		"video=DP-2:d",
		"video=DP-3:d",
		"nvidia-drm.modeset=1",
		"nvidia-drm.fbdev=1",
	}
	if len(toks) != len(want) {
		t.Fatalf("token count: got %d (%v) want %d (%v)", len(toks), toks, len(want), want)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("tokens[%d]: got %q want %q", i, toks[i], want[i])
		}
	}
}

func TestPlanGrubArgs_SkipsForcedConnectorAndEmpties(t *testing.T) {
	a := PlanGrubArgs("DP-1", "virtual-4k120-hdr.bin", []string{"DP-1", "", "DP-2"})
	for _, tok := range a.VideoDisableAll {
		if tok == "video=DP-1:d" {
			t.Errorf("forced connector should not be in disable list: %v", a.VideoDisableAll)
		}
		if tok == "video=:d" {
			t.Errorf("empty connector should be skipped: %v", a.VideoDisableAll)
		}
	}
}

func TestGrubDropIn_AppendsToExistingCmdline(t *testing.T) {
	a := PlanGrubArgs("DP-1", "virtual-4k120-hdr.bin", []string{"DP-2"})
	body := GrubDropIn(a)
	if !strings.Contains(body, `GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT `) {
		t.Errorf("drop-in must append rather than replace; got:\n%s", body)
	}
	if !strings.Contains(body, "drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin") {
		t.Errorf("drop-in must include EDID token; got:\n%s", body)
	}
}

func TestCmdlinePresent(t *testing.T) {
	a := PlanGrubArgs("DP-1", "virtual-4k120-hdr.bin", nil)
	good := "BOOT_IMAGE=/vmlinuz ro " + a.CmdlineFragment() + " quiet"
	if !CmdlinePresent(good, a) {
		t.Errorf("CmdlinePresent should return true when every token is present")
	}
	bad := "BOOT_IMAGE=/vmlinuz ro nvidia-drm.modeset=1 quiet" // missing EDID + fbdev
	if CmdlinePresent(bad, a) {
		t.Errorf("CmdlinePresent should return false when EDID firmware token is missing")
	}
}

func TestFilenameFor(t *testing.T) {
	if FilenameFor("virtual-4k120-hdr") != "virtual-4k120-hdr.bin" {
		t.Errorf("FilenameFor failed for virtual-4k120-hdr")
	}
	if FilenameFor("nope") != "" {
		t.Errorf("FilenameFor unknown should be empty")
	}
}
