// Package dpkg owns the dpkg-database health probe + repair logic.
//
// Motivating failure (live VM, 2026-05):
//
//	dpkg: error: parsing file '/var/lib/dpkg/updates/0000' near line 0:
//	end of file after field name ''
//
// dpkg refuses to make any progress in this state, including
// `dpkg --configure -a`, and any subsequent apt-get install attempt
// then half-configures packages and can clobber pieces of the
// installed system (sudo, snapd) leaving the operator locked out
// from SSH-side recovery. The repair recipe v2 uses (and which we
// codify here) is:
//
//  1. Walk /var/lib/dpkg/updates/. Numeric files there are dpkg's
//     half-written transaction journal entries.
//  2. If `dpkg --audit` fails AND those journal files exist, MOVE
//     them out of the way (we keep a backup, never delete) so dpkg
//     stops parsing them.
//  3. Run `dpkg --configure -a` to complete any half-configured
//     packages.
//  4. Run `apt-get -f install -y` to clear any half-installed
//     dependencies.
//
// The package is structured to be testable cross-platform: every
// step accepts a fake-able input (filesystem walk + audit result),
// the side-effecting calls (mv + dpkg + apt) sit behind small Step
// types the phase wires to a real runner.
package dpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultUpdatesDir is the dpkg transaction-journal directory.
const DefaultUpdatesDir = "/var/lib/dpkg/updates"

// DefaultBackupRoot is where CloudDeploy stashes quarantined files.
const DefaultBackupRoot = "/var/lib/clouddeploy/backups"

// AuditResult describes what `dpkg --audit` reported.
type AuditResult struct {
	// ExitErr is non-nil when dpkg itself errored (binary failed to
	// run, corrupt journal blocked the audit, etc.). When non-nil
	// the caller should consider the dpkg DB unhealthy regardless
	// of Lines.
	ExitErr error
	// Stderr is the captured stderr from `dpkg --audit`. Inspected
	// for the "/var/lib/dpkg/updates/" corruption signature.
	Stderr string
	// Lines is the captured stdout split into rows; dpkg --audit
	// reports half-configured packages here.
	Lines []string
}

// HasCorruptUpdatesJournal returns true when the audit stderr names a
// file under /var/lib/dpkg/updates/ as a parse failure. That is the
// canonical "quarantine the journal" signal.
func (a AuditResult) HasCorruptUpdatesJournal() bool {
	return strings.Contains(a.Stderr, "/var/lib/dpkg/updates/") &&
		strings.Contains(a.Stderr, "parsing")
}

// JournalFiles lists numeric files under `dir`. dpkg names them
// 0000, 0001, ... ; non-numeric entries (e.g. a backup directory we
// may have placed nearby) are ignored.
func JournalFiles(dir string) ([]string, error) {
	if dir == "" {
		dir = DefaultUpdatesDir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isNumericFilename(name) {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

func isNumericFilename(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// QuarantinePlan describes what HealthCheck wants to do BEFORE
// touching the filesystem. The phase logs the plan, then asks the
// caller to execute it (the phase decides whether DryRun applies).
type QuarantinePlan struct {
	// JournalFiles is the absolute-path list of journal files that
	// should be moved aside.
	JournalFiles []string
	// BackupDir is the destination where files are moved. The phase
	// MkdirAll's this path before the move.
	BackupDir string
	// Reason explains why the quarantine fires (audit signal, etc.).
	Reason string
}

// HealthCheck inspects the audit result + journal directory and
// returns a QuarantinePlan when one is needed, plus an OK flag when
// dpkg looks healthy and no repair work is required.
//
// inputs:
//   - audit:        result of `dpkg --audit`
//   - updatesDir:   path to /var/lib/dpkg/updates (override for tests)
//   - backupRoot:   where to stash quarantined files
//   - now:          time-source for the backup-dir timestamp
func HealthCheck(audit AuditResult, updatesDir, backupRoot string, now time.Time) (plan QuarantinePlan, healthy bool, err error) {
	if updatesDir == "" {
		updatesDir = DefaultUpdatesDir
	}
	if backupRoot == "" {
		backupRoot = DefaultBackupRoot
	}

	// Healthy: no audit error AND audit reported no half-configured
	// rows.
	if audit.ExitErr == nil && len(audit.Lines) == 0 {
		return QuarantinePlan{}, true, nil
	}

	// Unhealthy. Decide if a journal quarantine would help.
	if !audit.HasCorruptUpdatesJournal() && audit.ExitErr == nil {
		// dpkg ran cleanly but reported half-configured packages; no
		// journal corruption. Caller still needs to run --configure -a
		// + apt-get -f install, but no files to move.
		return QuarantinePlan{}, false, nil
	}

	files, ferr := JournalFiles(updatesDir)
	if ferr != nil {
		// Updates dir is unreadable. Surface the error so the phase
		// fails before source rewrite rather than silently skipping.
		return QuarantinePlan{}, false, fmt.Errorf("dpkg: read %s: %w", updatesDir, ferr)
	}
	if len(files) == 0 {
		// Audit error but no journal files to move. The phase still
		// needs to retry `dpkg --configure -a`.
		return QuarantinePlan{}, false, nil
	}

	stamp := now.UTC().Format("20060102-150405")
	plan = QuarantinePlan{
		JournalFiles: files,
		BackupDir:    filepath.Join(backupRoot, "dpkg-updates-"+stamp),
		Reason:       "dpkg audit reports parse error under /var/lib/dpkg/updates/; quarantining journal",
	}
	return plan, false, nil
}

// ExecuteQuarantine performs the file moves the plan describes. The
// phase calls this AFTER logging the plan (and only when not DryRun).
func ExecuteQuarantine(plan QuarantinePlan) error {
	if len(plan.JournalFiles) == 0 {
		return nil
	}
	if err := os.MkdirAll(plan.BackupDir, 0o700); err != nil {
		return fmt.Errorf("dpkg: mkdir backup %s: %w", plan.BackupDir, err)
	}
	for _, src := range plan.JournalFiles {
		dst := filepath.Join(plan.BackupDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("dpkg: quarantine %s -> %s: %w", src, dst, err)
		}
	}
	return nil
}
