// Package runner is the only place in clouddeployctl that calls
// os/exec. Every subprocess invocation goes through Exec so we get
// uniform logging, timeouts, structured exit codes, secret
// redaction, and dry-run support.
//
// Design summary
// --------------
//
//  1. CommandSpec is plain data: Argv, Cwd, Env, Stdin, Sudo, Timeout,
//     LogFile, DryRun, RedactArgs, RedactEnvs. Mutating it is cheap.
//  2. Runner.Exec(ctx, spec) Result runs the command (or pretends to,
//     on DryRun), captures stdout/stderr (capped), tees them to LogFile
//     if set, and returns a typed Result.
//  3. Logs land under /var/log/clouddeploy/ by default. The runner
//     auto-creates the directory; on a developer host or CI without
//     write access to /var/log it falls back to ${TMPDIR}/clouddeploy/.
//  4. Secret redaction: when logging the command (the "RUN " line) or
//     the captured tail on error, indices listed in RedactArgs are
//     replaced with "<REDACTED>" and env vars whose names appear in
//     RedactEnvs are masked.
//
// What this package does NOT do:
//   - It does not retry on its own. Retry policy is the caller's
//     concern (see internal/cuda.RetryDecider for the canonical
//     example).
//   - It does not parse stdout/stderr. Callers parse.
//   - It does not enforce Sudo by re-execing under sudo; it asserts
//     that the calling process is already root when Sudo=true.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultLogDir is the canonical location for per-command logs.
// Created lazily by ensureLogDir.
const DefaultLogDir = "/var/log/clouddeploy"

// FallbackLogDir is used when DefaultLogDir is not writable (dev host,
// CI without sudo).
const FallbackLogDirEnv = "TMPDIR"

// MaxCaptureBytes caps the in-memory capture of stdout / stderr per
// stream. The full output is still tee'd to the log file if LogFile
// is set; only the captured tail is bounded.
const MaxCaptureBytes = 64 * 1024

// Redacted is the placeholder substituted into log output for args
// or env values that callers asked to hide.
const Redacted = "<REDACTED>"

// CommandSpec is the input to Exec.
type CommandSpec struct {
	// Argv is the program + args. Argv[0] is the program path or
	// name (resolved via exec.LookPath if it doesn't contain '/').
	// Argv must have at least one element.
	Argv []string

	// Cwd is the working directory. Empty = inherit from the calling
	// process.
	Cwd string

	// Env is appended to the current environment. Each entry is
	// "KEY=VALUE". Empty = inherit. To unset a variable, pass
	// "KEY=" (or use Env={"KEY="} and let the child see the empty
	// value).
	Env []string

	// Stdin is piped to the child's stdin. Empty = no stdin
	// redirection (child inherits /dev/null).
	Stdin string

	// Sudo asserts that the calling process is already root. When
	// true and the calling process is not root, Exec returns
	// ErrNotRoot without invoking the command.
	Sudo bool

	// Timeout caps the command's wall-clock time. Zero = no cap
	// (other than ctx.Done).
	Timeout time.Duration

	// LogFile is a path to append a full transcript of the command
	// (the "RUN" header + stdout + stderr + the "EXIT" footer). If
	// empty, Runner picks a default under DefaultLogDir based on
	// the first non-redacted arg. Set to "-" to suppress file
	// logging entirely.
	LogFile string

	// DryRun: log "WOULD RUN ..." and return success without
	// invoking the program.
	DryRun bool

	// RedactArgs is a list of 0-based indices into Argv whose values
	// should be masked in log output. Sentinel value -1 redacts the
	// final arg (useful for passwords on the command line).
	RedactArgs []int

	// RedactEnvs is a list of env var NAMES whose values are masked
	// in log output. Both Env entries and inherited env vars are
	// matched.
	RedactEnvs []string
}

// Result captures the outcome of a single Exec call.
type Result struct {
	Spec      CommandSpec
	ExitCode  int
	Err       error // non-nil on any non-zero exit, timeout, or invocation error
	Stdout    string
	Stderr    string
	Started   time.Time
	Finished  time.Time
	Duration  time.Duration
	DryRun    bool
	TimedOut  bool
	LogPath   string
	WasSudoed bool // true iff Sudo=true and the call succeeded the root check
}

// Common errors.
var (
	ErrEmptyArgv  = errors.New("runner: CommandSpec.Argv must have at least one element")
	ErrNotRoot    = errors.New("runner: CommandSpec.Sudo=true but the calling process is not root (euid != 0)")
	ErrLookupPath = errors.New("runner: could not resolve program in PATH")
	ErrTimedOut   = errors.New("runner: command exceeded its Timeout")
)

