package cuda

import (
	"fmt"
	"strconv"
	"strings"
)

// Selection bundles the configured selection knobs from the profile
// (post-parse) plus the driver-derived preference. Plumbed through the
// phase's apt + runfile + verify steps so all of them share the same
// view of "what counts as a successful install for this profile".
type Selection struct {
	// DriverPreferredMajor is what PreferredMajorForDriver returned
	// for the profile's nvidia.driver_major. "" for unknown drivers.
	DriverPreferredMajor string
	// Policy is the post-parse SelectionPolicy.
	Policy SelectionPolicy
	// ExpectedMajor / MinMajor mirror the profile's CUDAConfig.
	ExpectedMajor string
	MinMajor      string
	// AllowUbuntuArchiveFallback mirrors the profile knob.
	AllowUbuntuArchiveFallback bool
}

// CandidateOptions packs this Selection into the form CandidateLadder
// expects, plus the operator's explicit package_name override.
func (s Selection) CandidateOptions(explicitName string) CandidateOptions {
	return CandidateOptions{
		PreferredMajor:             s.DriverPreferredMajor,
		ExplicitName:               explicitName,
		ExpectedMajor:              s.ExpectedMajor,
		MinMajor:                   s.MinMajor,
		SelectionPolicy:            s.Policy,
		AllowUbuntuArchiveFallback: s.AllowUbuntuArchiveFallback,
	}
}

// SatisfiesNvcc returns (ok, reason). ok=true means the installed
// nvcc release meets the selection's policy. reason carries an
// operator-friendly explanation for the failure case.
//
// Cases:
//
//   - rel is zero-valued (no nvcc / unparseable):
//     never satisfies, regardless of policy.
//   - PolicyExactMajor: rel.Major must equal ExpectedMajor.
//   - PolicyMinMajor: rel.Major must be a number >= MinMajor.
//   - PolicyLatestCompatible / PolicyAny / empty: any parseable rel
//     satisfies. MinMajor is honored if set (acts as a soft floor).
func (s Selection) SatisfiesNvcc(rel NvccRelease) (bool, string) {
	if rel.Major == "" {
		return false, "no nvcc release detected"
	}
	policy := s.Policy
	if policy == "" {
		policy = PolicyLatestCompatible
	}
	switch policy {
	case PolicyExactMajor:
		if s.ExpectedMajor == "" {
			return false, "selection_policy=exact-major but expected_major is empty"
		}
		if rel.Major != s.ExpectedMajor {
			return false, fmt.Sprintf("nvcc reports CUDA %s; selection_policy=exact-major requires major %s", rel.Full(), s.ExpectedMajor)
		}
		return true, ""
	case PolicyMinMajor:
		if s.MinMajor == "" {
			return false, "selection_policy=min-major but min_major is empty"
		}
		minN, err := strconv.Atoi(strings.TrimSpace(s.MinMajor))
		if err != nil {
			return false, fmt.Sprintf("min_major %q is not a positive integer", s.MinMajor)
		}
		got, err := strconv.Atoi(rel.Major)
		if err != nil {
			return false, fmt.Sprintf("nvcc major %q is not a positive integer", rel.Major)
		}
		if got < minN {
			return false, fmt.Sprintf("nvcc reports CUDA %s; selection_policy=min-major requires major >= %s", rel.Full(), s.MinMajor)
		}
		return true, ""
	default:
		// PolicyLatestCompatible / PolicyAny: any parseable nvcc OK.
		// If MinMajor is set as a soft floor, honor it.
		if s.MinMajor != "" {
			minN, err := strconv.Atoi(strings.TrimSpace(s.MinMajor))
			if err == nil {
				got, err := strconv.Atoi(rel.Major)
				if err == nil && got < minN {
					return false, fmt.Sprintf("nvcc reports CUDA %s; min_major %s not satisfied", rel.Full(), s.MinMajor)
				}
			}
		}
		return true, ""
	}
}

// EffectivePinnedMajor returns the "operator-pinned" major the phase
// uses for diagnostics (state.Details["expected_major"]). For
// exact-major this is ExpectedMajor; for min-major it's MinMajor; for
// the open policies it's whatever the driver pairs with.
func (s Selection) EffectivePinnedMajor() string {
	switch s.Policy {
	case PolicyExactMajor:
		return s.ExpectedMajor
	case PolicyMinMajor:
		return s.MinMajor
	}
	return s.DriverPreferredMajor
}
