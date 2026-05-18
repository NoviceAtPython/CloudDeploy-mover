package cuda

import "fmt"

// RunfileTmpDir is the disk-backed staging directory the runfile
// install uses. v2 picked /var/tmp because /tmp is often tmpfs on
// cloud images and the ~4 GiB self-extract exhausts RAM.
const RunfileTmpDir = "/var/tmp/clouddeploy-cuda"

// MinRunfileBytes is the lower-bound size v2 uses to detect HTML
// error pages disguised as runfile downloads. The CUDA 13.0.2
// toolkit-only runfile is ~4 GiB; anything under 1 GiB is almost
// certainly an HTTP error page.
const MinRunfileBytes int64 = 1 << 30 // 1 GiB

// DefaultRunfileMaxAttempts mirrors v2's
// `CLOUDDEPLOY_CUDA_RUNFILE_MAX_ATTEMPTS:-3` default.
const DefaultRunfileMaxAttempts = 3

// ResolveRunfileMaxAttempts picks the effective cap. Profile knob
// wins; then the env override; then DefaultRunfileMaxAttempts.
// envValue is passed in by the caller (we keep this pure for tests).
func ResolveRunfileMaxAttempts(profile int, envValue string) int {
	if profile > 0 {
		return profile
	}
	if envValue != "" {
		// Try a 1..N parse. We refuse to use a non-positive override.
		var n int
		_, err := fmt.Sscanf(envValue, "%d", &n)
		if err == nil && n > 0 {
			return n
		}
	}
	return DefaultRunfileMaxAttempts
}
