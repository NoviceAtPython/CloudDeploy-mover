package cuda

// CUDA apt package names differ across releases / NVIDIA repos:
//
//   - On Ubuntu archive: nvidia-cuda-toolkit (older, lags upstream;
//     currently CUDA 12.4 on 24.04 + 25.10).
//   - On developer.download.nvidia.com (CUDA repo):
//       cuda-toolkit (metapackage; tracks latest)
//       cuda-toolkit-14-N, cuda-toolkit-13-N, cuda-toolkit-12-N ...
//
// The ladder + filter logic is policy-driven so a strict profile
// can refuse to install a stale Ubuntu archive CUDA 12 when CUDA 13
// is required, while a broad-compatibility profile can deliberately
// fall back to it when the official NVIDIA repo has no compatible
// package for the host's Ubuntu version.

import (
	"fmt"
	"strconv"
	"strings"
)

// AvailabilityProbe is "is this apt package installable?". Returns
// true when apt-cache policy reports a non-(none) Candidate. The
// phase wraps the real apt-cache call; tests can pass a stub.
type AvailabilityProbe func(pkg string) bool

// SelectionPolicy mirrors the YAML knob `cuda.selection_policy`.
type SelectionPolicy string

const (
	// PolicyLatestCompatible: empty / default. Prefer the newest
	// official NVIDIA toolkit major compatible with the installed
	// driver; fall through to lower majors, the metapackage, and
	// (if allowed) the Ubuntu archive.
	PolicyLatestCompatible SelectionPolicy = "latest-compatible"
	// PolicyExactMajor: ExpectedMajor must be installed exactly.
	PolicyExactMajor SelectionPolicy = "exact-major"
	// PolicyMinMajor: accept any toolkit major >= MinMajor.
	PolicyMinMajor SelectionPolicy = "min-major"
	// PolicyAny: any installable toolkit is fine.
	PolicyAny SelectionPolicy = "any"
)

// ParseSelectionPolicy normalizes the YAML knob. Empty defaults to
// PolicyLatestCompatible.
func ParseSelectionPolicy(s string) (SelectionPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "latest-compatible":
		return PolicyLatestCompatible, nil
	case "exact-major":
		return PolicyExactMajor, nil
	case "min-major":
		return PolicyMinMajor, nil
	case "any":
		return PolicyAny, nil
	}
	return "", fmt.Errorf("cuda: unknown selection_policy %q (want latest-compatible|exact-major|min-major|any)", s)
}

// CandidateOptions controls how DiscoverCandidate enumerates names.
//
// PreferredMajor is the driver-derived preference (e.g. "13" for
// driver 580; "12" for 535). Empty for unknown future drivers — the
// ladder still emits all known majors descending.
//
// ExplicitName, when non-empty, short-circuits the search: if it's
// installable, return it; otherwise return "". This lets a profile
// say cuda.package_name=cuda-toolkit-13-0 and skip the heuristic.
//
// ExpectedMajor + MinMajor + SelectionPolicy combine to filter the
// ladder. AllowUbuntuArchiveFallback gates Ubuntu's nvidia-cuda-toolkit.
type CandidateOptions struct {
	PreferredMajor             string
	ExplicitName               string
	ExpectedMajor              string
	MinMajor                   string
	SelectionPolicy            SelectionPolicy
	AllowUbuntuArchiveFallback bool
}

// knownCudaMajors lists every CUDA major the ladder generator knows
// about, paired with the apt-suffixes ("X-Y") seen on the official
// NVIDIA repo. Order MUST be descending: newer majors first. New
// majors are appended at the top; minors are appended at the front of
// their list.
//
// Maintenance: when NVIDIA publishes a new minor (e.g. cuda-toolkit-13-6),
// prepend the minor to the entry's `minors` slice. When a new major
// drops, prepend a new entry. Tests cover both directions.
var knownCudaMajors = []cudaMajorEntry{
	{Major: "14", Minors: []string{"5", "4", "3", "2", "1", "0"}},
	{Major: "13", Minors: []string{"5", "4", "3", "2", "1", "0"}},
	{Major: "12", Minors: []string{"9", "8", "7", "6", "5", "4", "3", "2", "1", "0"}},
}

type cudaMajorEntry struct {
	Major  string
	Minors []string
}

// CandidateLadder returns the ordered list of names DiscoverCandidate
// would probe. Exported for doctor cuda output + tests.
func CandidateLadder(opts CandidateOptions) []string {
	if opts.ExplicitName != "" {
		return []string{opts.ExplicitName}
	}

	policy := opts.SelectionPolicy
	if policy == "" {
		policy = PolicyLatestCompatible
	}

	// 1. Pick which majors to consider given the policy.
	majors := majorsForPolicy(policy, opts.ExpectedMajor, opts.MinMajor)

	// 2. Order them so the driver-preferred major comes first if it
	//    survives policy filtering. Other majors stay in descending
	//    order.
	majors = orderMajorsWithPreference(majors, opts.PreferredMajor)

	// 3. Emit cuda-toolkit-${major}-${minor} entries.
	var out []string
	for _, m := range majors {
		out = append(out, namesForMajor(m)...)
	}

	// 4. Append the metapackage. exact-major skips it: the
	//    metapackage tracks "latest", which is not necessarily the
	//    pinned major.
	if policy != PolicyExactMajor {
		out = append(out, "cuda-toolkit")
	}

	// 5. Append Ubuntu archive fallback only when allowed by both
	//    the profile knob AND the policy. exact-major never accepts
	//    the archive (it ships CUDA 12.x), unless that's the pinned
	//    major.
	if opts.AllowUbuntuArchiveFallback && archiveAllowedByPolicy(policy, opts.ExpectedMajor, opts.MinMajor) {
		out = append(out, "nvidia-cuda-toolkit")
	}
	return out
}

