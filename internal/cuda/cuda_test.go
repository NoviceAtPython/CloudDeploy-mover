package cuda

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", ModeNone, false},
		{"none", ModeNone, false},
		{"None", ModeNone, false},
		{" NONE ", ModeNone, false},
		{"optional", ModeOptional, false},
		{"required", ModeRequired, false},
		{"REQUIRED", ModeRequired, false},
		{"yes", "", true},
		{"true", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseMode(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("expected error for %q, got mode %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		mode         Mode
		willAttempt  bool
		failsOnError bool
		rationaleHas string
	}{
		{ModeNone, false, false, "skip"},
		{ModeOptional, true, false, "continues"},
		{ModeRequired, true, true, "halts"},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			p := Plan(c.mode)
			if p.WillAttemptInstall != c.willAttempt {
				t.Errorf("WillAttemptInstall: got %v want %v", p.WillAttemptInstall, c.willAttempt)
			}
			if p.FailsDeployOnError != c.failsOnError {
				t.Errorf("FailsDeployOnError: got %v want %v", p.FailsDeployOnError, c.failsOnError)
			}
			if c.rationaleHas != "" && !strings.Contains(strings.ToLower(p.Rationale), c.rationaleHas) {
				t.Errorf("Rationale missing %q: %q", c.rationaleHas, p.Rationale)
			}
		})
	}
}

// TestRetryDecider_SameShaSameSize is the v2 regression we are
// preventing. The CUDA runfile produced an identical 4.3 GB blob with
// identical SHA-256 every download, and `--check` failed every time.
// v2 retried five times for no benefit. v3 must refuse the second
// attempt.
func TestRetryDecider_SameShaSameSize(t *testing.T) {
	const sha = "81a5d0d0870ba2022efb0a531dcc60adbdc2bbff7b3ef19d6fd6d8105406c775"
	const size int64 = 4328066903
	var r RetryDecider
	r.Record(Attempt{URL: "https://nvidia/cuda.run", Size: size, SHA256: sha, CheckOK: false})

	ok, reason := r.ShouldRetry(sha, size)
	if ok {
		t.Fatalf("expected refuse-to-retry; got retry=true reason=%q", reason)
	}
	if !strings.Contains(reason, "already failed --check") {
		t.Errorf("reason should explain the same-SHA logic: %q", reason)
	}
}

func TestRetryDecider_DifferentShaAllowed(t *testing.T) {
	var r RetryDecider
	r.Record(Attempt{URL: "u", Size: 1, SHA256: "aaa", CheckOK: false})
	ok, _ := r.ShouldRetry("bbb", 2)
	if !ok {
		t.Fatalf("a different SHA + size should be allowed to retry once")
	}
}

func TestRetryDecider_ThreeConsecutiveCheckFailuresGiveUp(t *testing.T) {
	var r RetryDecider
	r.Record(Attempt{SHA256: "a", Size: 1, CheckOK: false})
	r.Record(Attempt{SHA256: "b", Size: 2, CheckOK: false})
	r.Record(Attempt{SHA256: "c", Size: 3, CheckOK: false})
	ok, reason := r.ShouldRetry("d", 4)
	if ok {
		t.Fatalf("expected give-up after 3 consecutive --check failures; reason=%q", reason)
	}
	if !strings.Contains(reason, "consecutive") {
		t.Errorf("reason should mention consecutive failures: %q", reason)
	}
}

func TestRetryDecider_AfterSuccessfulCheckCounterResets(t *testing.T) {
	var r RetryDecider
	r.Record(Attempt{SHA256: "a", Size: 1, CheckOK: false})
	r.Record(Attempt{SHA256: "b", Size: 2, CheckOK: false})
	r.Record(Attempt{SHA256: "c", Size: 3, CheckOK: true, InstallOK: false}) // check OK, install crashed
	r.Record(Attempt{SHA256: "d", Size: 4, CheckOK: false})
	ok, reason := r.ShouldRetry("e", 5)
	if !ok {
		t.Errorf("counter should reset after a successful --check; got refuse with reason=%q", reason)
	}
}

func TestRetryDecider_FirstAttemptAlwaysAllowed(t *testing.T) {
	var r RetryDecider
	ok, _ := r.ShouldRetry("anything", 0)
	if !ok {
		t.Errorf("first attempt should always be allowed")
	}
}

func TestRetryDecider_AttemptsIsCopy(t *testing.T) {
	var r RetryDecider
	r.Record(Attempt{SHA256: "a", Size: 1, CheckOK: true, InstallOK: true})
	got := r.Attempts()
	got[0].SHA256 = "tampered"
	if r.Attempts()[0].SHA256 != "a" {
		t.Errorf("Attempts() must return a defensive copy; original was mutated")
	}
}
