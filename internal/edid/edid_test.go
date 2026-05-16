package edid

import (
	"strings"
	"testing"
)

func TestSelectFilename_HDR4K(t *testing.T) {
	got := SelectFilename("3840x2160", 120, true)
	if got != "virtual-4k120-hdr.bin" {
		t.Errorf("got %q want virtual-4k120-hdr.bin", got)
	}
}

func TestSelectFilename_SDR4K120(t *testing.T) {
	got := SelectFilename("3840x2160", 120, false)
	if got != "virtual-4k120-sdr.bin" {
		t.Errorf("got %q want virtual-4k120-sdr.bin", got)
	}
}

func TestSelectFilename_SDR4K60(t *testing.T) {
	got := SelectFilename("3840x2160", 60, false)
	if got != "virtual-4k60-sdr.bin" {
		t.Errorf("got %q want virtual-4k60-sdr.bin", got)
	}
}

func TestSelectFilename_SDR1080p(t *testing.T) {
	got := SelectFilename("1920x1080", 60, false)
	if got != "virtual-1080p-sdr.bin" {
		t.Errorf("got %q want virtual-1080p-sdr.bin", got)
	}
}

func TestSelectFilename_UnknownYieldsEmpty(t *testing.T) {
	if got := SelectFilename("2560x1440", 144, false); got != "" {
		t.Errorf("unrecognised resolution should yield empty; got %q", got)
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
