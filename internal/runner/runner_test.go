package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is the canonical Go pattern for cross-platform
// subprocess testing. The test binary re-execs itself with
// GO_TEST_HELPER_PROCESS=1 and a BEHAVIOR env var that selects which
// scripted behaviour to run. Tests build a CommandSpec that invokes
// os.Args[0] (the test binary) with -test.run=TestHelperProcess and
// the desired BEHAVIOR.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER_PROCESS") != "1" {
		return
	}
	behavior := os.Getenv("BEHAVIOR")
	switch behavior {
	case "echo-args":
		// Print the user-supplied args after our own.
		// Real args: [-test.run=TestHelperProcess -- args...]
		for i, a := range os.Args {
			if a == "--" {
				for _, ua := range os.Args[i+1:] {
					_, _ = os.Stdout.WriteString(ua + "\n")
				}
				break
			}
		}
		os.Exit(0)
	case "exit-2":
		_, _ = os.Stderr.WriteString("simulated failure\n")
		os.Exit(2)
	case "sleep":
		dur, _ := time.ParseDuration(os.Getenv("SLEEP_FOR"))
		time.Sleep(dur)
		os.Exit(0)
	case "print-env":
		want := os.Getenv("TEST_ECHO_VAR")
		_, _ = os.Stdout.WriteString("ECHOED=" + want + "\n")
		os.Exit(0)
	case "print-cwd":
		wd, _ := os.Getwd()
		_, _ = os.Stdout.WriteString("CWD=" + wd + "\n")
		os.Exit(0)
	case "echo-stdin":
		b, _ := io.ReadAll(os.Stdin)
		_, _ = os.Stdout.Write(b)
		os.Exit(0)
	default:
		_, _ = os.Stderr.WriteString("unknown BEHAVIOR=" + behavior + "\n")
		os.Exit(99)
	}
}

// helperSpec returns a CommandSpec that re-execs the test binary
// with -test.run=TestHelperProcess and the given BEHAVIOR.
func helperSpec(behavior string, extraArgs ...string) CommandSpec {
	argv := []string{os.Args[0], "-test.run=TestHelperProcess", "--"}
	argv = append(argv, extraArgs...)
	return CommandSpec{
		Argv: argv,
		Env:  []string{"GO_TEST_HELPER_PROCESS=1", "BEHAVIOR=" + behavior},
		// LogFile=- suppresses file logging entirely; tests don't
		// care about the log file and don't want to litter $TMPDIR
		// across runs.
		LogFile: "-",
	}
}

func newTestRunner() *Runner {
	return &Runner{LogDir: "-", PrintToStdout: false}
}

func TestExec_Success(t *testing.T) {
	r := newTestRunner()
	res := r.Exec(context.Background(), helperSpec("echo-args", "hello", "world"))
	if res.Err != nil {
		t.Fatalf("expected no error, got: %v (stderr: %s)", res.Err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: got %d want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") || !strings.Contains(res.Stdout, "world") {
		t.Errorf("stdout missing user args: %q", res.Stdout)
	}
	if res.Duration <= 0 {
		t.Errorf("expected positive Duration, got %v", res.Duration)
	}
}

func TestExec_NonZeroExit(t *testing.T) {
	r := newTestRunner()
	res := r.Exec(context.Background(), helperSpec("exit-2"))
	if res.ExitCode != 2 {
		t.Errorf("ExitCode: got %d want 2", res.ExitCode)
	}
	if res.Err == nil {
		t.Errorf("expected Err to be non-nil on non-zero exit")
	}
	if !strings.Contains(res.Stderr, "simulated failure") {
		t.Errorf("stderr capture missing expected content: %q", res.Stderr)
	}
}

func TestExec_Timeout(t *testing.T) {
	r := newTestRunner()
	spec := helperSpec("sleep")
	spec.Env = append(spec.Env, "SLEEP_FOR=5s")
	spec.Timeout = 200 * time.Millisecond

	res := r.Exec(context.Background(), spec)
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got false (err=%v)", res.Err)
	}
	if !errors.Is(res.Err, ErrTimedOut) {
		t.Errorf("expected Err to wrap ErrTimedOut, got: %v", res.Err)
	}
	// On Linux a sleep killed via context terminates within ~10ms
	// after the deadline; allow generous overhead.
	if res.Duration > 5*time.Second {
		t.Errorf("Duration looks wrong: %v", res.Duration)
	}
}

func TestExec_ContextCancel(t *testing.T) {
	r := newTestRunner()
	ctx, cancel := context.WithCancel(context.Background())
	spec := helperSpec("sleep")
	spec.Env = append(spec.Env, "SLEEP_FOR=5s")

	// Cancel shortly after start.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res := r.Exec(ctx, spec)
	if res.Err == nil {
		t.Errorf("expected non-nil Err on ctx cancel")
	}
	if res.ExitCode == 0 {
		t.Errorf("expected non-zero exit on ctx cancel")
	}
}

