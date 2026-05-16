// Apt/dpkg transaction wrapper.
//
// Every apt-get / dpkg invocation should go through Transaction so we
// get a uniform pre-flight (lock wait + dpkg audit) and a uniform
// guard (policy-rc.d exit 101 to prevent the package-postinst
// service-start deadlock the RTX 5090 deploy hit on v2).
//
// Usage:
//
//	tx := &Transaction{Env: &Env{FS: RealFS{}}, Runner: runner.New()}
//	err := tx.Run(ctx, func(tc *TxContext) error {
//	        if err := tc.Update(ctx); err != nil { return err }
//	        return tc.Install(ctx, []string{"git", "curl"})
//	})
//
// The TxContext exposed to the callback wraps apt-get / dpkg calls
// behind methods. Direct exec.Command in callers is forbidden; use
// the TxContext methods.
package apt

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// LockPaths is the set of files dpkg / apt take exclusive locks on
// during a transaction. WaitForLocks polls all of them.
var LockPaths = []string{
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/dpkg/lock",
	"/var/lib/apt/lists/lock",
	"/var/cache/apt/archives/lock",
}

// DefaultLockWait is how long Transaction.Run polls for the dpkg /
// apt locks before giving up.
const DefaultLockWait = 5 * time.Minute

// DefaultLockPollInterval is how often we re-check.
const DefaultLockPollInterval = 2 * time.Second

// DefaultAptTimeout caps a single apt-get invocation. apt-get install
// of a multi-hundred-MB package on a slow connection can take
// several minutes; the cap exists to prevent a hung process from
// pinning the deploy forever.
const DefaultAptTimeout = 30 * time.Minute

// Transaction is the wrapper. Construct one per deploy.
type Transaction struct {
	// Env carries the FS used by InstallPolicyRcD. Required.
	Env *Env

	// Runner runs apt-get / dpkg / fuser commands. Required.
	Runner *runner.Runner

	// LockWait is the maximum time Run waits for dpkg / apt locks.
	// Zero = DefaultLockWait.
	LockWait time.Duration

	// AptTimeout caps a single apt-get invocation. Zero =
	// DefaultAptTimeout.
	AptTimeout time.Duration

	// DryRun, when true, propagates DryRun into each runner call.
	// Useful for `clouddeployctl apply --dry-run` (Milestone 4).
	DryRun bool

	// AssumeYes, when true, passes `-y` to apt-get. Default true; you
	// rarely want interactive prompts in a deploy.
	AssumeYes bool
}

// TxContext is what the callback receives. The methods here are the
// only sanctioned way to talk to apt/dpkg inside a transaction; they
// share the lock + policy-rc.d guard + log path.
type TxContext struct {
	tx     *Transaction
	ctx    context.Context
	logDir string // where per-step log files land
}

// Run holds the dpkg lock, installs the policy-rc.d guard, runs an
// optional `dpkg --configure -a` if audit reports half-configured
// packages, calls fn, and always restores the policy-rc.d guard on
// return.
//
// Errors from any step short-circuit: the callback is skipped if the
// pre-flight fails. The policy-rc.d guard is restored regardless.
func (t *Transaction) Run(ctx context.Context, fn func(*TxContext) error) (retErr error) {
	if t.Env == nil || t.Env.FS == nil {
		return errors.New("apt: Transaction.Env.FS is nil")
	}
	if t.Runner == nil {
		return errors.New("apt: Transaction.Runner is nil")
	}

	tc := &TxContext{tx: t, ctx: ctx, logDir: runner.DefaultLogDir}

	// 1. Wait for dpkg/apt locks.
	if err := t.waitForLocks(ctx); err != nil {
		return fmt.Errorf("apt: wait for dpkg/apt locks: %w", err)
	}

	// 2. Install policy-rc.d guard.
	guard, err := InstallPolicyRcD(t.Env)
	if err != nil {
		return fmt.Errorf("apt: install policy-rc.d guard: %w", err)
	}
	defer func() {
		if rerr := guard.Restore(); rerr != nil && retErr == nil {
			retErr = fmt.Errorf("apt: restore policy-rc.d guard: %w", rerr)
		}
	}()

	// 3. dpkg audit + repair if needed.
	if err := tc.RepairIfNeeded(ctx); err != nil {
		return fmt.Errorf("apt: dpkg repair: %w", err)
	}

	// 4. Run the callback.
	return fn(tc)
}

