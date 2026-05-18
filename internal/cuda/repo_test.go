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

// -----------------------------------------------------------------------------
// ResolveCudaRepoCandidates
// -----------------------------------------------------------------------------

func TestResolveCudaRepoCandidates_Defaults(t *testing.T) {
	got := ResolveCudaRepoCandidates("25.10", nil, false)
	if len(got) != 1 || got[0] != "ubuntu2510" {
		t.Errorf("empty configured list should default to [auto-host -> host slug]; got %v", got)
	}
}

func TestResolveCudaRepoCandidates_CrossDistroAllowed(t *testing.T) {
	got := ResolveCudaRepoCandidates("25.10",
		[]string{"auto-host", "ubuntu2404"}, true)
	want := []string{"ubuntu2510", "ubuntu2404"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolveCudaRepoCandidates_CrossDistroDeniedDropsExtras(t *testing.T) {
	got := ResolveCudaRepoCandidates("25.10",
		[]string{"auto-host", "ubuntu2404"}, false)
	if len(got) != 1 || got[0] != "ubuntu2510" {
		t.Errorf("allow_cross_distro_cuda_repo=false should drop ubuntu2404; got %v", got)
	}
}

func TestResolveCudaRepoCandidates_DedupePreservesOrder(t *testing.T) {
	got := ResolveCudaRepoCandidates("24.04",
		[]string{"auto-host", "ubuntu2404", "auto-host"}, true)
	// auto-host on 24.04 expands to ubuntu2404, which then dedupes
	// against the explicit ubuntu2404 entry and the second auto-host.
	if len(got) != 1 || got[0] != "ubuntu2404" {
		t.Errorf("dedup should collapse to [ubuntu2404]; got %v", got)
	}
}

func TestResolveCudaRepoCandidates_AutoHostUnknownVersionDropped(t *testing.T) {
	// Non-Ubuntu host: auto-host expands to "" and is dropped.
	got := ResolveCudaRepoCandidates("", []string{"auto-host", "ubuntu2404"}, true)
	if len(got) != 1 || got[0] != "ubuntu2404" {
		t.Errorf("auto-host on unknown host should drop; got %v", got)
	}
}

// -----------------------------------------------------------------------------
// PickReachableRepoDistro
// -----------------------------------------------------------------------------

func TestPickReachableRepoDistro_FirstReachable(t *testing.T) {
	candidates := []string{"ubuntu2510", "ubuntu2404"}
	probe := func(d string) bool { return d == "ubuntu2510" }
	got := PickReachableRepoDistro("25.10", candidates, probe)
	if got.Selected != "ubuntu2510" {
		t.Errorf("Selected: got %q want ubuntu2510", got.Selected)
	}
	if got.CrossDistro {
		t.Errorf("CrossDistro: got true want false (host==selected)")
	}
	if len(got.Tried) != 1 || got.Tried[0] != "ubuntu2510" {
		t.Errorf("Tried: got %v want [ubuntu2510]", got.Tried)
	}
}

func TestPickReachableRepoDistro_FallsThroughToCrossDistro(t *testing.T) {
	// The brief's central scenario: 25.10 host, ubuntu2510 repo
	// missing, ubuntu2404 reachable.
	candidates := []string{"ubuntu2510", "ubuntu2404"}
	probe := func(d string) bool { return d == "ubuntu2404" }
	got := PickReachableRepoDistro("25.10", candidates, probe)
	if got.Selected != "ubuntu2404" {
		t.Errorf("Selected: got %q want ubuntu2404", got.Selected)
	}
	if !got.CrossDistro {
		t.Errorf("CrossDistro: got false want true (selected != host native)")
	}
	if got.HostNative != "ubuntu2510" {
		t.Errorf("HostNative: got %q want ubuntu2510", got.HostNative)
	}
	if len(got.Tried) != 2 || got.Tried[0] != "ubuntu2510" || got.Tried[1] != "ubuntu2404" {
		t.Errorf("Tried: got %v want [ubuntu2510 ubuntu2404]", got.Tried)
	}
}

func TestPickReachableRepoDistro_NoneReachable(t *testing.T) {
	candidates := []string{"ubuntu2510", "ubuntu2404"}
	probe := func(string) bool { return false }
	got := PickReachableRepoDistro("25.10", candidates, probe)
	if got.Selected != "" {
		t.Errorf("Selected: got %q want empty", got.Selected)
	}
	if got.CrossDistro {
		t.Errorf("CrossDistro: got true want false when nothing was selected")
	}
	if len(got.Tried) != 2 {
		t.Errorf("Tried: got %v; want both candidates probed", got.Tried)
	}
}

func TestPickReachableRepoDistro_NilProbeReturnsEmpty(t *testing.T) {
	got := PickReachableRepoDistro("25.10",
		[]string{"ubuntu2510", "ubuntu2404"}, nil)
	if got.Selected != "" {
		t.Errorf("nil probe should yield empty Selected; got %q", got.Selected)
	}
}
