package phase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
)

// CudaName is the canonical state-key.
const CudaName = "cuda"

// Cuda phase: honour cuda.Plan(mode). For Milestone 3 we install only
// from apt (the runfile path lands in Milestone 4 since hdr-4k120
// uses mode=none anyway). The RetryDecider from internal/cuda is wired
// in by the runfile phase when that lands.
type Cuda struct{}

// Name implements Phase.
func (Cuda) Name() string { return CudaName }

// Run implements Phase.
func (Cuda) Run(ctx context.Context, deps *Deps) error {
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
	if deps.Profile != nil {
		m, err := cuda.ParseMode(deps.Profile.CUDA.Mode)
		if err != nil {
			deps.State.MarkFailed(CudaName, "parse cuda.mode", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda: %w", err)
		}
		mode = m
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

	// optional / required: attempt the apt-toolkit install. The
	// runfile path is Milestone 4.
	major := "13"
	if deps.Profile != nil && deps.Profile.NVIDIA.DriverMajor == "535" {
		// Driver 535 pairs with CUDA 12.
		major = "12"
	}
	pkg := "cuda-toolkit-" + major
	log.Info("phase cuda: attempting apt install", "package", pkg, "mode", string(plan.Mode))

	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		// Note: we don't run apt-get update here; base-packages
		// already did. If the operator runs `phase cuda` standalone
		// without base-packages, they get whatever apt-cache has.
		return tc.Install(ctx, []string{pkg})
	})
	if err != nil {
		if plan.FailsDeployOnError {
			deps.State.MarkFailed(CudaName, "cuda apt install failed; mode=required", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda (mode=required): %w", err)
		}
		// optional: continue.
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
