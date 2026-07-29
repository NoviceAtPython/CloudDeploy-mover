package ubuntu

import "testing"

// A CUDA profile must not select an Ubuntu release where Sunshine's
// CUDA capture module cannot be built: the toolkit would install and
// capture would still run on the GPU -> RAM -> GPU fallback.
func TestResolveOSTarget_RequireSunshineCUDASkipsUnbuildable(t *testing.T) {
	in := OSResolverInputs{
		CurrentVersion:      "22.04",
		Candidates:          []string{"26.04", "25.10", "24.04"},
		Policy:              OSPolicyLatestCompatible,
		AcceptNonLTS:        true,
		RequireSunshineCUDA: true,
		SkipSupportedCheck:  true,
	}
	got := ResolveOSTarget(in)
	if got.Selected != "24.04" {
		t.Fatalf("Selected = %q, want 24.04 (newest release that can build the CUDA module); rejected=%v", got.Selected, got.RejectedReasons())
	}
	for _, v := range []string{"26.04", "25.10"} {
		if _, ok := got.RejectedReasons()[v]; !ok {
			t.Errorf("%s should have been rejected with a reason", v)
		}
	}
}

// Without the flag the resolver keeps its original behavior.
func TestResolveOSTarget_WithoutRequireSunshineCUDAPicksNewest(t *testing.T) {
	in := OSResolverInputs{
		CurrentVersion:     "22.04",
		Candidates:         []string{"26.04", "25.10", "24.04"},
		Policy:             OSPolicyLatestCompatible,
		AcceptNonLTS:       true,
		SkipSupportedCheck: true,
	}
	if got := ResolveOSTarget(in); got.Selected != "26.04" {
		t.Fatalf("Selected = %q, want 26.04 when the CUDA constraint is off", got.Selected)
	}
}