func TestExec_Env(t *testing.T) {
	r := newTestRunner()
	spec := helperSpec("print-env")
	spec.Env = append(spec.Env, "TEST_ECHO_VAR=hello_from_test")
	res := r.Exec(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if !strings.Contains(res.Stdout, "ECHOED=hello_from_test") {
		t.Errorf("env not passed; stdout=%q", res.Stdout)
	}
}

func TestExec_Cwd(t *testing.T) {
	r := newTestRunner()
	dir := t.TempDir()
	spec := helperSpec("print-cwd")
	spec.Cwd = dir
	res := r.Exec(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	// On macOS /tmp is a symlink to /private/tmp; on Windows the
	// case may differ. Compare with EvalSymlinks-equivalent: look
	// for the base name.
	wantBase := filepath.Base(dir)
	if !strings.Contains(res.Stdout, wantBase) {
		t.Errorf("cwd not honoured; stdout=%q want substring %q", res.Stdout, wantBase)
	}
}

func TestExec_Stdin(t *testing.T) {
	r := newTestRunner()
	spec := helperSpec("echo-stdin")
	spec.Stdin = "input line one\ninput line two\n"
	res := r.Exec(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if !strings.Contains(res.Stdout, "input line one") {
		t.Errorf("stdin not echoed; stdout=%q", res.Stdout)
	}
}

func TestExec_DryRun(t *testing.T) {
	r := newTestRunner()
	spec := helperSpec("exit-2")
	spec.DryRun = true
	res := r.Exec(context.Background(), spec)
	if !res.DryRun {
		t.Errorf("expected DryRun=true")
	}
	if res.ExitCode != 0 {
		t.Errorf("dry-run should report exit 0; got %d", res.ExitCode)
	}
	if res.Err != nil {
		t.Errorf("dry-run should not surface an error; got %v", res.Err)
	}
}

func TestExec_LookPathFailure(t *testing.T) {
	r := newTestRunner()
	spec := CommandSpec{
		Argv:    []string{"clouddeploy-no-such-tool-xyzzy"},
		LogFile: "-",
	}
	res := r.Exec(context.Background(), spec)
	if res.Err == nil || !errors.Is(res.Err, ErrLookupPath) {
		t.Errorf("expected ErrLookupPath, got: %v", res.Err)
	}
}

func TestExec_EmptyArgv(t *testing.T) {
	r := newTestRunner()
	res := r.Exec(context.Background(), CommandSpec{LogFile: "-"})
	if !errors.Is(res.Err, ErrEmptyArgv) {
		t.Errorf("expected ErrEmptyArgv, got: %v", res.Err)
	}
}

func TestExec_SudoRefusedWhenNotRoot(t *testing.T) {
	// On both Linux and Windows, the test process is not root /
	// returns euid -1, so Sudo=true must refuse.
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("running as root; this test asserts non-root behaviour")
	}
	r := newTestRunner()
	spec := helperSpec("echo-args", "hi")
	spec.Sudo = true
	res := r.Exec(context.Background(), spec)
	if !errors.Is(res.Err, ErrNotRoot) {
		t.Errorf("expected ErrNotRoot, got: %v", res.Err)
	}
	if res.WasSudoed {
		t.Errorf("WasSudoed should be false when refused")
	}
}

func TestRedactArgv(t *testing.T) {
	in := []string{"echo", "secret", "public"}
	out := redactArgv(in, []int{1})
	if out[1] != Redacted {
		t.Errorf("expected index 1 redacted, got %v", out)
	}
	if out[0] != "echo" || out[2] != "public" {
		t.Errorf("other indices changed: %v", out)
	}

	// Sentinel -1 = last arg.
	out = redactArgv(in, []int{-1})
	if out[2] != Redacted {
		t.Errorf("expected last arg redacted, got %v", out)
	}

	// Original slice untouched.
	if in[1] != "secret" {
		t.Errorf("original slice was mutated: %v", in)
	}
}

func TestRedactEnv(t *testing.T) {
	in := []string{"FOO=bar", "PASSWORD=hunter2", "ANOTHER=ok"}
	out := redactEnv(in, []string{"PASSWORD"})
	if got := out[1]; got != "PASSWORD="+Redacted {
		t.Errorf("PASSWORD should be redacted, got %q", got)
	}
	if out[0] != "FOO=bar" || out[2] != "ANOTHER=ok" {
		t.Errorf("non-redacted env mutated: %v", out)
	}
}

func TestFormatRunHeader_HasRedaction(t *testing.T) {
	r := newTestRunner()
	spec := CommandSpec{
		Argv:       []string{"login", "--user", "alice", "--password", "hunter2"},
		Env:        []string{"FOO=bar", "DB_PASSWORD=correct-horse"},
		RedactArgs: []int{-1},
		RedactEnvs: []string{"DB_PASSWORD"},
		Timeout:    5 * time.Second,
	}
	got := r.formatRunHeader(spec)
	if strings.Contains(got, "hunter2") {
		t.Errorf("argv redaction failed; header contains password: %q", got)
	}
	if strings.Contains(got, "correct-horse") {
		t.Errorf("env redaction failed; header contains password: %q", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Errorf("expected redacted placeholder in header: %q", got)
	}
	if !strings.Contains(got, "timeout=5s") {
		t.Errorf("timeout missing from header: %q", got)
	}
}

func TestExec_LogFileWritten(t *testing.T) {
	r := newTestRunner()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "exec.log")
	spec := helperSpec("echo-args", "hello", "logged")
	spec.LogFile = logPath
	res := r.Exec(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "RUN") {
		t.Errorf("log missing RUN header: %s", text)
	}
	if !strings.Contains(text, "EXIT 0") {
		t.Errorf("log missing EXIT footer: %s", text)
	}
	if !strings.Contains(text, "hello") || !strings.Contains(text, "logged") {
		t.Errorf("log missing stdout tee: %s", text)
	}
}

// Compile-time check that *Runner satisfies the obvious "has Exec"
// shape callers can rely on without depending on this concrete type.
var _ interface {
	Exec(ctx context.Context, spec CommandSpec) Result
} = (*Runner)(nil)

// Sanity that exec.ExitError still has ExitCode; if Go ever removes
// it we want a compile-time signal.
var _ = (&exec.ExitError{}).ExitCode
