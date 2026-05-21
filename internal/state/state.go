// Package state owns /var/lib/clouddeploy/state.json: the canonical
// record of which deploy phases have run on this VM.
//
// State has three roles:
//
//  1. Idempotency. apply / resume / phase consult it to decide which
//     work to skip.
//  2. Diagnostics. doctor reads it to summarise what the system thinks
//     happened.
//  3. Resume across reboots. The continuation systemd unit calls
//     `clouddeployctl resume`, which loads the same state file.
//
// Writes are transactional (temp file + fsync + rename) so a crash
// mid-write cannot leave a corrupt JSON. A lock file
// (/var/lib/clouddeploy/state.lock) prevents two clouddeployctl
// invocations from racing.
//
// The schema version is bumped (Version field) when an incompatible
// change lands so older binaries error out clearly instead of
// silently mis-parsing.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// CurrentVersion is the state-file schema version this binary writes.
const CurrentVersion = 3

// DefaultPath is the canonical location of the state file.
const DefaultPath = "/var/lib/clouddeploy/state.json"

// PhaseStatus is the lifecycle of a single deploy phase.
type PhaseStatus string

const (
	StatusPending                 PhaseStatus = "pending"
	StatusRunning                 PhaseStatus = "running"
	StatusDone                    PhaseStatus = "done"
	StatusSkipped                 PhaseStatus = "skipped"
	StatusFailedFatal             PhaseStatus = "failed_fatal"
	StatusFailedNonfatal          PhaseStatus = "failed_nonfatal"
	StatusPendingMoonlightConnect PhaseStatus = "pending_moonlight_connect"
)

// IsTerminal returns true when no further state transitions are expected
// from this status without operator action.
func (s PhaseStatus) IsTerminal() bool {
	switch s {
	case StatusDone, StatusSkipped, StatusFailedFatal, StatusFailedNonfatal, StatusPendingMoonlightConnect:
		return true
	}
	return false
}

