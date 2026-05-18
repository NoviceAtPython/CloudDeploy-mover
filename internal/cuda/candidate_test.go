package cuda

import (
	"reflect"
	"strings"
	"testing"
)

func TestPreferredMajorForDriver(t *testing.T) {
	cases := map[string]string{
		"580":  "13",
		"570":  "12",
		"535":  "12",
		"":     "",
		"9999": "",
		"590":  "", // unknown future - falls through to "highest known"
		"600":  "",
	}
	for in, want := range cases {
		if got := PreferredMajorForDriver(in); got != want {
			t.Errorf("PreferredMajorForDriver(%q): got %q want %q", in, got, want)
		}
	}
}

func TestParseSelectionPolicy(t *testing.T) {
	for in, want := range map[string]SelectionPolicy{
		"":                  PolicyLatestCompatible,
		"latest-compatible": PolicyLatestCompatible,
		"exact-major":       PolicyExactMajor,
		"min-major":         PolicyMinMajor,
		"any":               PolicyAny,
	} {
		got, err := ParseSelectionPolicy(in)
		if err != nil {
			t.Errorf("ParseSelectionPolicy(%q): err=%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSelectionPolicy(%q): got %q want %q", in, got, want)
		}
	}
	if _, err := ParseSelectionPolicy("yolo"); err == nil {
		t.Error("ParseSelectionPolicy should reject unknown value")
	}
}

func TestCandidateLadder_ExplicitNameShortCircuits(t *testing.T) {
	got := CandidateLadder(CandidateOptions{ExplicitName: "cuda-toolkit-13-0"})
	if !reflect.DeepEqual(got, []string{"cuda-toolkit-13-0"}) {
		t.Errorf("explicit name should be the only candidate; got %v", got)
	}
}

func TestCandidateLadder_DefaultPutsDriverPreferredFirst(t *testing.T) {
	got := CandidateLadder(CandidateOptions{PreferredMajor: "13"})
	if !strings.HasPrefix(got[0], "cuda-toolkit-13-") {
		t.Errorf("driver-preferred=13: first candidate should be a 13-x package; got %v", got)
	}
	// Lower compatible majors must follow in descending order.
	if !containsName(got, "cuda-toolkit-12-9") {
		t.Errorf("ladder should descend into CUDA 12; got %v", got)
	}
	// Metapackage included in default policy.
	if !containsName(got, "cuda-toolkit") {
		t.Errorf("default policy should include cuda-toolkit metapackage; got %v", got)
	}
	// Archive must NOT appear without AllowUbuntuArchiveFallback.
	if containsName(got, "nvidia-cuda-toolkit") {
		t.Errorf("archive must be filtered when AllowUbuntuArchiveFallback=false; got %v", got)
	}
}

func TestCandidateLadder_ExactMajor13_StrictFiltersArchiveAndMetapackage(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyExactMajor,
		ExpectedMajor:              "13",
		AllowUbuntuArchiveFallback: false,
	})
	if len(got) == 0 {
		t.Fatalf("ladder must not be empty for exact-major=13")
	}
	for _, name := range got {
		if name == "nvidia-cuda-toolkit" {
			t.Errorf("exact-major=13: nvidia-cuda-toolkit (CUDA 12.x) must not appear; got %v", got)
		}
		if name == "cuda-toolkit" {
			t.Errorf("exact-major: cuda-toolkit metapackage tracks 'latest' and must not appear; got %v", got)
		}
		if !strings.HasPrefix(name, "cuda-toolkit-13-") {
			t.Errorf("exact-major=13: every candidate must be cuda-toolkit-13-N; got %q in %v", name, got)
		}
	}
}

func TestCandidateLadder_ExactMajor12_AllowsArchive(t *testing.T) {
	// exact-major=12: archive's nvidia-cuda-toolkit (CUDA 12.4) is
	// the correct major, so it IS allowed when the fallback knob is
	// on.
	got := CandidateLadder(CandidateOptions{
		SelectionPolicy:            PolicyExactMajor,
		ExpectedMajor:              "12",
		AllowUbuntuArchiveFallback: true,
	})
	if !containsName(got, "cuda-toolkit-12-4") {
		t.Errorf("exact-major=12 must include cuda-toolkit-12-4; got %v", got)
	}
	if !containsName(got, "nvidia-cuda-toolkit") {
		t.Errorf("exact-major=12 + archive-fallback=true must include nvidia-cuda-toolkit; got %v", got)
	}
}

func TestCandidateLadder_LatestCompatibleArchiveFallbackOK(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyLatestCompatible,
		AllowUbuntuArchiveFallback: true,
	})
	// CUDA 13 must come first.
	if !strings.HasPrefix(got[0], "cuda-toolkit-13-") {
		t.Errorf("latest-compatible + preferred=13: 13 should come first; got %v", got)
	}
	// Archive is appended at the very end.
	if got[len(got)-1] != "nvidia-cuda-toolkit" {
		t.Errorf("latest-compatible + archive=true: nvidia-cuda-toolkit should be last; got %v", got)
	}
}

