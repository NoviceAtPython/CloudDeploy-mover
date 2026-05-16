package phase

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// CudaName is the canonical state-key.
const CudaName = "cuda"

// Cuda phase: honour cuda.Plan(mode) + cuda.DiscoverCandidate.
// hdr-4k120 uses mode=none and this phase is a no-op there.
type Cuda struct {
	// ProbeFn lets tests inject a candidate availability probe.
	// nil = use real apt-cache via the runner.
	ProbeFn cuda.AvailabilityProbe
}

// Name implements Phase.
func (Cuda) Name() string { return CudaName }

// Run implements Phase.
func (p Cuda) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, CudaName) {
		log.Info("phase cuda: already terminal; skipping")
		return nil
	}

	deps.State.MarkRunning(CudaName)
	_ = deps.PersistState()

	mode := cuda.ModeNone
	explicitName := ""
	driverMajor := ""
	if deps.Profile != nil {
		m, err := cuda.ParseMode(deps.Profile.CUDA.Mode)
		if err != nil {
			deps.State.MarkFailed(CudaName, "parse cuda.mode", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda: %w", err)
		}
		mode = m
		explicitName = deps.Profile.CUDA.PackageName
		driverMajor = deps.Profile.NVIDIA.DriverMajor
	}
	plan := cuda.Plan(mode)

	if !plan.WillAttemptInstall {
		// mode=none. This is the hdr-4k120 default; skip cleanly.
		deps.State.MarkSkipped(CudaName, plan.Rationale)
		deps.State.Get(CudaName).Details = map[string]any{
			"mode":      string(plan.Mode),
			"rationale": plan.Rationale,
		}
		_ = deps.PersistState()
		log.Info("phase cuda: skipped", "mode", string(plan.Mode))
		return nil
	}

	// Discover an installable candidate.
	probe := p.ProbeFn
	if probe == nil {
		probe = makeAptCacheProbe(ctx, deps)
	}
	pkg := cuda.DiscoverCandidate(cuda.CandidateOptions{
		PreferredMajor: driverMajor,
		ExplicitName:   explicitName,
	}, probe)

	if pkg == "" {
		// No installable candidate found.
		if plan.FailsDeployOnError {
			deps.State.MarkFailed(CudaName,
				"no installable CUDA toolkit candidate; mode=required",
				fmt.Errorf("apt-cache reports none of %v are installable", cuda.CandidateLadder(cuda.CandidateOptions{PreferredMajor: driverMajor, ExplicitName: explicitName})),
				true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda (mode=required): no installable candidate")
		}
		// optional: skip nonfatal.
		deps.State.MarkSkipped(CudaName, "no installable CUDA toolkit candidate; mode=optional -> skipped")
		deps.State.Get(CudaName).Details = map[string]any{
			"mode":       string(plan.Mode),
			"rationale":  plan.Rationale,
			"candidates": cuda.CandidateLadder(cuda.CandidateOptions{PreferredMajor: driverMajor, ExplicitName: explicitName}),
		}
		_ = deps.PersistState()
		log.Warn("phase cuda: no installable candidate; skipping (mode=optional)")
		return nil
	}

	log.Info("phase cuda: attempting apt install",
		"package", pkg, "mode", string(plan.Mode))

	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Install(ctx, []string{pkg})
	})
	if err != nil {
		if plan.FailsDeployOnError {
			deps.State.MarkFailed(CudaName, "cuda apt install failed; mode=required", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda (mode=required): %w", err)
		}
		deps.State.MarkFailed(CudaName, "cuda apt install failed; mode=optional", err, false)
		deps.State.Get(CudaName).Details = map[string]any{
			"mode":      string(plan.Mode),
			"rationale": plan.Rationale,
			"package":   pkg,
		}
		_ = deps.PersistState()
		log.Warn("phase cuda: optional install failed; continuing",
			"package", pkg, "err", err)
		return nil
	}

	deps.State.MarkDone(CudaName, map[string]any{
		"mode":    string(plan.Mode),
		"package": pkg,
	})
	_ = deps.PersistState()
	log.Info("phase cuda: done", "mode", string(plan.Mode), "package", pkg)
	return nil
}

// makeAptCacheProbe returns an AvailabilityProbe that shells out to
// `apt-cache policy <pkg>` via the deps runner.
func makeAptCacheProbe(ctx context.Context, deps *Deps) cuda.AvailabilityProbe {
	return func(pkg string) bool {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"apt-cache", "policy", pkg},
			LogFile: "-",
			DryRun:  deps.DryRun,
		})
		if res.Err != nil {
			return false
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Candidate:") {
				cand := strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
				return cand != "" && cand != "(none)"
			}
		}
		return false
	}
}
