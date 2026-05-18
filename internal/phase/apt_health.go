package phase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/dpkg"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/sudo"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// AptHealthName is the state-key for the new pre-upgrade phase.
const AptHealthName = "apt_health"

// AptHealth runs BEFORE ubuntu-upgrade. It guards two failure modes
// observed on the live VM:
//
//  1. /etc/sudoers being clobbered mid-upgrade (snapd postinst /
//     half-finished sudo reinstall) and the operator losing SSH-side
//     sudo access. AptHealth writes /etc/sudoers.d/90-clouddeploy-<user>
//     FIRST so the recovery account stays privileged regardless of
//     what happens next.
//
//  2. dpkg database corruption (e.g. /var/lib/dpkg/updates/0000 parse
//     error). AptHealth detects this via `dpkg --audit`, quarantines
//     the journal files into /var/lib/clouddeploy/backups/dpkg-
//     updates-<ts>/, then runs `dpkg --configure -a` + `apt-get -f
//     install -y` so the rest of apply can proceed.
//
//  3. Mid-upgrade detection: when the host VERSION_ID disagrees with
//     the codename in /etc/apt/sources.list.d/ubuntu.sources (e.g.
//     host=24.04 but sources=questing), the phase logs an explicit
//     "mid-upgrade state" warning and records it in state.Details so
//     the operator + downstream phases can react.
//
// All side effects gate on DryRun.
type AptHealth struct {
	// SudoUserFn lets tests inject the operator account name. nil =
	// read SUDO_USER from env (the user who ran `sudo clouddeployctl
	// apply`).
	SudoUserFn func() string

	// AuditFn lets tests inject the `dpkg --audit` result. nil =
	// real dpkg via the runner.
	AuditFn func(ctx context.Context, deps *Deps) dpkg.AuditResult

	// UpdatesDir overrides /var/lib/dpkg/updates for tests.
	UpdatesDir string

	// BackupRoot overrides /var/lib/clouddeploy/backups for tests.
	BackupRoot string

	// SudoersDir overrides /etc/sudoers.d for tests.
	SudoersDir string

	// VisudoCheckFn lets tests inject the visudo validator. nil =
	// `visudo -cf <path>` via the runner.
	VisudoCheckFn func(path string) error

	// CurrentVersionFn lets tests inject the host VERSION_ID for the
	// mid-upgrade detector. nil = read /etc/os-release.
	CurrentVersionFn func() string

	// SourcesPath overrides /etc/apt/sources.list.d/ubuntu.sources
	// for tests. The mid-upgrade detector reads this file and
	// looks for codename mentions.
	SourcesPath string

	// NowFn lets tests pin the backup-dir timestamp. nil =
	// time.Now().
	NowFn func() time.Time
}

// Name implements Phase.
func (AptHealth) Name() string { return AptHealthName }

