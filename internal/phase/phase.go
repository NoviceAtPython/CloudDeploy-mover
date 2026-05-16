// Package phase contains the deploy phases. Each phase implements
// the Phase interface and is invoked by `clouddeployctl apply`,
// `resume`, or `phase <name>`.
//
// A phase is responsible for:
//
//   - reading state.json to decide whether to skip
//   - reading the profile + GPU evidence to decide what to do
//   - calling apt.Transaction / nvidia helpers / etc.
//   - updating state.json with the outcome (done / skipped /
//     failed_fatal / failed_nonfatal / pending_moonlight_connect)
//   - optionally setting state.RebootNeeded + ResumeTarget so the
//     continuation service picks up after reboot
//
// The Deps struct is shared. Each phase receives the same Deps; the
// `apply` command builds Deps once and passes it through.
package phase

import (
	"context"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// Phase is the common interface every deploy phase implements.
type Phase interface {
	// Name returns the canonical name used in state.json (e.g.
	// "base_packages", "nvidia_driver", "cuda").
	Name() string

	// Run executes the phase. The phase is expected to:
	//   - mark itself running in state
	//   - do its work
	//   - mark itself done / skipped / failed_* in state
	//   - return nil if the deploy should proceed, or a wrapped
	//     error if the deploy should stop.
	Run(ctx context.Context, deps *Deps) error
}

// Deps is what every phase needs.
type Deps struct {
	Runner  *runner.Runner
	APT     *apt.Transaction
	State   *state.State
	Profile *config.Profile
	Logger  *slog.Logger
	DryRun  bool

	// StatePath is the on-disk location of state.json so phases can
	// persist after each step rather than waiting for apply to save
	// at the end.
	StatePath string
}

// PersistState writes Deps.State to Deps.StatePath. Safe to call
// often; the underlying State.Save uses temp + fsync + rename.
func (d *Deps) PersistState() error {
	if d.StatePath == "" {
		// In tests we sometimes skip persistence; that's fine.
		return nil
	}
	return d.State.Save(d.StatePath)
}

// shouldSkip returns true when the phase is already terminal-done
// or terminal-skipped and the operator did not explicitly reset it.
//
// Returns false when the phase is in any other state (pending,
// running, failed_*), letting the phase retry. The running state
// is recovered: a previous deploy that crashed mid-phase leaves the
// status at "running"; we retry.
func shouldSkip(s *state.State, name string) bool {
	p := s.Get(name)
	return p.Status == state.StatusDone || p.Status == state.StatusSkipped
}
