package cuda

import (
	"regexp"
	"strings"
)

// CudaRoot is the canonical /usr/local/cuda symlink the v2 runfile +
// the NVIDIA apt packages both lay down.
const CudaRoot = "/usr/local/cuda"

// ProfileSnippetPath is where the post-install side drops a
// CloudDeploy-owned PATH/LD_LIBRARY_PATH snippet so login shells
// pick CUDA up. v2 sets these inline; v3 persists them.
const ProfileSnippetPath = "/etc/profile.d/clouddeploy-cuda.sh"

// ProfileSnippetBody returns the contents written to ProfileSnippetPath.
// Pure / deterministic so tests can assert on it.
func ProfileSnippetBody() string {
	return "# managed by clouddeployctl: cuda phase\n" +
		"export PATH=\"/usr/local/cuda/bin:${PATH}\"\n" +
		"export LD_LIBRARY_PATH=\"/usr/local/cuda/lib64:${LD_LIBRARY_PATH:-}\"\n"
}

// nvccReleaseRE matches NVIDIA's "release 13.0, V13.0.88" line.
var nvccReleaseRE = regexp.MustCompile(`release ([0-9]+)\.([0-9]+)`)

// NvccRelease is the parse result of `nvcc --version`.
type NvccRelease struct {
	Major string // "13"
	Minor string // "0"
}

// Full is "13.0" when both halves are present, otherwise "".
func (r NvccRelease) Full() string {
	if r.Major == "" || r.Minor == "" {
		return ""
	}
	return r.Major + "." + r.Minor
}

// ParseNvccRelease pulls the first "release X.Y" out of nvcc --version
// output. Returns zero NvccRelease when nothing parses (caller-aware).
func ParseNvccRelease(out string) NvccRelease {
	m := nvccReleaseRE.FindStringSubmatch(out)
	if m == nil {
		return NvccRelease{}
	}
	return NvccRelease{Major: m[1], Minor: m[2]}
}

// pkgToolkitMajorRE matches "cuda-toolkit-13-0" / "cuda-toolkit-12-4"
// etc.; not the unversioned "cuda-toolkit" metapackage.
var pkgToolkitMajorRE = regexp.MustCompile(`^cuda-toolkit-([0-9]+)(?:-[0-9]+)?$`)

// ExpectedMajor derives "the CUDA major this profile wants" from the
// explicit package name (e.g. cuda-toolkit-13-0 -> "13") or from the
// runfile filename (e.g. cuda_13.0.2_580.95.05_linux.run -> "13") or
// from the runfile URL's version segment.
//
// Returns "" when nothing pins a specific major — the caller treats
// that as "any major is acceptable".
func ExpectedMajor(packageName, runfileURL string) string {
	pkg := strings.TrimSpace(packageName)
	if m := pkgToolkitMajorRE.FindStringSubmatch(pkg); m != nil {
		return m[1]
	}
	url := strings.TrimSpace(runfileURL)
	if url == "" {
		return ""
	}
	// Pull the first "X.Y.Z" out of the URL path. Conservative: only
	// match a version that looks like NVIDIA's own pattern.
	re := regexp.MustCompile(`/cuda/([0-9]+)\.([0-9]+)\.([0-9]+)/`)
	if m := re.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	// Also tolerate "cuda_13.0.2_..._linux.run" in the URL path.
	re2 := regexp.MustCompile(`/cuda_([0-9]+)\.([0-9]+)\.([0-9]+)_`)
	if m := re2.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}

// MajorMatches reports whether a parsed nvcc release is compatible
// with the expected major. When expected is empty, any installed
// major satisfies. When both are present, they must match exactly.
func MajorMatches(actual NvccRelease, expected string) bool {
	if expected == "" {
		return actual.Major != ""
	}
	return actual.Major == expected
}