// Run implements Phase.
func (p AptHealth) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, AptHealthName) {
		log.Info("phase apt-health: already done; skipping")
		return nil
	}

	deps.State.MarkRunning(AptHealthName)
	_ = deps.PersistState()

	details := map[string]any{}
	now := time.Now()
	if p.NowFn != nil {
		now = p.NowFn()
	}

	// Step 1: preserve sudo for the operator.
	sudoUser := p.sudoUser()
	if sudoUser == "" {
		details["sudo_preserve"] = "skipped (no SUDO_USER detected)"
		log.Info("phase apt-health: no SUDO_USER detected; skipping sudo preserve")
	} else if !sudo.ValidUser(sudoUser) {
		details["sudo_preserve"] = fmt.Sprintf("skipped (unsafe user %q)", sudoUser)
		log.Warn("phase apt-health: refusing to install sudoers entry for unsafe user", "user", sudoUser)
	} else {
		visudo := p.VisudoCheckFn
		if visudo == nil {
			visudo = p.runVisudoCheck(ctx, deps)
		}
		res, err := sudo.Preserve(sudo.PreserveOptions{
			User:        sudoUser,
			SudoersDir:  p.SudoersDir,
			VisudoCheck: visudo,
			DryRun:      deps.DryRun,
		})
		if err != nil {
			deps.State.MarkFailed(AptHealthName, "preserve sudoers entry", err, true)
			details["sudo_preserve_err"] = err.Error()
			deps.State.Get(AptHealthName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase apt-health: %w", err)
		}
		log.Info("phase apt-health: sudo preserve",
			"user", res.User,
			"path", res.Path,
			"wrote", res.Wrote,
			"already_correct", res.AlreadyCorrect)
		details["sudo_preserve_user"] = res.User
		details["sudo_preserve_path"] = res.Path
		details["sudo_preserve_wrote"] = res.Wrote
		details["sudo_preserve_already_correct"] = res.AlreadyCorrect
	}

	// Step 2: dpkg health probe + repair.
	auditFn := p.AuditFn
	if auditFn == nil {
		auditFn = p.runDpkgAudit
	}
	audit := auditFn(ctx, deps)
	details["dpkg_audit_lines"] = len(audit.Lines)
	if audit.ExitErr != nil {
		details["dpkg_audit_err"] = audit.ExitErr.Error()
	}
	if audit.HasCorruptUpdatesJournal() {
		details["dpkg_journal_corrupt"] = true
	}
	plan, healthy, err := dpkg.HealthCheck(audit, p.UpdatesDir, p.BackupRoot, now)
	if err != nil {
		deps.State.MarkFailed(AptHealthName, "dpkg HealthCheck", err, true)
		details["dpkg_health_err"] = err.Error()
		deps.State.Get(AptHealthName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase apt-health: %w", err)
	}
	if !healthy {
		log.Warn("phase apt-health: dpkg unhealthy; repair required",
			"audit_lines", len(audit.Lines),
			"audit_err", audit.ExitErr,
			"plan_files", plan.JournalFiles,
			"backup_dir", plan.BackupDir,
			"reason", plan.Reason)
		details["dpkg_repair_attempted"] = true
		details["dpkg_quarantine_files"] = plan.JournalFiles
		details["dpkg_quarantine_backup"] = plan.BackupDir
		details["dpkg_quarantine_reason"] = plan.Reason

		if !deps.DryRun && len(plan.JournalFiles) > 0 {
			if err := dpkg.ExecuteQuarantine(plan); err != nil {
				deps.State.MarkFailed(AptHealthName, "dpkg journal quarantine", err, true)
				details["dpkg_quarantine_err"] = err.Error()
				deps.State.Get(AptHealthName).Details = details
				_ = deps.PersistState()
				return fmt.Errorf("phase apt-health: %w", err)
			}
		}
		if !deps.DryRun {
			if err := p.runDpkgConfigure(ctx, deps); err != nil {
				deps.State.MarkFailed(AptHealthName, "dpkg --configure -a after quarantine", err, true)
				details["dpkg_configure_err"] = err.Error()
				deps.State.Get(AptHealthName).Details = details
				_ = deps.PersistState()
				return fmt.Errorf("phase apt-health: %w", err)
			}
			if err := p.runAptFixBroken(ctx, deps); err != nil {
				deps.State.MarkFailed(AptHealthName, "apt-get -f install after dpkg repair", err, true)
				details["apt_fix_broken_err"] = err.Error()
				deps.State.Get(AptHealthName).Details = details
				_ = deps.PersistState()
				return fmt.Errorf("phase apt-health: %w", err)
			}
		}
		details["dpkg_repair_ok"] = true
		log.Info("phase apt-health: dpkg repair complete")
	} else {
		details["dpkg_audit_ok"] = true
	}

	// Step 3: mid-upgrade detection.
	mid, why := p.detectMidUpgrade()
	details["mid_upgrade"] = mid
	if mid {
		details["mid_upgrade_reason"] = why
		log.Warn("phase apt-health: mid-upgrade state detected", "reason", why)
	}

	deps.State.MarkDone(AptHealthName, details)
	_ = deps.PersistState()
	log.Info("phase apt-health: done",
		"sudo_user", sudoUser,
		"dpkg_repair_attempted", details["dpkg_repair_attempted"] == true,
		"mid_upgrade", mid)
	return nil
}

func (p AptHealth) sudoUser() string {
	if p.SudoUserFn != nil {
		return p.SudoUserFn()
	}
	return os.Getenv("SUDO_USER")
}

func (p AptHealth) runDpkgAudit(ctx context.Context, deps *Deps) dpkg.AuditResult {
	if deps.DryRun {
		return dpkg.AuditResult{}
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"dpkg", "--audit"},
		LogFile: "-",
		Timeout: 30 * time.Second,
	})
	a := dpkg.AuditResult{Stderr: res.Stderr}
	if res.Err != nil {
		a.ExitErr = res.Err
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		a.Lines = append(a.Lines, line)
	}
	return a
}

