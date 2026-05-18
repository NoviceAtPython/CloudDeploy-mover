package cuda

import "testing"

func TestCandidateLadder_Driver580PrefersCuda13Toolkits(t *testing.T) {
	got := CandidateLadder(CandidateOptions{PreferredMajor: "580"})
	if len(got) < 3 {
		t.Fatalf("expected at least 3 candidates, got %v", got)
	}
	if got[0] != "cuda-toolkit-13-5" {
		t.Errorf("first candidate should be cuda-toolkit-13-5; got %q (full=%v)", got[0], got)
	}
	last2 := got[len(got)-2:]
	if last2[0] != "cuda-toolkit" || last2[1] != "nvidia-cuda-toolkit" {
		t.Errorf("last 2 candidates should be cuda-toolkit then nvidia-cuda-toolkit; got %v", last2)
	}
}

func TestCandidateLadder_Driver535PrefersCuda12Toolkits(t *testing.T) {
	got := CandidateLadder(CandidateOptions{PreferredMajor: "535"})
	if got[0] != "cuda-toolkit-12-5" {
		t.Errorf("first candidate should be cuda-toolkit-12-5; got %q", got[0])
	}
}

func TestCandidateLadder_UnknownDriverSkipsToolkitsBlock(t *testing.T) {
	got := CandidateLadder(CandidateOptions{PreferredMajor: "9999"})
	// Only the metapackage + Ubuntu fallback.
	if len(got) != 2 || got[0] != "cuda-toolkit" || got[1] != "nvidia-cuda-toolkit" {
		t.Errorf("unknown driver should yield [cuda-toolkit, nvidia-cuda-toolkit]; got %v", got)
	}
}

func TestCandidateLadder_FiltersUbuntuArchiveWhenCuda13Required(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor: "580",
		RequiredMajor:  "13",
	})
	for _, c := range got {
		if c == "nvidia-cuda-toolkit" {
			t.Errorf("CUDA 13 required: Ubuntu's nvidia-cuda-toolkit must be filtered (currently 12.x); got %v", got)
		}
	}
	// The cuda-toolkit metapackage MUST still be there as the
	// fallback when no major-minor matches.
	last := got[len(got)-1]
	if last != "cuda-toolkit" {
		t.Errorf("last candidate should be cuda-toolkit metapackage when ubuntu archive is filtered; got %v", got)
	}
}

func TestCandidateLadder_KeepsUbuntuArchiveForCuda12(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor: "535",
		RequiredMajor:  "12",
	})
	found := false
	for _, c := range got {
		if c == "nvidia-cuda-toolkit" {
			found = true
		}
	}
	if !found {
		t.Errorf("CUDA 12 required: nvidia-cuda-toolkit should remain in the ladder; got %v", got)
	}
}

func TestCandidateLadder_ExplicitNameShortCircuits(t *testing.T) {
	got := CandidateLadder(CandidateOptions{ExplicitName: "cuda-toolkit-13-0"})
	if len(got) != 1 || got[0] != "cuda-toolkit-13-0" {
		t.Errorf("explicit name should be the only candidate; got %v", got)
	}
}

func TestDiscoverCandidate_ReturnsFirstAvailable(t *testing.T) {
	probe := func(pkg string) bool {
		return pkg == "cuda-toolkit-13-3" || pkg == "nvidia-cuda-toolkit"
	}
	got := DiscoverCandidate(CandidateOptions{PreferredMajor: "580"}, probe)
	if got != "cuda-toolkit-13-3" {
		t.Errorf("DiscoverCandidate should return the first probe-true entry; got %q", got)
	}
}

func TestDiscoverCandidate_NothingAvailable(t *testing.T) {
	probe := func(pkg string) bool { return false }
	got := DiscoverCandidate(CandidateOptions{PreferredMajor: "580"}, probe)
	if got != "" {
		t.Errorf("expected empty when nothing installable; got %q", got)
	}
}

func TestDiscoverCandidate_ExplicitNamePresent(t *testing.T) {
	probe := func(pkg string) bool { return pkg == "my-special-cuda" }
	got := DiscoverCandidate(CandidateOptions{ExplicitName: "my-special-cuda"}, probe)
	if got != "my-special-cuda" {
		t.Errorf("explicit-name installable should be returned; got %q", got)
	}
}

func TestDiscoverCandidate_ExplicitNameMissing(t *testing.T) {
	probe := func(pkg string) bool { return false }
	got := DiscoverCandidate(CandidateOptions{ExplicitName: "my-special-cuda"}, probe)
	if got != "" {
		t.Errorf("explicit-name not installable should yield empty; got %q", got)
	}
}

func TestDiscoverCandidate_NilProbeReturnsTopOfLadder(t *testing.T) {
	got := DiscoverCandidate(CandidateOptions{PreferredMajor: "580"}, nil)
	if got != "cuda-toolkit-13-5" {
		t.Errorf("nil probe should yield top-of-ladder; got %q", got)
	}
}
