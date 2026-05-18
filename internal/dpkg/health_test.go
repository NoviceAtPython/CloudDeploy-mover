package dpkg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditResult_HasCorruptUpdatesJournal(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"live VM signature", `dpkg: error: parsing file '/var/lib/dpkg/updates/0000' near line 0:
end of file after field name ''`, true},
		{"parsing only, different path", "dpkg: error: parsing file '/etc/dpkg/policy-rc.d' line 1", false},
		{"path mention only, no parsing word", "info: /var/lib/dpkg/updates/ is empty", false},
		{"clean output", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := AuditResult{Stderr: c.stderr}
			if got := a.HasCorruptUpdatesJournal(); got != c.want {
				t.Errorf("HasCorruptUpdatesJournal: got %v want %v (stderr=%q)", got, c.want, c.stderr)
			}
		})
	}
}

func TestJournalFiles_IgnoresNonNumeric(t *testing.T) {
	dir := t.TempDir()
	seeds := []string{"0000", "0001", "0042", "readme.txt", "backup-dir"}
	for _, s := range seeds {
		full := filepath.Join(dir, s)
		if s == "backup-dir" {
			if err := os.Mkdir(full, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", s, err)
		}
	}
	files, err := JournalFiles(dir)
	if err != nil {
		t.Fatalf("JournalFiles: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("JournalFiles: got %v want 3 numeric entries", files)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if base == "readme.txt" || base == "backup-dir" {
			t.Errorf("JournalFiles: unexpected entry %q", base)
		}
	}
}

func TestHealthCheck_HealthyAuditPassesThrough(t *testing.T) {
	plan, ok, err := HealthCheck(AuditResult{}, t.TempDir(), t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !ok {
		t.Errorf("healthy audit should report ok=true")
	}
	if len(plan.JournalFiles) != 0 {
		t.Errorf("healthy audit should yield no quarantine plan; got %v", plan)
	}
}

func TestHealthCheck_CorruptJournal_PlansQuarantine(t *testing.T) {
	updates := t.TempDir()
	for _, s := range []string{"0000", "0001"} {
		if err := os.WriteFile(filepath.Join(updates, s), []byte("garbage"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	backupRoot := t.TempDir()
	audit := AuditResult{
		ExitErr: errors.New("dpkg --audit exit=1"),
		Stderr:  "dpkg: error: parsing file '/var/lib/dpkg/updates/0000' near line 0:\nend of file after field name ''",
	}
	now := time.Date(2026, 5, 18, 12, 34, 56, 0, time.UTC)

	plan, ok, err := HealthCheck(audit, updates, backupRoot, now)
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if ok {
		t.Errorf("corrupt journal must NOT be reported healthy")
	}
	if len(plan.JournalFiles) != 2 {
		t.Errorf("JournalFiles: got %v want 2 entries", plan.JournalFiles)
	}
	wantSuffix := "dpkg-updates-20260518-123456"
	if !strings.HasSuffix(plan.BackupDir, wantSuffix) {
		t.Errorf("BackupDir: got %q want suffix %q", plan.BackupDir, wantSuffix)
	}
	if !strings.Contains(plan.Reason, "quarantining") {
		t.Errorf("Reason should explain the action; got %q", plan.Reason)
	}
}

func TestHealthCheck_AuditFailsButNoJournalFiles(t *testing.T) {
	// Audit reported half-configured packages but no journal
	// corruption => no quarantine plan, but healthy=false so the
	// caller knows to run dpkg --configure -a.
	plan, ok, err := HealthCheck(AuditResult{
		Lines: []string{"The following packages are in a mess..."},
	}, t.TempDir(), t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if ok {
		t.Errorf("half-configured packages should yield healthy=false")
	}
	if len(plan.JournalFiles) != 0 {
		t.Errorf("no journal files => no quarantine plan; got %v", plan.JournalFiles)
	}
}

func TestExecuteQuarantine_MovesFiles(t *testing.T) {
	updates := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "dpkg-updates-20260518-123456")
	srcs := []string{"0000", "0001", "0042"}
	for _, s := range srcs {
		if err := os.WriteFile(filepath.Join(updates, s), []byte(s), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	plan := QuarantinePlan{
		JournalFiles: []string{
			filepath.Join(updates, "0000"),
			filepath.Join(updates, "0001"),
			filepath.Join(updates, "0042"),
		},
		BackupDir: backupDir,
		Reason:    "test",
	}
	if err := ExecuteQuarantine(plan); err != nil {
		t.Fatalf("ExecuteQuarantine: %v", err)
	}
	// Originals gone.
	for _, s := range srcs {
		if _, err := os.Stat(filepath.Join(updates, s)); !os.IsNotExist(err) {
			t.Errorf("source %s should be gone; stat err=%v", s, err)
		}
	}
	// Backups present.
	for _, s := range srcs {
		b, err := os.ReadFile(filepath.Join(backupDir, s))
		if err != nil {
			t.Errorf("backup %s missing: %v", s, err)
			continue
		}
		if string(b) != s {
			t.Errorf("backup %s contents: got %q want %q", s, string(b), s)
		}
	}
}

func TestExecuteQuarantine_EmptyPlanNoop(t *testing.T) {
	if err := ExecuteQuarantine(QuarantinePlan{}); err != nil {
		t.Errorf("empty plan should be a no-op; got %v", err)
	}
}