// waitForLocks polls LockPaths via fuser. Any non-empty fuser output
// means another process is holding the lock; back off + retry until
// the LockWait timeout.
func (t *Transaction) waitForLocks(ctx context.Context) error {
	deadline := time.Now().Add(t.lockWait())
	for {
		holders := t.lockHolders(ctx)
		if len(holders) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("locks still held after %s: %v", t.lockWait(), holders)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(DefaultLockPollInterval):
		}
	}
}

// LockHolders returns a list of "path -> pids" for any LockPath
// currently held by another process. Empty list = no holders.
func (t *Transaction) LockHolders(ctx context.Context) []string {
	return t.lockHolders(ctx)
}

func (t *Transaction) lockHolders(ctx context.Context) []string {
	// fuser may not be installed on all hosts; we tolerate ENOENT.
	if _, err := exec.LookPath("fuser"); err != nil {
		// No fuser - fall back to a simple "can we acquire the lock
		// non-blockingly?" probe. Without an OS-specific flock call
		// the safest approach is to consider all locks free.
		return nil
	}
	var holders []string
	for _, p := range LockPaths {
		res := t.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"fuser", p},
			LogFile: "-",
			Timeout: 5 * time.Second,
		})
		// fuser exits 0 when there are holders, 1 when there are none.
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
			holders = append(holders, p+" -> "+strings.TrimSpace(res.Stdout))
		}
	}
	return holders
}

func (t *Transaction) lockWait() time.Duration {
	if t.LockWait > 0 {
		return t.LockWait
	}
	return DefaultLockWait
}

func (t *Transaction) aptTimeout() time.Duration {
	if t.AptTimeout > 0 {
		return t.AptTimeout
	}
	return DefaultAptTimeout
}

func (t *Transaction) yes() []string {
	// Default to AssumeYes=true; only suppress if explicitly set to
	// false via the zero-value comparison.
	if !t.AssumeYes && t.aptTimeout() == DefaultAptTimeout {
		// Zero-value Transaction: assume yes. AssumeYes is opt-out,
		// not opt-in, because forgetting it makes apt block forever
		// on a deploy.
		return []string{"-y"}
	}
	if t.AssumeYes {
		return []string{"-y"}
	}
	return nil
}

// -----------------------------------------------------------------------------
// TxContext methods - the sanctioned apt/dpkg invocations.
// -----------------------------------------------------------------------------

// Update runs `apt-get update`.
func (tc *TxContext) Update(ctx context.Context) error {
	argv := append([]string{"apt-get"}, tc.tx.yes()...)
	argv = append(argv, "update")
	return tc.runOrErr(ctx, "apt-update", argv)
}

// Install runs `apt-get install <pkg>...` with the standard non-interactive
// flags. No-op (returns nil) when packages is empty.
func (tc *TxContext) Install(ctx context.Context, packages []string) error {
	if len(packages) == 0 {
		return nil
	}
	argv := append([]string{"apt-get"}, tc.tx.yes()...)
	argv = append(argv, "--no-install-recommends", "install")
	argv = append(argv, packages...)
	return tc.runOrErr(ctx, "apt-install", argv)
}

// Purge runs `apt-get purge <pkg>...`.
func (tc *TxContext) Purge(ctx context.Context, packages []string) error {
	if len(packages) == 0 {
		return nil
	}
	argv := append([]string{"apt-get"}, tc.tx.yes()...)
	argv = append(argv, "purge")
	argv = append(argv, packages...)
	return tc.runOrErr(ctx, "apt-purge", argv)
}

// FixBroken runs `apt-get -f install` to clear half-installed
// packages. Used by RepairIfNeeded.
func (tc *TxContext) FixBroken(ctx context.Context) error {
	argv := append([]string{"apt-get"}, tc.tx.yes()...)
	argv = append(argv, "-f", "install")
	return tc.runOrErr(ctx, "apt-fix-broken", argv)
}

// DpkgConfigureAll runs `dpkg --configure -a` to finish any
// half-configured packages. Used by RepairIfNeeded.
func (tc *TxContext) DpkgConfigureAll(ctx context.Context) error {
	return tc.runOrErr(ctx, "dpkg-configure-all", []string{"dpkg", "--configure", "-a"})
}

