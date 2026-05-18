package cuda

import (
	"fmt"
	"strings"
)

// RepoDistroForUbuntuVersion turns a /etc/os-release VERSION_ID into
// the NVIDIA CUDA apt-repo slug. v2 maps the four supported releases
// explicitly and falls back to a "ubuntu${maj}${min}" derivation;
// callers must still confirm the resulting slug is reachable
// (RepoAvailable below).
func RepoDistroForUbuntuVersion(version string) string {
	v := strings.TrimSpace(version)
	switch v {
	case "22.04":
		return "ubuntu2204"
	case "24.04":
		return "ubuntu2404"
	case "25.10":
		return "ubuntu2510"
	case "26.04":
		return "ubuntu2604"
	}
	// Fallback: ubuntu + version with dot stripped, matching v2's
	// `printf 'ubuntu%s\n' "${version_id//./}"`. We still gate on
	// RepoAvailable before using it, so guessing here is safe.
	if v == "" {
		return ""
	}
	return "ubuntu" + strings.ReplaceAll(v, ".", "")
}

// KeyringURL is the deterministic download URL for the
// cuda-keyring_1.1-1_all.deb that bootstraps the official NVIDIA CUDA
// apt repository on a given ubuntu distro slug.
func KeyringURL(distro string) string {
	if strings.TrimSpace(distro) == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://developer.download.nvidia.com/compute/cuda/repos/%s/x86_64/cuda-keyring_1.1-1_all.deb",
		distro,
	)
}

// RepoAvailabilityProbe is "is this NVIDIA CUDA apt repo reachable?".
// The phase wires this to a real HEAD probe; tests pass a stub.
type RepoAvailabilityProbe func(distro string) bool

// DetectRepoDistro resolves a v2-style slug for an Ubuntu version and
// returns it iff the probe confirms it is reachable. Returns "" on
// any non-Ubuntu / unknown / unreachable case.
//
// DEPRECATED for new callers: prefer ResolveCudaRepoCandidates +
// PickReachableRepoDistro, which support the cross-distro repo ladder
// (e.g. fall back to `ubuntu2404` from a 25.10 host the way v2 does).
// Kept exported because tests + the old "single-distro probe" path
// still exercise it.
func DetectRepoDistro(versionID string, probe RepoAvailabilityProbe) string {
	distro := RepoDistroForUbuntuVersion(versionID)
	if distro == "" {
		return ""
	}
	if probe == nil {
		// Without a probe we can't confirm reachability; behave like
		// v2 fallback when wget --spider is not run: return "" so the
		// caller treats the repo as unavailable and uses runfile.
		return ""
	}
	if !probe(distro) {
		return ""
	}
	return distro
}

// AutoHostRepoCandidate is the literal that ResolveCudaRepoCandidates
// expands into the host's slug at probe time.
const AutoHostRepoCandidate = "auto-host"

// ResolveCudaRepoCandidates turns the operator-configured candidate
// list into the actual ordered probe list:
//
//   - `auto-host` -> RepoDistroForUbuntuVersion(hostVersion). When the
//     host is non-Ubuntu / unknown, auto-host expands to "" (and is
//     dropped, since probing an empty slug is nonsense).
//   - All other entries are kept verbatim.
//   - Duplicate entries are removed (first wins).
//   - When `allowCrossDistro=false`, any non-auto-host entry is
//     dropped silently — the profile's intent is "native only".
//
// An empty configured list defaults to [AutoHostRepoCandidate].
//
// The returned slice is never nil; callers can range over it freely.
func ResolveCudaRepoCandidates(hostVersion string, configured []string, allowCrossDistro bool) []string {
	if len(configured) == 0 {
		configured = []string{AutoHostRepoCandidate}
	}
	hostSlug := RepoDistroForUbuntuVersion(hostVersion)
	seen := make(map[string]bool, len(configured))
	out := make([]string, 0, len(configured))
	for _, c := range configured {
		v := strings.TrimSpace(c)
		if strings.EqualFold(v, AutoHostRepoCandidate) {
			if hostSlug == "" {
				continue
			}
			v = hostSlug
		} else if !allowCrossDistro {
			continue
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// RepoSelection is the result of walking the candidate ladder.
type RepoSelection struct {
	// Selected is the first reachable slug in `Tried` (i.e., the one
	// the phase should bootstrap). "" when no candidate was reachable.
	Selected string
	// Tried is the ordered list of slugs the resolver probed. Useful
	// for state.Details so an operator can see what the deploy
	// attempted in what order.
	Tried []string
	// CrossDistro reports whether Selected differs from the host's
	// native slug.
	CrossDistro bool
	// HostNative is the slug that "would have been" auto-host (for
	// diagnostics, even when the resolver ultimately picked a
	// different one).
	HostNative string
}

// PickReachableRepoDistro probes each `candidates` slug with `probe`
// and returns the first that probes true. The returned RepoSelection
// records what was tried + whether the selection ended up cross-distro.
func PickReachableRepoDistro(hostVersion string, candidates []string, probe RepoAvailabilityProbe) RepoSelection {
	out := RepoSelection{
		Tried:      []string{},
		HostNative: RepoDistroForUbuntuVersion(hostVersion),
	}
	for _, slug := range candidates {
		out.Tried = append(out.Tried, slug)
		if probe == nil {
			continue
		}
		if probe(slug) {
			out.Selected = slug
			break
		}
	}
	if out.Selected != "" && out.HostNative != "" && out.Selected != out.HostNative {
		out.CrossDistro = true
	}
	return out
}
