package cuda

// CUDA apt package names differ across releases / NVIDIA repos:
//
//   - On Ubuntu archive: nvidia-cuda-toolkit (older, lags upstream)
//   - On developer.download.nvidia.com (CUDA repo):
//       cuda-toolkit (metapackage; latest)
//       cuda-toolkit-13-0, cuda-toolkit-12-4, cuda-toolkit-12-2 ...
//
// The hard-coded "cuda-toolkit-${MAJOR}" guess in the early Cuda
// phase was wrong for almost every real Ubuntu host. This file owns
// candidate discovery: given a preferred major + the host's apt-cache
// availability, return the FIRST candidate that is actually installable.
//
// The phase calls DiscoverCandidate, gets back an installable package
// name (or "" if nothing matches), and proceeds based on cuda.mode:
//
//   - none      -> no candidate needed; phase skips immediately.
//   - optional  -> "" candidate -> mark failed_nonfatal, continue.
//   - required  -> "" candidate -> failed_fatal.
//
// Apt-cache availability is queried by AvailabilityProbe, which the
// phase wires to a real apt-cache call (or, in tests, to a fake
// function).

import (
	"fmt"
	"strings"
)

// AvailabilityProbe is "is this apt package installable?". Returns
// true when apt-cache policy reports a non-(none) Candidate. The
// phase wraps the real apt-cache call; tests can pass a stub.
type AvailabilityProbe func(pkg string) bool

// CandidateOptions controls how DiscoverCandidate enumerates names.
//
// PreferredMajor is the driver major from the profile (e.g. "580").
// CUDA tracks NVIDIA driver: driver 580 pairs with CUDA 13; driver
// 535 pairs with CUDA 12. Empty = no preference; DiscoverCandidate
// emits both 13.x and 12.x candidates.
//
// ExplicitName, when non-empty, short-circuits the search: if it's
// installable, return it; otherwise return "". This lets a profile
// say cuda.package_name=cuda-toolkit-13-0 and skip the heuristic.
type CandidateOptions struct {
	PreferredMajor string
	ExplicitName   string
}

// DiscoverCandidate returns the first installable CUDA apt package
// name from the candidate ladder, or "" if none are installable.
//
// Ladder (when ExplicitName is empty):
//
//  1. cuda-toolkit-${major}-0 / -1 / -2 / -3 / -4 / -5 (NVIDIA repo)
//     where ${major} is derived from PreferredMajor.
//  2. cuda-toolkit (metapackage on the NVIDIA repo).
//  3. nvidia-cuda-toolkit (Ubuntu archive).
//
// CandidateLadder returns the same list as a slice without probing;
// useful for doctor cuda printouts.
func DiscoverCandidate(opts CandidateOptions, probe AvailabilityProbe) string {
	if opts.ExplicitName != "" {
		if probe == nil || probe(opts.ExplicitName) {
			return opts.ExplicitName
		}
		return ""
	}
	if probe == nil {
		// No probe = we can't tell what's installable. Return the
		// top-of-ladder candidate so doctor cuda can at least say
		// what it would try.
		ladder := CandidateLadder(opts)
		if len(ladder) > 0 {
			return ladder[0]
		}
		return ""
	}
	for _, c := range CandidateLadder(opts) {
		if probe(c) {
			return c
		}
	}
	return ""
}

// CandidateLadder returns the ordered list of names DiscoverCandidate
// would probe. Exported for doctor cuda output + tests.
func CandidateLadder(opts CandidateOptions) []string {
	if opts.ExplicitName != "" {
		return []string{opts.ExplicitName}
	}
	cudaMajor := cudaMajorForDriver(opts.PreferredMajor)
	var out []string
	// cuda-toolkit-${cudaMajor}-${minor}, newest minor first.
	if cudaMajor != "" {
		for _, minor := range []string{"5", "4", "3", "2", "1", "0"} {
			out = append(out, fmt.Sprintf("cuda-toolkit-%s-%s", cudaMajor, minor))
		}
	}
	// Always try the metapackage second-to-last; it tracks the
	// latest CUDA toolkit on the configured NVIDIA repo.
	out = append(out, "cuda-toolkit")
	// Ubuntu-archive fallback, last.
	out = append(out, "nvidia-cuda-toolkit")
	return out
}

// cudaMajorForDriver maps an NVIDIA driver major to the most likely
// CUDA major. Conservative: if we don't recognise the driver, return
// "" so the ladder skips the cuda-toolkit-${major}-${minor} block.
//
// Source: developer.nvidia.com CUDA-driver compatibility matrix.
//
//	driver 580 -> CUDA 13
//	driver 570 -> CUDA 12 (still common in the wild)
//	driver 560 -> CUDA 12
//	driver 550 -> CUDA 12
//	driver 535 -> CUDA 12
func cudaMajorForDriver(driverMajor string) string {
	switch strings.TrimSpace(driverMajor) {
	case "580", "585", "590":
		return "13"
	case "570", "560", "550", "545", "540", "535":
		return "12"
	}
	return ""
}