// Runner is the entry point. Exec is goroutine-safe.
type Runner struct {
	// LogDir is the parent directory for per-command log files when
	// CommandSpec.LogFile is empty. Defaults to DefaultLogDir.
	LogDir string

	// PrintToStdout, when true, also writes the "RUN ..." / "EXIT
	// ..." headers to os.Stdout. Useful for the CLI; off for tests.
	PrintToStdout bool

	mu sync.Mutex // serialises log-file opens; cheap
}

// New returns a Runner with sensible defaults.
func New() *Runner {
	return &Runner{LogDir: DefaultLogDir}
}

// Exec runs spec and returns Result. ctx.Done cancels the command
// (and is OR'd with spec.Timeout).
func (r *Runner) Exec(ctx context.Context, spec CommandSpec) (res Result) {
	res = Result{Spec: spec, Started: time.Now()}
	// Named return so the deferred mutation to Finished/Duration is
	// visible to the caller. Returning a value type without a named
	// return would copy res before the defer runs.
	defer func() {
		res.Finished = time.Now()
		res.Duration = res.Finished.Sub(res.Started)
	}()

	if len(spec.Argv) == 0 {
		res.Err = ErrEmptyArgv
		res.ExitCode = -1
		return res
	}

	// Sudo precheck. Skipped under DryRun since no actual command
	// runs - DryRun is meant to be inspectable by a non-root operator.
	if spec.Sudo && !spec.DryRun {
		if euid := geteuid(); euid != 0 {
			res.Err = fmt.Errorf("%w (euid=%d)", ErrNotRoot, euid)
			res.ExitCode = -1
			r.logLine(spec, "PRECHECK", fmt.Sprintf("Sudo required but euid=%d; refusing.", euid), res.LogPath)
			return res
		}
		res.WasSudoed = true
	}

	// Resolve LogFile.
	logPath := spec.LogFile
	if logPath == "" {
		logPath = r.defaultLogPath(spec)
	}
	if logPath == "-" {
		logPath = ""
	}
	res.LogPath = logPath

	header := r.formatRunHeader(spec)
	if r.PrintToStdout {
		fmt.Println(header)
	}
	r.appendLogLine(logPath, header)

	if spec.DryRun {
		res.DryRun = true
		res.ExitCode = 0
		footer := fmt.Sprintf("DRY-RUN EXIT 0 (would have invoked)")
		if r.PrintToStdout {
			fmt.Println(footer)
		}
		r.appendLogLine(logPath, footer)
		return res
	}

	// Resolve program path. If Argv[0] contains a separator, take
	// it verbatim; otherwise consult PATH.
	progPath := spec.Argv[0]
	if !strings.ContainsAny(progPath, "/\\") {
		resolved, err := exec.LookPath(progPath)
		if err != nil {
			res.Err = fmt.Errorf("%w: %s: %v", ErrLookupPath, progPath, err)
			res.ExitCode = -1
			r.appendLogLine(logPath, fmt.Sprintf("EXIT -1 (lookup failure: %v)", err))
			return res
		}
		progPath = resolved
	}

	// Build *exec.Cmd.
	cmdCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(cmdCtx, progPath, spec.Argv[1:]...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	if spec.Env != nil {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}

	// Capture + tee setup.
	stdoutCap := &capWriter{cap: MaxCaptureBytes}
	stderrCap := &capWriter{cap: MaxCaptureBytes}
	var stdoutW io.Writer = stdoutCap
	var stderrW io.Writer = stderrCap
	if logPath != "" {
		// Open + truncate-by-append; we keep the file across multiple
		// commands and let the operator rotate. Best-effort.
		if f := r.openLog(logPath); f != nil {
			defer f.Close()
			stdoutW = io.MultiWriter(stdoutCap, &prefixedWriter{w: f, prefix: "stdout: "})
			stderrW = io.MultiWriter(stderrCap, &prefixedWriter{w: f, prefix: "stderr: "})
		}
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	// Run.
	runErr := cmd.Run()
	res.Stdout = stdoutCap.String()
	res.Stderr = stderrCap.String()

	exitCode := 0
	switch {
	case runErr == nil:
		exitCode = 0
	case errors.Is(cmdCtx.Err(), context.DeadlineExceeded):
		exitCode = -1
		res.TimedOut = true
		res.Err = fmt.Errorf("%w (limit=%s)", ErrTimedOut, spec.Timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		exitCode = -1
		res.Err = fmt.Errorf("runner: context cancelled")
	default:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
		res.Err = runErr
	}
	res.ExitCode = exitCode

	footer := fmt.Sprintf("EXIT %d (duration=%s)", exitCode, time.Since(res.Started).Round(time.Millisecond))
	if res.TimedOut {
		footer = fmt.Sprintf("EXIT TIMEOUT (limit=%s)", spec.Timeout)
	}
	if r.PrintToStdout {
		fmt.Println(footer)
	}
	r.appendLogLine(logPath, footer)
	return res
}

// formatRunHeader returns the "RUN <cmd>" line written to the log
// file and (optionally) stdout. Redaction is applied here.
func (r *Runner) formatRunHeader(spec CommandSpec) string {
	args := redactArgv(spec.Argv, spec.RedactArgs)
	envRedacted := redactEnv(spec.Env, spec.RedactEnvs)
	prefix := "RUN"
	if spec.DryRun {
		prefix = "DRY-RUN"
	}
	cwd := ""
	if spec.Cwd != "" {
		cwd = " cwd=" + spec.Cwd
	}
	envStr := ""
	if len(envRedacted) > 0 {
		envStr = " env=[" + strings.Join(envRedacted, " ") + "]"
	}
	timeoutStr := ""
	if spec.Timeout > 0 {
		timeoutStr = " timeout=" + spec.Timeout.String()
	}
	return fmt.Sprintf("%s %s%s%s%s", prefix, strings.Join(args, " "), cwd, envStr, timeoutStr)
}

func redactArgv(argv []string, redact []int) []string {
	if len(redact) == 0 {
		return argv
	}
	out := make([]string, len(argv))
	copy(out, argv)
	last := len(argv) - 1
	for _, i := range redact {
		if i == -1 && last >= 0 {
			out[last] = Redacted
			continue
		}
		if i >= 0 && i < len(out) {
			out[i] = Redacted
		}
	}
	return out
}

func redactEnv(env []string, redactNames []string) []string {
	if len(env) == 0 {
		return nil
	}
	redact := make(map[string]bool, len(redactNames))
	for _, n := range redactNames {
		redact[n] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		name := kv[:eq]
		if redact[name] {
			out = append(out, name+"="+Redacted)
		} else {
			out = append(out, kv)
		}
	}
	return out
}

// defaultLogPath returns the per-command log path under LogDir or
// the fallback TMPDIR location if LogDir is not writable.
//
// Returns "" when LogDir == "-" so callers (tests; --log-dir=- on a
// future CLI flag) can opt out of file logging entirely. The
// caller-level CommandSpec.LogFile == "-" also opts out per call.
func (r *Runner) defaultLogPath(spec CommandSpec) string {
	dir := r.LogDir
	if dir == "-" {
		return ""
	}
	if dir == "" {
		dir = DefaultLogDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Fall back to TMPDIR/clouddeploy.
		tmp := os.TempDir()
		dir = filepath.Join(tmp, "clouddeploy")
		_ = os.MkdirAll(dir, 0o755)
	}
	base := filepath.Base(spec.Argv[0])
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "cmd"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("%s-%s.log", base, stamp))
}

// openLog opens the log file in append-or-create mode. Returns nil on
// any error so the caller can keep streaming to the capture buffers.
func (r *Runner) openLog(path string) *os.File {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// appendLogLine writes a single header line to the log file
// (separate from the stdout/stderr stream). Best-effort.
func (r *Runner) appendLogLine(path, line string) {
	if path == "" {
		return
	}
	f := r.openLog(path)
	if f == nil {
		return
	}
	defer f.Close()
	_, _ = io.WriteString(f, line+"\n")
}

// logLine writes a single line to the per-command log AND optionally
// stdout. Used for precheck failures (Sudo refused).
func (r *Runner) logLine(spec CommandSpec, kind, msg, path string) {
	line := fmt.Sprintf("%s %s", kind, msg)
	if r.PrintToStdout {
		fmt.Println(line)
	}
	r.appendLogLine(path, line)
}

// -----------------------------------------------------------------------------
// capWriter caps in-memory captures of stdout/stderr at MaxCaptureBytes.
// -----------------------------------------------------------------------------

type capWriter struct {
	cap int
	buf bytes.Buffer
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.buf.Len() >= c.cap {
		return len(p), nil
	}
	remaining := c.cap - c.buf.Len()
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capWriter) String() string { return c.buf.String() }

// prefixedWriter writes each line with a fixed prefix. Used to tag
// stdout vs stderr in the unified log file.
type prefixedWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	// Split on newlines; emit each as a separate line with prefix.
	// We don't have to be efficient here - command output volume in
	// a deploy is small.
	start := 0
	for i, c := range b {
		if c == '\n' {
			line := p.prefix + string(b[start:i+1])
			if _, err := io.WriteString(p.w, line); err != nil {
				return 0, err
			}
			start = i + 1
		}
	}
	if start < len(b) {
		// Incomplete line (no trailing newline); flush anyway.
		line := p.prefix + string(b[start:]) + "\n"
		if _, err := io.WriteString(p.w, line); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}
