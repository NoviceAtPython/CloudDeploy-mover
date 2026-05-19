package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := New("hdr-4k120")
	st.MarkDone("base_packages", map[string]any{
		"installed": []string{"git", "cmake"},
	})
	st.MarkSkipped("cuda", "not required for KMS/NVENC/HDR path")
	st.MarkRunning("nvidia_driver")
	st.MarkDone("nvidia_driver", map[string]any{
		"package_family": "server-open",
		"driver_version": "580.126.20",
	})

	if err := st.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Profile != "hdr-4k120" {
		t.Errorf("profile mismatch: got %q", loaded.Profile)
	}
	if loaded.Get("base_packages").Status != StatusDone {
		t.Errorf("base_packages status: got %q want done", loaded.Get("base_packages").Status)
	}
	if loaded.Get("cuda").Status != StatusSkipped {
		t.Errorf("cuda status: got %q want skipped", loaded.Get("cuda").Status)
	}
	if loaded.Get("cuda").Reason != "not required for KMS/NVENC/HDR path" {
		t.Errorf("cuda reason: got %q", loaded.Get("cuda").Reason)
	}
	if loaded.Get("nvidia_driver").Status != StatusDone {
		t.Errorf("nvidia_driver status: got %q want done", loaded.Get("nvidia_driver").Status)
	}
	if loaded.Get("nvidia_driver").Details["package_family"] != "server-open" {
		t.Errorf("nvidia_driver details lost: got %#v", loaded.Get("nvidia_driver").Details)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// Verify Save uses temp + rename, not in-place write. Inspecting
	// the directory between Save calls should show no half-written
	// state file.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := New("hdr-4k120")
	st.MarkDone("base_packages", nil)
	if err := st.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Now mutate and save again; confirm the previous content is fully
	// replaced and no stray temp file lingers.
	st.MarkDone("nvidia_driver", map[string]any{"driver_version": "580.126.20"})
	if err := st.Save(path); err != nil {
		t.Fatalf("save (2): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("stray file after atomic save: %s", e.Name())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Get("nvidia_driver").Details["driver_version"] != "580.126.20" {
		t.Errorf("second save did not take: %#v", got.Get("nvidia_driver"))
	}
}

func TestLoadVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Pretend a future version wrote this file.
	if err := os.WriteFile(path, []byte(`{"version":99,"phases":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("expected version-mismatch error, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatalf("expected error loading missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got %v", err)
	}
}

func TestPhaseStatusTerminal(t *testing.T) {
	cases := map[PhaseStatus]bool{
		StatusPending:                 false,
		StatusRunning:                 false,
		StatusDone:                    true,
		StatusSkipped:                 true,
		StatusFailedFatal:             true,
		StatusFailedNonfatal:          true,
		StatusPendingMoonlightConnect: true,
	}
	for status, want := range cases {
		if got := status.IsTerminal(); got != want {
			t.Errorf("status %q IsTerminal: got %v want %v", status, got, want)
		}
	}
}

// TestReset_ClearsPhase verifies that Reset wipes a phase entirely so
// the next apply treats it as never-attempted.
func TestReset_ClearsPhase(t *testing.T) {
	st := New("hdr-4k120")
	st.MarkDone("nvidia_driver", map[string]any{"driver_version": "580.126.20"})
	if st.Get("nvidia_driver").Status != StatusDone {
		t.Fatalf("setup failed: not Done")
	}

	if !st.Reset("nvidia_driver") {
		t.Errorf("Reset should return true for an existing phase")
	}
	// After Reset, Get must return a fresh pending phase, not the
	// stale "done" entry.
	if got := st.Get("nvidia_driver"); got.Status != StatusPending {
		t.Errorf("after Reset, status should be Pending, got %q", got.Status)
	}

	// Resetting an unknown phase is a no-op signalled by false.
	if st.Reset("never_seen") {
		t.Errorf("Reset should return false for unknown phase")
	}
}

func TestRecoverInterruptedRunningPhases(t *testing.T) {
	st := New("hdr-4k120")
	st.MarkRunning("cuda")
	st.MarkDone("base_packages", nil)

	recovered := st.RecoverInterruptedRunningPhases("hdr-4k120")
	if len(recovered) != 1 {
		t.Fatalf("recovered len: got %d want 1 (%#v)", len(recovered), recovered)
	}
	if recovered[0].Name != "cuda" {
		t.Fatalf("recovered phase: got %q want cuda", recovered[0].Name)
	}
	if st.Get("cuda").Status != StatusPending {
		t.Fatalf("cuda status: got %q want pending", st.Get("cuda").Status)
	}
	if st.Get("cuda").StartedAt != nil {
		t.Fatalf("cuda StartedAt should be cleared after recovery")
	}
	if st.Get("base_packages").Status != StatusDone {
		t.Fatalf("done phase should not change; got %q", st.Get("base_packages").Status)
	}
	if st.Get("cuda").LastError == "" {
		t.Fatalf("expected LastError to explain stale running recovery")
	}
	if st.Get("cuda").Details["recovery_guidance"] == nil {
		t.Fatalf("expected recovery guidance details: %#v", st.Get("cuda").Details)
	}
}

// TestRebootNeededRoundTrip verifies SetRebootNeeded persists across
// save/load and ClearRebootNeeded zeroes it.
func TestRebootNeededRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := New("hdr-4k120")
	st.SetRebootNeeded(true, "nvidia_driver")
	st.MarkRunning("nvidia_driver")
	if err := st.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.RebootNeeded {
		t.Errorf("RebootNeeded should persist as true")
	}
	if loaded.ResumeTarget != "nvidia_driver" {
		t.Errorf("ResumeTarget mismatch: got %q", loaded.ResumeTarget)
	}

	loaded.ClearRebootNeeded()
	if loaded.RebootNeeded || loaded.ResumeTarget != "" {
		t.Errorf("ClearRebootNeeded did not zero fields: %+v", loaded)
	}
}

// TestMarkFailed_RecordsErrorAndFatality ensures the new error +
// fatal/nonfatal contract is honoured.
func TestMarkFailed_RecordsErrorAndFatality(t *testing.T) {
	st := New("hdr-4k120")
	st.MarkFailed("nvidia_driver", "apt-get install failed", fmtErr("exit 100"), true)
	p := st.Get("nvidia_driver")
	if p.Status != StatusFailedFatal {
		t.Errorf("Status: got %q want failed_fatal", p.Status)
	}
	if p.LastError != "exit 100" {
		t.Errorf("LastError: got %q", p.LastError)
	}
	if p.Reason != "apt-get install failed" {
		t.Errorf("Reason: got %q", p.Reason)
	}

	st.MarkFailed("cuda", "runfile checksum failed", fmtErr("checksum"), false)
	if st.Get("cuda").Status != StatusFailedNonfatal {
		t.Errorf("non-fatal status mismatch")
	}
}

// fmtErr is a tiny helper that returns a sentinel error with a given
// message. Avoids importing errors and pkg/errors just for tests.
func fmtErr(msg string) error { return &simpleErr{msg: msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// TestLockAcquireRelease covers the happy path: take the lock,
// release it, take it again from the same process.
func TestLockAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if l1.Path() != path {
		t.Errorf("Path: got %q want %q", l1.Path(), path)
	}
	if l1.PID() != os.Getpid() {
		t.Errorf("PID: got %d want %d", l1.PID(), os.Getpid())
	}
	if err := l1.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock file should be gone after Release; stat err=%v", err)
	}
	// Second Release is a no-op.
	if err := l1.Release(); err != nil {
		t.Errorf("double Release should be a no-op: %v", err)
	}
}

// TestLockHeldByOtherProcess: a lock file with someone else's live
// PID returns ErrLocked.
func TestLockHeldByOtherProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l1.Release()

	_, err = Acquire(path)
	if err == nil {
		t.Fatalf("expected ErrLocked when same-process tries twice")
	}
	// Note: we treat any os.Getpid() collision as "alive" because
	// FindProcess(my-pid).Signal(0) returns nil. That's the correct
	// behaviour for the production case (another clouddeployctl
	// process), even if the unit-test contortion is awkward.
}

// TestLockStaleEntryIsStolen: a lock file pointing at a dead PID
// gets force-removed and re-acquired.
func TestLockStaleEntryIsStolen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")

	// Write a lock file pointing at PID 1 isn't safe (PID 1 is init).
	// Use a guaranteed-dead PID: a very high value unlikely to exist.
	// On Linux PID_MAX is typically 2^22; 9999998 is high enough.
	stalePID := 9999998
	if err := os.WriteFile(path, []byte("9999998\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire should succeed by stealing a stale entry; got: %v (stalePID was %d)", err, stalePID)
	}
	if l.PID() != os.Getpid() {
		t.Errorf("after steal, lock should be ours; PID got %d want %d", l.PID(), os.Getpid())
	}
	_ = l.Release()
}

// TestInspect_NoLock returns Exists=false; HolderLive=false.
func TestInspect_NoLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")
	info := Inspect(path)
	if info.Exists {
		t.Errorf("Exists: got true, want false")
	}
	if info.PID != 0 {
		t.Errorf("PID: got %d want 0", info.PID)
	}
	if info.HolderLive {
		t.Errorf("HolderLive: got true, want false")
	}
}

// TestInspect_LiveHolder: lock taken by this process is reported as
// alive.
func TestInspect_LiveHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	info := Inspect(path)
	if !info.Exists {
		t.Fatalf("Exists: got false, want true")
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID: got %d want %d", info.PID, os.Getpid())
	}
	if !info.HolderLive {
		t.Errorf("HolderLive: got false, want true (self is alive)")
	}
}

// TestInspect_StaleHolder: a lock file with a dead PID is reported
// as HolderLive=false; the operator can rm and retry.
func TestInspect_StaleHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.lock")
	if err := os.WriteFile(path, []byte("9999998\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info := Inspect(path)
	if !info.Exists {
		t.Fatalf("Exists: got false, want true")
	}
	if info.PID != 9999998 {
		t.Errorf("PID: got %d want 9999998", info.PID)
	}
	if info.HolderLive {
		t.Errorf("HolderLive: got true, want false (PID 9999998 should be dead)")
	}
}
