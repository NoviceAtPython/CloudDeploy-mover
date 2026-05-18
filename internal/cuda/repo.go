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
