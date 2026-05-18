package cuda

import (
	"strings"
	"testing"
)

func TestSelection_SatisfiesNvcc_LatestCompatibleAccepts13(t *testing.T) {
	s := Selection{Policy: PolicyLatestCompatible}
	ok, _ := s.SatisfiesNvcc(NvccRelease{Major: "13", Minor: "0"})
	if !ok {
		t.Errorf("latest-compatible should accept any parseable nvcc")
	}
}

func TestSelection_SatisfiesNvcc_ExactMajorRejectsMismatch(t *testing.T) {
	s := Selection{Policy: PolicyExactMajor, ExpectedMajor: "13"}
	ok, why := s.SatisfiesNvcc(NvccRelease{Major: "12", Minor: "4"})
	if ok {
		t.Errorf("exact-major=13 must reject nvcc 12.4")
	}
	if !strings.Contains(why, "exact-major") {
		t.Errorf("reason should explain the policy: %q", why)
	}
}

func TestSelection_SatisfiesNvcc_ExactMajorAcceptsMatch(t *testing.T) {
	s := Selection{Policy: PolicyExactMajor, ExpectedMajor: "13"}
	ok, _ := s.SatisfiesNvcc(NvccRelease{Major: "13", Minor: "0"})
	if !ok {
		t.Errorf("exact-major=13 must accept nvcc 13.0")
	}
}

func TestSelection_SatisfiesNvcc_MinMajorAcceptsHigher(t *testing.T) {
	s := Selection{Policy: PolicyMinMajor, MinMajor: "12"}
	ok, _ := s.SatisfiesNvcc(NvccRelease{Major: "13", Minor: "0"})
	if !ok {
		t.Errorf("min-major=12 must accept nvcc 13.0")
	}
}

func TestSelection_SatisfiesNvcc_MinMajorRejectsLower(t *testing.T) {
	s := Selection{Policy: PolicyMinMajor, MinMajor: "13"}
	ok, why := s.SatisfiesNvcc(NvccRelease{Major: "12", Minor: "4"})
	if ok {
		t.Errorf("min-major=13 must reject nvcc 12.4")
	}
	if !strings.Contains(why, "min-major") {
		t.Errorf("reason should explain the policy: %q", why)
	}
}

func TestSelection_SatisfiesNvcc_LatestCompatibleHonorsMinFloor(t *testing.T) {
	// MinMajor acts as a soft floor under latest-compatible.
	s := Selection{Policy: PolicyLatestCompatible, MinMajor: "13"}
	ok, _ := s.SatisfiesNvcc(NvccRelease{Major: "12", Minor: "4"})
	if ok {
		t.Errorf("latest-compatible + min-floor=13 must reject 12.x")
	}
	okHigher, _ := s.SatisfiesNvcc(NvccRelease{Major: "13", Minor: "0"})
	if !okHigher {
		t.Errorf("latest-compatible + min-floor=13 must accept 13.0")
	}
}

func TestSelection_SatisfiesNvcc_ZeroRelease_AlwaysRejected(t *testing.T) {
	for _, pol := range []SelectionPolicy{PolicyLatestCompatible, PolicyExactMajor, PolicyMinMajor, PolicyAny} {
		s := Selection{Policy: pol, ExpectedMajor: "13", MinMajor: "12"}
		ok, _ := s.SatisfiesNvcc(NvccRelease{})
		if ok {
			t.Errorf("zero NvccRelease must never satisfy policy %q", pol)
		}
	}
}

func TestSelection_EffectivePinnedMajor(t *testing.T) {
	cases := []struct {
		sel  Selection
		want string
	}{
		{Selection{Policy: PolicyExactMajor, ExpectedMajor: "13"}, "13"},
		{Selection{Policy: PolicyMinMajor, MinMajor: "12"}, "12"},
		{Selection{Policy: PolicyLatestCompatible, DriverPreferredMajor: "13"}, "13"},
		{Selection{Policy: PolicyAny, DriverPreferredMajor: ""}, ""},
	}
	for _, c := range cases {
		if got := c.sel.EffectivePinnedMajor(); got != c.want {
			t.Errorf("EffectivePinnedMajor(%+v): got %q want %q", c.sel, got, c.want)
		}
	}
}