func (p AptHealth) runDpkgConfigure(ctx context.Context, deps *Deps) error {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{"dpkg", "--configure", "-a"},
		Env: []string{
			"DEBIAN_FRONTEND=noninteractive",
			"NEEDRESTART_MODE=a",
			"NEEDRESTART_SUSPEND=1",
		},
		Sudo:    true,
		Timeout: 15 * time.Minute,
	})
	if res.Err != nil {
		return fmt.Errorf("dpkg --configure -a: %w (stderr=%q)", res.Err, lastLines(res.Stderr, 5))
	}
	return nil
}

func (p AptHealth) runAptFixBroken(ctx context.Context, deps *Deps) error {
	// Best-effort: route through apt.Transaction so the policy-rc.d
	// guard covers the repair.
	return deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.FixBroken(ctx)
	})
}

// runVisudoCheck returns a sudo.VisudoCheck adapter that shells out
// to `visudo -cf <path>`. Returns a no-op checker when sudo+visudo
// aren't available (developer hosts / tests).
func (p AptHealth) runVisudoCheck(ctx context.Context, deps *Deps) func(string) error {
	return func(path string) error {
		if deps.DryRun {
			return nil
		}
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"visudo", "-cf", path},
			LogFile: "-",
			Timeout: 10 * time.Second,
		})
		if res.Err != nil {
			return fmt.Errorf("visudo -cf %s: %w (stderr=%q)", path, res.Err, lastLines(res.Stderr, 3))
		}
		return nil
	}
}

// detectMidUpgrade reports true when the host's VERSION_ID disagrees
// with the codename mentioned in ubuntu.sources / sources.list. That
// is the canonical "we already rewrote the sources but the upgrade
// didn't complete" state from the live VM postmortem.
func (p AptHealth) detectMidUpgrade() (bool, string) {
	currentVer := ""
	if p.CurrentVersionFn != nil {
		currentVer = p.CurrentVersionFn()
	} else {
		r, err := ubuntu.Read("")
		if err == nil {
			currentVer = r.VersionID
		}
	}
	if currentVer == "" {
		return false, ""
	}
	hostCodename := ubuntu.CodenameForVersion(currentVer)
	if hostCodename == "" {
		return false, ""
	}

	sourcesPath := p.SourcesPath
	if sourcesPath == "" {
		sourcesPath = "/etc/apt/sources.list.d/ubuntu.sources"
	}
	b, err := os.ReadFile(sourcesPath)
	if err != nil {
		return false, ""
	}
	body := string(b)
	// Walk every known codename and see which ones appear. A
	// "mismatch" means we find a codename != hostCodename mentioned
	// in the sources file.
	for _, codename := range KnownCodenames() {
		if codename == hostCodename {
			continue
		}
		if strings.Contains(body, codename) {
			return true, fmt.Sprintf("host VERSION_ID=%s (codename=%s) but %s mentions codename %q (probable interrupted release upgrade)", currentVer, hostCodename, filepath.Base(sourcesPath), codename)
		}
	}
	return false, ""
}

// KnownCodenames is the set of Ubuntu codenames the mid-upgrade
// detector looks for. Mirrors internal/ubuntu.codenameByVersion's
// values (we keep a local list so the apt-health phase doesn't grow
// a dependency on an exported map-walker).
func KnownCodenames() []string {
	return []string{
		"jammy", "kinetic", "lunar", "mantic",
		"noble", "oracular", "plucky", "questing",
		"resolute",
	}
}

func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// _ keeps errors imported even if unused after edits.
var _ = errors.New