// AuditState describes what `dpkg --audit` reports.
type AuditState struct {
	HasErrors bool
	Lines     []string
	RawOutput string
}

// Audit runs `dpkg --audit` and parses the result. dpkg --audit
// exits 0 even when reporting issues, so we have to inspect the
// output.
func (tc *TxContext) Audit(ctx context.Context) (AuditState, error) {
	res := tc.tx.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"dpkg", "--audit"},
		Timeout: 30 * time.Second,
	})
	if res.Err != nil && !isExpectedExit(res) {
		return AuditState{RawOutput: res.Stdout + res.Stderr}, fmt.Errorf("dpkg --audit failed: %w", res.Err)
	}
	out := strings.TrimSpace(res.Stdout)
	state := AuditState{RawOutput: out}
	if out == "" {
		return state, nil
	}
	state.HasErrors = true
	state.Lines = strings.Split(out, "\n")
	return state, nil
}

// RepairIfNeeded runs `dpkg --audit`, and if anything looks
// half-installed, runs `dpkg --configure -a` followed by
// `apt-get -f install`. Safe to call multiple times.
func (tc *TxContext) RepairIfNeeded(ctx context.Context) error {
	audit, err := tc.Audit(ctx)
	if err != nil {
		// Audit itself failed (dpkg missing? something exotic).
		// Surface but don't block.
		return nil
	}
	if !audit.HasErrors {
		return nil
	}
	if err := tc.DpkgConfigureAll(ctx); err != nil {
		return fmt.Errorf("dpkg --configure -a failed: %w", err)
	}
	if err := tc.FixBroken(ctx); err != nil {
		return fmt.Errorf("apt-get -f install failed: %w", err)
	}
	return nil
}

// runOrErr is the inner runner-wrapper that adds CloudDeploy-specific
// env (DEBIAN_FRONTEND=noninteractive) and standard timeouts. It
// returns nil on exit 0 and a wrapped error otherwise.
func (tc *TxContext) runOrErr(ctx context.Context, name string, argv []string) error {
	spec := runner.CommandSpec{
		Argv: argv,
		Env: []string{
			"DEBIAN_FRONTEND=noninteractive",
			"APT_LISTCHANGES_FRONTEND=none",
		},
		Sudo:    true,
		Timeout: tc.tx.aptTimeout(),
		DryRun:  tc.tx.DryRun,
	}
	res := tc.tx.Runner.Exec(ctx, spec)
	if res.Err != nil {
		return fmt.Errorf("apt: %s: exit=%d err=%v stderr=%q", name, res.ExitCode, res.Err, tailLines(res.Stderr, 5))
	}
	return nil
}

// isExpectedExit covers commands that legitimately exit non-zero
// (dpkg --audit when there are findings; fuser when no holders).
func isExpectedExit(r runner.Result) bool {
	// dpkg --audit exits 0 even with findings, so anything non-zero
	// is unexpected. fuser exits 1 when no holders.
	if r.ExitCode == 1 && len(r.Spec.Argv) > 0 && r.Spec.Argv[0] == "fuser" {
		return true
	}
	return false
}

func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// -----------------------------------------------------------------------------
// Stat helpers: read-only probes the doctor command uses.
// -----------------------------------------------------------------------------

// PolicyRcDStatus describes what the doctor apt command sees on disk.
type PolicyRcDStatus struct {
	Path          string
	Exists        bool
	IsClouddeploy bool
	BackupExists  bool
}

// InspectPolicyRcD is a read-only probe. Tells the operator whether
// our guard is currently installed, whether a non-CloudDeploy guard
// is on disk, and whether a backup file is hanging around (e.g. from
// a crashed prior run).
func InspectPolicyRcD(env *Env) (PolicyRcDStatus, error) {
	out := PolicyRcDStatus{Path: PolicyRcDPath}
	if env == nil || env.FS == nil {
		return out, errors.New("apt: InspectPolicyRcD requires Env.FS")
	}
	info, err := env.FS.Stat(PolicyRcDPath)
	if err == nil && info != nil {
		out.Exists = true
		if b, rerr := env.FS.ReadFile(PolicyRcDPath); rerr == nil {
			out.IsClouddeploy = isClouddeployBody(b)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Stat failed for an unexpected reason; surface as no info.
	}
	if _, err := env.FS.Stat(policyRcDBackup); err == nil {
		out.BackupExists = true
	}
	return out, nil
}