func TestCandidateLadder_LatestCompatible_Cuda13Available_BeatsArchive(t *testing.T) {
	// Simulate a host where cuda-toolkit-13-0 exists in the NVIDIA
	// repo and nvidia-cuda-toolkit also exists in the Ubuntu archive.
	// DiscoverCandidate must select the NVIDIA repo entry.
	probe := func(pkg string) bool {
		return pkg == "cuda-toolkit-13-0" || pkg == "nvidia-cuda-toolkit"
	}
	got := DiscoverCandidate(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyLatestCompatible,
		AllowUbuntuArchiveFallback: true,
	}, probe)
	if got != "cuda-toolkit-13-0" {
		t.Errorf("CUDA 13 must beat Ubuntu archive; got %q", got)
	}
}

func TestDiscoverCandidate_LatestCompatible_NoCuda13_FallsBackToArchive(t *testing.T) {
	// 25.10 case: NVIDIA repo has no CUDA 13 candidates for this
	// distro; archive's nvidia-cuda-toolkit is the only thing.
	probe := func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" }
	got := DiscoverCandidate(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyLatestCompatible,
		AllowUbuntuArchiveFallback: true,
	}, probe)
	if got != "nvidia-cuda-toolkit" {
		t.Errorf("no CUDA 13 available + archive allowed: should select nvidia-cuda-toolkit; got %q", got)
	}
}

func TestDiscoverCandidate_StrictExact13_RefusesArchive(t *testing.T) {
	probe := func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" }
	got := DiscoverCandidate(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyExactMajor,
		ExpectedMajor:              "13",
		AllowUbuntuArchiveFallback: false,
	}, probe)
	if got != "" {
		t.Errorf("strict exact-major=13 must refuse to install Ubuntu archive CUDA 12; got %q", got)
	}
}

func TestCandidateLadder_MinMajor12_IncludesBoth12And13(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyMinMajor,
		MinMajor:                   "12",
		AllowUbuntuArchiveFallback: true,
	})
	saw12, saw13 := false, false
	for _, c := range got {
		if strings.HasPrefix(c, "cuda-toolkit-12-") {
			saw12 = true
		}
		if strings.HasPrefix(c, "cuda-toolkit-13-") {
			saw13 = true
		}
	}
	if !saw12 || !saw13 {
		t.Errorf("min-major=12 must include both CUDA 12 and CUDA 13; got %v", got)
	}
}

func TestCandidateLadder_MinMajor13_Excludes12(t *testing.T) {
	got := CandidateLadder(CandidateOptions{
		PreferredMajor:             "13",
		SelectionPolicy:            PolicyMinMajor,
		MinMajor:                   "13",
		AllowUbuntuArchiveFallback: true,
	})
	for _, c := range got {
		if strings.HasPrefix(c, "cuda-toolkit-12-") || c == "nvidia-cuda-toolkit" {
			t.Errorf("min-major=13: CUDA 12 / archive must be filtered out; got %v", got)
		}
	}
}

func TestCandidateLadder_FutureDriver600_NonEmpty(t *testing.T) {
	// Future driver 600 is unknown to PreferredMajorForDriver. The
	// ladder must still produce candidates (newest-known majors
	// descending) so the deploy can succeed when a future driver
	// pairs with an existing CUDA major.
	got := CandidateLadder(CandidateOptions{
		PreferredMajor:             PreferredMajorForDriver("600"),
		SelectionPolicy:            PolicyLatestCompatible,
		AllowUbuntuArchiveFallback: true,
	})
	if len(got) < 5 {
		t.Errorf("unknown future driver ladder too short; got %v", got)
	}
	// First entry must be a known major's package, NOT empty.
	if got[0] == "" || got[0] == "nvidia-cuda-toolkit" {
		t.Errorf("unknown future driver ladder must start with an NVIDIA repo CUDA package; got %v", got)
	}
}

func TestDiscoverCandidate_ExplicitNameRespected(t *testing.T) {
	probe := func(pkg string) bool { return pkg == "my-special-cuda" }
	got := DiscoverCandidate(CandidateOptions{ExplicitName: "my-special-cuda"}, probe)
	if got != "my-special-cuda" {
		t.Errorf("explicit-name installable should be returned; got %q", got)
	}
}

func TestDiscoverCandidate_NilProbeReturnsTopOfLadder(t *testing.T) {
	got := DiscoverCandidate(CandidateOptions{
		PreferredMajor:  "580",
		SelectionPolicy: PolicyLatestCompatible,
	}, nil)
	if !strings.HasPrefix(got, "cuda-toolkit-") {
		t.Errorf("nil probe should yield top-of-ladder; got %q", got)
	}
}

// -----------------------------------------------------------------------------

func containsName(list []string, want string) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}
