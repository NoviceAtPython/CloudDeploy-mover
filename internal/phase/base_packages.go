package phase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
)

// BasePackagesName is the canonical state-key for the base-packages
// phase. Exported so tests / doctor can reference it without
// hard-coding the string.
const BasePackagesName = "base_packages"

// BasePackagesList is the minimum tooling clouddeployctl needs to do
// its job. Deliberately conservative: we want to fail-fast if any
// of these is unreachable on the configured apt sources, not silently
// pull in five hundred recommends.
//
// Intentionally NOT here (Milestone 4):
//   - libavcodec-dev, cmake, ninja, libpipewire-0.3-dev, etc.
//     (Sunshine fork build prerequisites)
//   - kde-plasma-desktop, kwin-wayland, plasma-workspace, etc.
//     (compositor)
//   - libdrm-dev / drm_info (DRM HDR validation)
//
// Those land when their respective phases land.
var BasePackagesList = []string{
	"ca-certificates",
	"curl",
	"git",
	"build-essential",
	"pkg-config",
	"python3",
	"python3-venv",
	"jq",
	"pciutils",
	"lsb-release",
	"systemd",
	// dmsetup is needed for LVM-on-cloud-VM hosts; harmless on others.
	// procps for ps / lock-holder enumeration.
	"procps",
	// ffmpeg: the gpu-capture-capability-probe shells out to the
	// `ffmpeg` CLI for its h264_nvenc / hevc_main10_nvenc smoke tests.
	// It is NOT shipped by the base cloud images (and the Sunshine fork
	// bundles its own libav, not the CLI), so without this the probe
	// failed_fatal on a clean deploy with "failed to execute ffmpeg:
	// No such file or directory". Installing it here (well before the
	// probe phase) keeps the probe self-sufficient.
	"ffmpeg",
}

// BasePackages is the apt-install bootstrap phase. Idempotent: when
// every package is already installed, the phase exits without
// calling apt.
type BasePackages struct{}

// Name implements Phase.
func (BasePackages) Name() string { return BasePackagesName }

// Run implements Phase.
func (BasePackages) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, BasePackagesName) {
		log.Info("phase base-packages: already done; skipping")
		return nil
	}

	deps.State.MarkRunning(BasePackagesName)
	if err := deps.PersistState(); err != nil {
		// State persistence failure is non-fatal mid-run; we'll try
		// again at the end. Log it.
		log.Warn("phase base-packages: persist running marker failed",
			"err", err)
	}

	// apt-get update first so the install resolves against fresh
	// indices; then install the package list inside the same
	// transaction (single policy-rc.d guard).
	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		if err := tc.Update(ctx); err != nil {
			return fmt.Errorf("apt-get update: %w", err)
		}
		if err := tc.Install(ctx, BasePackagesList); err != nil {
			return fmt.Errorf("apt-get install base packages: %w", err)
		}
		return nil
	})
	if err != nil {
		deps.State.MarkFailed(BasePackagesName, "apt transaction failed", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase base-packages: %w", err)
	}

	deps.State.MarkDone(BasePackagesName, map[string]any{
		"installed": BasePackagesList,
	})
	if err := deps.PersistState(); err != nil {
		log.Warn("phase base-packages: persist done marker failed",
			"err", err)
	}
	log.Info("phase base-packages: done",
		"packages", len(BasePackagesList))
	return nil
}