// majorsForPolicy returns the CUDA majors that survive the policy
// filter, in default (descending) order.
func majorsForPolicy(policy SelectionPolicy, expectedMajor, minMajor string) []string {
	switch policy {
	case PolicyExactMajor:
		if expectedMajor == "" {
			return nil
		}
		// Even an "unknown to us" pin (e.g. "15") yields a single
		// entry; the apt-cache probe will just fail to find it,
		// which is the correct loud failure for a misconfigured pin.
		return []string{expectedMajor}
	case PolicyMinMajor:
		if minMajor == "" {
			// No minimum configured: behave like latest-compatible.
			return knownCudaMajorsList()
		}
		minN, err := strconv.Atoi(minMajor)
		if err != nil {
			return knownCudaMajorsList()
		}
		var out []string
		for _, m := range knownCudaMajors {
			n, err := strconv.Atoi(m.Major)
			if err != nil {
				continue
			}
			if n >= minN {
				out = append(out, m.Major)
			}
		}
		return out
	default:
		// PolicyLatestCompatible + PolicyAny: all known majors.
		return knownCudaMajorsList()
	}
}

func knownCudaMajorsList() []string {
	out := make([]string, 0, len(knownCudaMajors))
	for _, m := range knownCudaMajors {
		out = append(out, m.Major)
	}
	return out
}

// orderMajorsWithPreference puts `preferred` first (if present in the
// input list) and keeps the remaining majors in their original
// (descending) order. If `preferred` is empty or absent from `majors`,
// returns `majors` unchanged.
func orderMajorsWithPreference(majors []string, preferred string) []string {
	if preferred == "" {
		return majors
	}
	idx := -1
	for i, m := range majors {
		if m == preferred {
			idx = i
			break
		}
	}
	if idx == -1 {
		return majors
	}
	out := make([]string, 0, len(majors))
	out = append(out, preferred)
	for i, m := range majors {
		if i == idx {
			continue
		}
		out = append(out, m)
	}
	return out
}

func namesForMajor(major string) []string {
	for _, m := range knownCudaMajors {
		if m.Major == major {
			out := make([]string, 0, len(m.Minors))
			for _, minor := range m.Minors {
				out = append(out, fmt.Sprintf("cuda-toolkit-%s-%s", m.Major, minor))
			}
			return out
		}
	}
	// Unknown major (e.g. operator-pinned future): emit a small
	// best-effort range. apt-cache will reject anything that isn't
	// real; this just means we don't return an empty ladder.
	out := make([]string, 0, 6)
	for _, minor := range []string{"5", "4", "3", "2", "1", "0"} {
		out = append(out, fmt.Sprintf("cuda-toolkit-%s-%s", major, minor))
	}
	return out
}

// archiveAllowedByPolicy says whether Ubuntu's nvidia-cuda-toolkit
// (currently CUDA 12.x) is policy-allowed AFTER the per-profile
// AllowUbuntuArchiveFallback gate has already said yes.
//
//   - exact-major: only when ExpectedMajor=="12" (the archive's
//     actual major today). The strict CUDA 13 profile must never
//     reach the archive.
//   - min-major: only when MinMajor<="12".
//   - latest-compatible / any: always.
func archiveAllowedByPolicy(policy SelectionPolicy, expectedMajor, minMajor string) bool {
	switch policy {
	case PolicyExactMajor:
		return expectedMajor == "12"
	case PolicyMinMajor:
		if minMajor == "" {
			return true
		}
		n, err := strconv.Atoi(minMajor)
		if err != nil {
			return true
		}
		return n <= 12
	}
	return true
}

// PreferredMajorForDriver maps an NVIDIA driver major to the CUDA
// major NVIDIA pairs it with by default. Conservative: only entries
// observed from NVIDIA's driver-CUDA compatibility matrix are listed.
// Unknown drivers (current or future) return "" and the ladder emits
// every known major in descending order — see knownCudaMajors.
//
// Reference: developer.nvidia.com CUDA-driver compatibility matrix.
//
//	driver 580 -> CUDA 13
//	driver 570 -> CUDA 12
//	driver 560 -> CUDA 12
//	driver 550 -> CUDA 12
//	driver 535 -> CUDA 12
func PreferredMajorForDriver(driverMajor string) string {
	switch strings.TrimSpace(driverMajor) {
	case "580":
		return "13"
	case "570", "565", "560", "555", "550", "545", "540", "535":
		return "12"
	}
	return ""
}

// DiscoverCandidate returns the first installable name from the
// policy-filtered ladder, or "" when nothing matches.
func DiscoverCandidate(opts CandidateOptions, probe AvailabilityProbe) string {
	if opts.ExplicitName != "" {
		if probe == nil || probe(opts.ExplicitName) {
			return opts.ExplicitName
		}
		return ""
	}
	ladder := CandidateLadder(opts)
	if probe == nil {
		if len(ladder) > 0 {
			return ladder[0]
		}
		return ""
	}
	for _, c := range ladder {
		if probe(c) {
			return c
		}
	}
	return ""
}
