package cuda

import "testing"

func TestResolveRunfileMaxAttempts_ProfileWins(t *testing.T) {
	if got := ResolveRunfileMaxAttempts(5, "9"); got != 5 {
		t.Errorf("profile knob should win; got %d want 5", got)
	}
}

func TestResolveRunfileMaxAttempts_EnvFallback(t *testing.T) {
	if got := ResolveRunfileMaxAttempts(0, "7"); got != 7 {
		t.Errorf("env override should kick in when profile=0; got %d want 7", got)
	}
}

func TestResolveRunfileMaxAttempts_Default(t *testing.T) {
	if got := ResolveRunfileMaxAttempts(0, ""); got != DefaultRunfileMaxAttempts {
		t.Errorf("default fallback; got %d want %d", got, DefaultRunfileMaxAttempts)
	}
}

func TestResolveRunfileMaxAttempts_RejectNonsenseEnv(t *testing.T) {
	if got := ResolveRunfileMaxAttempts(0, "twelve"); got != DefaultRunfileMaxAttempts {
		t.Errorf("non-numeric env should fall back to default; got %d", got)
	}
	if got := ResolveRunfileMaxAttempts(0, "0"); got != DefaultRunfileMaxAttempts {
		t.Errorf("non-positive env should fall back to default; got %d", got)
	}
	if got := ResolveRunfileMaxAttempts(0, "-3"); got != DefaultRunfileMaxAttempts {
		t.Errorf("negative env should fall back to default; got %d", got)
	}
}

func TestRunfileConstants(t *testing.T) {
	if RunfileTmpDir != "/var/tmp/clouddeploy-cuda" {
		t.Errorf("RunfileTmpDir moved unexpectedly; got %q", RunfileTmpDir)
	}
	if MinRunfileBytes < 1<<30 {
		t.Errorf("MinRunfileBytes %d too small; v2's HTML-error detection wants >= 1 GiB", MinRunfileBytes)
	}
}