// Phase captures one deploy phase's status and any data it wants to
// remember for doctor / resume. Phase-specific keys live under
// Details (free-form map so we don't have to bump CurrentVersion every
// time a phase adds a field).
type Phase struct {
	Status      PhaseStatus    `json:"status"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

// State is the on-disk schema.
type State struct {
	Version              int               `json:"version"`
	Profile              string            `json:"profile,omitempty"`
	StartedAt            *time.Time        `json:"started_at,omitempty"`
	FinishedAt           *time.Time        `json:"finished_at,omitempty"`
	Phases               map[string]*Phase `json:"phases"`
	RebootNeeded         bool              `json:"reboot_needed,omitempty"`
	ResumeTarget         string            `json:"resume_target,omitempty"`
	RebootCount          int               `json:"reboot_count,omitempty"`
	LastRebootPhase      string            `json:"last_reboot_phase,omitempty"`
	SamePhaseRebootCount int               `json:"same_phase_reboot_count,omitempty"`
	MaxAutoReboots       int               `json:"max_auto_reboots,omitempty"`
	// StartupRecovery is set when loadDeps observed it had to clean
	// up stale on-disk state (a dead-PID lock file, an
	// interrupted-running phase). Diagnostic only; never gates
	// behavior.
	StartupRecovery *StartupRecovery `json:"startup_recovery,omitempty"`
}

// StartupRecovery records the most recent automatic cleanup the
// clouddeployctl CLI performed at startup.
type StartupRecovery struct {
	// StaleLockRecovered is true when AcquireWithRecovery stole a
	// lock file whose PID was dead at startup.
	StaleLockRecovered bool `json:"stale_lock_recovered,omitempty"`
	// StalePID is the dead PID we removed from the lock file. 0
	// when the lock was unreadable rather than dead-but-readable.
	StalePID int `json:"stale_pid,omitempty"`
	// LockPath is the path that was recovered.
	LockPath string `json:"lock_path,omitempty"`
	// RecoveredPhases is the names of phases that were forced from
	// "running" back to "pending" because the prior clouddeployctl
	// process died holding them.
	RecoveredPhases []string `json:"recovered_phases,omitempty"`
	// At is the UTC timestamp of the recovery sweep.
	At *time.Time `json:"at,omitempty"`
}

// InterruptedPhaseRecovery describes a phase that was still marked
// running when a new mutating clouddeployctl invocation acquired the
// process lock. At that point there is no live previous holder, so the
// old running marker is stale and the phase can be retried.
type InterruptedPhaseRecovery struct {
	Name              string
	PreviousStartedAt *time.Time
	Guidance          []string
}

// New returns an empty state for a fresh deploy.
func New(profile string) *State {
	now := time.Now().UTC()
	return &State{
		Version:   CurrentVersion,
		Profile:   profile,
		StartedAt: &now,
		Phases:    map[string]*Phase{},
	}
}

// Load reads state from disk. A non-existent file returns (nil, os.ErrNotExist)
// so the caller can decide whether to bootstrap.
func Load(path string) (*State, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var st State
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("state: parse %s: %w", path, err)
	}
	if st.Version > CurrentVersion {
		return nil, fmt.Errorf("state: %s is version %d, this binary only understands up to %d. Upgrade clouddeployctl or wipe the state file.", path, st.Version, CurrentVersion)
	}
	if st.Phases == nil {
		st.Phases = map[string]*Phase{}
	}
	return &st, nil
}

// Save writes state to disk via a temp-file rename so a crash mid-write
// cannot corrupt the file. The parent directory is created with mode
// 0755 if missing.
func (s *State) Save(path string) error {
	if s == nil {
		return errors.New("state: Save called on nil")
	}
	if s.Version == 0 {
		s.Version = CurrentVersion
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("state: create temp in %s: %w", dir, err)
	}
	// Remove the temp file if we leave through an error path.
	cleanup := func() { _ = os.Remove(tmp.Name()) }

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("state: marshal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("state: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		cleanup()
		return fmt.Errorf("state: chmod 0600 %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return fmt.Errorf("state: rename %s -> %s: %w", tmp.Name(), path, err)
	}
	return nil
}

// WriteIndented prints state to w as pretty JSON. Used by
// `clouddeployctl state show`.
func (s *State) WriteIndented(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// Get returns the phase, creating an empty pending entry if missing.
// Returned pointer is mutable; callers should call Save to persist.
func (s *State) Get(name string) *Phase {
	if p, ok := s.Phases[name]; ok && p != nil {
		return p
	}
	p := &Phase{Status: StatusPending}
	s.Phases[name] = p
	return p
}

// Set replaces a phase wholesale. Used when a phase wants to overwrite
// any previous Details.
func (s *State) Set(name string, p *Phase) {
	if p == nil {
		delete(s.Phases, name)
		return
	}
	s.Phases[name] = p
}

// MarkRunning transitions a phase to running with a fresh StartedAt.
// Existing details are cleared so an aborted previous run can't leak
// stale "Driver version X" data into the new attempt.
func (s *State) MarkRunning(name string) *Phase {
	now := time.Now().UTC()
	p := &Phase{
		Status:    StatusRunning,
		StartedAt: &now,
	}
	s.Phases[name] = p
	return p
}

// RecoverInterruptedRunningPhases rewrites stale "running" phase
// markers to "pending" once a fresh mutating invocation has acquired
// the process lock. This makes interrupted phases visibly retriable in
// state.json instead of leaving a misleading permanent "running"
// status after systemd killed a continuation service.
func (s *State) RecoverInterruptedRunningPhases(profileName string) []InterruptedPhaseRecovery {
	if s == nil || len(s.Phases) == 0 {
		return nil
	}
	if profileName == "" {
		profileName = s.Profile
	}
	if profileName == "" {
		profileName = "<profile>"
	}

	now := time.Now().UTC()
	var recovered []InterruptedPhaseRecovery
	for name, p := range s.Phases {
		if p == nil || p.Status != StatusRunning {
			continue
		}

		guidance := []string{
			fmt.Sprintf("clouddeployctl state reset --phase %s", name),
			fmt.Sprintf("clouddeployctl resume --profile %s", profileName),
		}
		details := map[string]any{
			"interrupted_detected_at": now.Format(time.RFC3339),
			"interrupted_recovery":    "phase was running in state, but this invocation acquired the CloudDeploy process lock; retry is allowed",
			"recovery_guidance":       guidance,
		}
		if p.StartedAt != nil {
			details["previous_started_at"] = p.StartedAt.Format(time.RFC3339)
		}

		recovered = append(recovered, InterruptedPhaseRecovery{
			Name:              name,
			PreviousStartedAt: p.StartedAt,
			Guidance:          guidance,
		})

		p.Status = StatusPending
		p.StartedAt = nil
		p.CompletedAt = nil
		p.Reason = "previous invocation ended while this phase was running; retrying"
		p.LastError = "No active CloudDeploy process lock was found for the previous run."
		p.Details = details
	}
	return recovered
}

// MarkDone marks a phase complete with the supplied details map. A nil
// map is allowed.
func (s *State) MarkDone(name string, details map[string]any) *Phase {
	now := time.Now().UTC()
	p := s.Get(name)
	p.Status = StatusDone
	p.CompletedAt = &now
	p.Reason = ""
	p.LastError = ""
	p.Details = details
	return p
}

// MarkSkipped marks a phase intentionally not run. The reason is
// surfaced in `doctor`.
func (s *State) MarkSkipped(name, reason string) *Phase {
	now := time.Now().UTC()
	p := s.Get(name)
	p.Status = StatusSkipped
	p.CompletedAt = &now
	p.Reason = reason
	return p
}

// MarkFailed marks a phase failed. fatal=true is the deploy-killing
// case; fatal=false lets the next phase run. The error message is
// surfaced in LastError so `state show` and `doctor` can print it.
func (s *State) MarkFailed(name, reason string, err error, fatal bool) *Phase {
	now := time.Now().UTC()
	p := s.Get(name)
	if fatal {
		p.Status = StatusFailedFatal
	} else {
		p.Status = StatusFailedNonfatal
	}
	p.CompletedAt = &now
	p.Reason = reason
	if err != nil {
		p.LastError = err.Error()
	}
	return p
}

// Reset clears a single phase's status back to pending. Used by
// `clouddeployctl state reset --phase <name>` to opt the operator
// into a re-run after a manual intervention.
func (s *State) Reset(name string) bool {
	if _, ok := s.Phases[name]; !ok {
		return false
	}
	delete(s.Phases, name)
	return true
}

// SetRebootNeeded toggles the reboot marker.
func (s *State) SetRebootNeeded(needed bool, target string) {
	s.RebootNeeded = needed
	s.ResumeTarget = target
}

// RecordRebootRequest increments reboot-loop counters for unattended
// deploys. It is called immediately before installing the continuation
// service so the state survives the reboot.
func (s *State) RecordRebootRequest(target string, maxAutoReboots int) {
	if s == nil {
		return
	}
	if maxAutoReboots > 0 {
		s.MaxAutoReboots = maxAutoReboots
	}
	s.RebootCount++
	if target != "" && target == s.LastRebootPhase {
		s.SamePhaseRebootCount++
	} else {
		s.LastRebootPhase = target
		s.SamePhaseRebootCount = 1
	}
}

// ClearRebootNeeded is the post-reboot side: resume calls this after
// it actually starts running again.
func (s *State) ClearRebootNeeded() {
	s.RebootNeeded = false
	s.ResumeTarget = ""
}

// MarkPendingMoonlightConnect is the "wait for client" sentinel. The
// HDR stream validator transitions to done only once a Moonlight
// client connects and the success markers land in the journal.
func (s *State) MarkPendingMoonlightConnect(name, reason string) *Phase {
	p := s.Get(name)
	p.Status = StatusPendingMoonlightConnect
	p.Reason = reason
	return p
}
