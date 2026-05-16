// Package cuda owns CUDA toolkit policy: whether to install it,
// whether to fail the deploy on toolkit error, and how to suppress
// pointless retries when the runfile keeps producing the same bad
// artifact.
//
// Motivating failure mode (from the v2 RTX 5090 deploy): the CUDA
// runfile was downloaded five times in a row, each download produced
// an identical 4.3 GB artifact with identical SHA-256
// (81a5d0d0870ba2022efb0a531dcc60adbdc2bbff7b3ef19d6fd6d8105406c775),
// and each `--check` run failed with an internal MD5 mismatch. The v2
// retry loop turned a permanent error into a 25-minute timeout.
//
// The correct behaviour: if the same SHA fails `--check` once, fail
// the runfile install immediately. Try apt fallback if available; if
// that also fails, branch on cuda.mode.
//
//	mode = none      ->  never attempt CUDA. doctor reports "skipped".
//	mode = optional  ->  try; if it fails, mark skipped and continue.
//	                     The KMS+NVENC+HDR path does not need CUDA.
//	mode = required  ->  try; if it fails, fail the deploy.
//
// The hdr-4k120 profile uses mode=none because Sunshine's CUDA/NvFBC
// module isn't compiled and the KMS capture path doesn't touch CUDA.
package cuda

import (
	"fmt"
	"strings"
)

// Mode is the policy a profile selects.
type Mode string

const (
	ModeNone     Mode = "none"
	ModeOptional Mode = "optional"
	ModeRequired Mode = "required"
)

// Valid returns true for known modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeNone, ModeOptional, ModeRequired:
		return true
	}
	return false
}

// ParseMode parses a string into a Mode, with case insensitivity.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "":
		return ModeNone, nil
	case "optional":
		return ModeOptional, nil
	case "required":
		return ModeRequired, nil
	}
	return "", fmt.Errorf("cuda: unknown mode %q (want none|optional|required)", s)
}

// PlanResult is what Plan returns for a given mode. It is meant to be
// printed by `doctor cuda` and consulted by the cuda phase.
type PlanResult struct {
	Mode               Mode
	WillAttemptInstall bool
	FailsDeployOnError bool
	Rationale          string
}

// Plan turns a mode into a deploy plan. Pure function; unit-tested.
func Plan(m Mode) PlanResult {
	switch m {
	case ModeNone:
		return PlanResult{
			Mode:               ModeNone,
			WillAttemptInstall: false,
			FailsDeployOnError: false,
			Rationale: "Profile selects cuda.mode=none. KMS+NVENC+AV1 HDR streaming does not " +
				"require the CUDA toolkit, so we skip the install entirely.",
		}
	case ModeOptional:
		return PlanResult{
			Mode:               ModeOptional,
			WillAttemptInstall: true,
			FailsDeployOnError: false,
			Rationale: "Profile selects cuda.mode=optional. Will try apt + runfile fallback. " +
				"If both fail (e.g. repeated runfile --check MD5 mismatch with identical SHA), " +
				"deploy continues with cuda marked skipped/failed_nonfatal.",
		}
	case ModeRequired:
		return PlanResult{
			Mode:               ModeRequired,
			WillAttemptInstall: true,
			FailsDeployOnError: true,
			Rationale: "Profile selects cuda.mode=required. Will try apt + runfile fallback. " +
				"If both fail, deploy halts with a clear error.",
		}
	}
	return PlanResult{
		Mode:      m,
		Rationale: "Unknown mode; treating as none.",
	}
}

// Attempt records one outcome of a runfile install attempt. The fields
// are what the retry-suppressor consults to decide whether to retry.
type Attempt struct {
	URL        string
	Size       int64
	SHA256     string
	CheckOK    bool   // did `cuda_*_linux.run --check` succeed?
	InstallOK  bool   // did the actual install succeed?
	ErrorTail  string // last few lines of stderr; surfaced in logs
}

// RetryDecider holds the history of runfile attempts and answers
// "should we redownload + retry?" deterministically. It is stateful
// but holds no I/O - the cuda phase wires its real attempts into this
// struct and consults the answer.
//
// Decision rule:
//
//	If we already saw a (sha256,size) pair that failed --check, and
//	the next download would produce the same (sha256,size) pair, the
//	answer is "no, that's a permanent failure of the artifact." This
//	is the v2 regression we are preventing.
type RetryDecider struct {
	attempts []Attempt
}

// Record adds an attempt to the history.
func (r *RetryDecider) Record(a Attempt) {
	r.attempts = append(r.attempts, a)
}

// Attempts returns a copy of the history. Read-only for callers.
func (r *RetryDecider) Attempts() []Attempt {
	out := make([]Attempt, len(r.attempts))
	copy(out, r.attempts)
	return out
}

// ShouldRetry returns true when the next attempt has a plausible
// chance of succeeding. The current implementation looks at:
//
//   - If any previous attempt with the same SHA256 already failed
//     CheckOK: refuse to retry.
//   - If three consecutive attempts ALL failed --check, even with
//     different SHAs: refuse (something else is wrong, the network
//     keeps corrupting, etc).
//   - Otherwise: retry.
func (r *RetryDecider) ShouldRetry(nextSHA256 string, nextSize int64) (bool, string) {
	if nextSHA256 != "" {
		for _, a := range r.attempts {
			if a.SHA256 == nextSHA256 && a.Size == nextSize && !a.CheckOK {
				return false, fmt.Sprintf(
					"refusing to retry: same artifact (sha256=%s size=%d) already failed --check on attempt %d. "+
						"Re-downloading the same bytes will produce the same failure.",
					nextSHA256, nextSize, indexOfMatch(r.attempts, nextSHA256, nextSize)+1)
			}
		}
	}
	if consecutiveCheckFailures(r.attempts) >= 3 {
		return false, "refusing to retry: 3 consecutive runfile --check failures. " +
			"Something is consistently corrupting the download or the upstream artifact is bad."
	}
	return true, ""
}

func indexOfMatch(attempts []Attempt, sha string, size int64) int {
	for i, a := range attempts {
		if a.SHA256 == sha && a.Size == size {
			return i
		}
	}
	return -1
}

func consecutiveCheckFailures(attempts []Attempt) int {
	count := 0
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].CheckOK {
			return count
		}
		count++
	}
	return count
}
