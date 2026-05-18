package cuda

import "testing"

func TestRepoDistroForUbuntuVersion(t *testing.T) {
	cases := map[string]string{
		"22.04": "ubuntu2204",
		"24.04": "ubuntu2404",
		"25.10": "ubuntu2510",
		"26.04": "ubuntu2604",
		"":      "",
		// Fallback path: dot-stripped pattern.
		"23.10": "ubuntu2310",
	}
	for in, want := range cases {
		if got := RepoDistroForUbuntuVersion(in); got != want {
			t.Errorf("RepoDistroForUbuntuVersion(%q): got %q want %q", in, got, want)
		}
	}
}

func TestKeyringURL(t *testing.T) {
	got := KeyringURL("ubuntu2404")
	want := "https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb"
	if got != want {
		t.Errorf("KeyringURL: got %q want %q", got, want)
	}
	if KeyringURL("") != "" {
		t.Errorf("KeyringURL: empty distro should yield empty URL")
	}
}

func TestDetectRepoDistro_ProbeMissingReturnsEmpty(t *testing.T) {
	if got := DetectRepoDistro("25.10", nil); got != "" {
		t.Errorf("nil probe should yield empty; got %q", got)
	}
}

func TestDetectRepoDistro_ProbeYesNo(t *testing.T) {
	probeYes := func(string) bool { return true }
	probeNo := func(string) bool { return false }
	if got := DetectRepoDistro("25.10", probeYes); got != "ubuntu2510" {
		t.Errorf("probeYes: got %q want ubuntu2510", got)
	}
	if got := DetectRepoDistro("25.10", probeNo); got != "" {
		t.Errorf("probeNo: got %q want empty", got)
	}
}
