package phase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// NvidiaDriverName is the canonical state-key.
const NvidiaDriverName = "nvidia_driver"

// NvidiaDriver phase: pick the family, apt-install, verify with
// nvidia-smi. Sets RebootNeeded when a kernel module change requires
// a reboot before nvidia-smi can succeed.
type NvidiaDriver struct {
	// EvidenceFn allows tests to inject a synthetic Evidence record
	// without touching lspci / dpkg / apt-cache / dmesg on the host.
	// nil = use nvidia.GatherEvidenceFromHost.
	EvidenceFn func(opts nvidia.EvidenceOptions) (nvidia.Evidence, error)

	// Repair, when true, lets PlanCleanup purge conflicting driver
	// families. Off by default; controlled by the
	// --repair-driver-family flag.
	Repair bool

	// DmesgFn lets tests inject a dmesg snippet for the post-install
	// verdict. nil = use the runner.
	DmesgFn func(ctx context.Context, deps *Deps) string
}

// Name implements Phase.
func (NvidiaDriver) Name() string { return NvidiaDriverName }

// Run implements Phase.
func (p NvidiaDriver) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, NvidiaDriverName) {
		log.Info("phase nvidia-driver: already done; skipping")
		return nil
	}

	deps.State.MarkRunning(NvidiaDriverName)
	_ = deps.PersistState()

	// 1. Gather evidence about the host.
	driverMajor := ""
	if deps.Profile != nil {
		driverMajor = deps.Profile.NVIDIA.DriverMajor
	}
	gather := p.EvidenceFn
	if gather == nil {
		gather = nvidia.GatherEvidenceFromHost
	}
	ev, err := gather(nvidia.EvidenceOptions{DriverMajor: driverMajor})
	if err != nil {
		// "no NVIDIA GPU evidence" is fatal: this phase only runs on
		// NVIDIA hosts.
		deps.State.MarkFailed(NvidiaDriverName, "gather evidence", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %w", err)
	}

	// Layer profile hints onto evidence.
	if deps.Profile != nil {
		ev.PreferOpenFamily = ev.PreferOpenFamily || deps.Profile.NVIDIA.PreferOpenFamily
		ev.PreferServerFamily = ev.PreferServerFamily || deps.Profile.NVIDIA.PreferServer
	}

	// 2. Pick a family.
	family, reason, selErr := nvidia.SelectFamily(ev)
	if selErr != nil {
		// ErrNoOpenAvailable / ErrNoFamilyAvailable are deploy-killing.
		deps.State.MarkFailed(NvidiaDriverName, "select family: "+reason, selErr, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %w (%s)", selErr, reason)
	}
	if family == nvidia.FamilyUnknown {
		deps.State.MarkFailed(NvidiaDriverName, "select family returned FamilyUnknown",
			errors.New("FamilyUnknown"), true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: FamilyUnknown (%s)", reason)
	}
	log.Info("phase nvidia-driver: family selected",
		"family", string(family), "reason", reason,
		"gpu", ev.GPUName, "pci_id", ev.PCIID)

	// 3. Plan cleanup (dirty-driver handling).
	plan := nvidia.PlanCleanup(family, ev, p.Repair)
	log.Info("phase nvidia-driver: cleanup plan",
		"action", plan.Action.String(), "reason", plan.Reason)
	switch plan.Action {
	case nvidia.CleanupKeepInstalled:
		details := map[string]any{
			"gpu":             ev.GPUName,
			"pci_id":          ev.PCIID,
			"package_family":  string(family),
			"driver_package":  family.DriverPackage(effectiveMajor(driverMajor)),
			"dkms_package":    family.DkmsPackage(effectiveMajor(driverMajor)),
			"kernel_module":   moduleKind(family),
			"already_present": true,
			"cleanup_action":  plan.Action.String(),
		}
		deps.State.MarkDone(NvidiaDriverName, details)
		_ = deps.PersistState()
		log.Info("phase nvidia-driver: installed family matches; keeping",
			"family", string(family))
		return nil
	case nvidia.CleanupRefuseMultipleFamilies:
		deps.State.MarkFailed(NvidiaDriverName, plan.Reason, plan.Err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %w", plan.Err)
	}

	// 4. apt-purge conflicting families (if any) + apt-install target.
	major := effectiveMajor(driverMajor)
	pkgs := []string{family.DriverPackage(major), family.DkmsPackage(major)}
	log.Info("phase nvidia-driver: installing", "packages", pkgs,
		"purge_first", plan.PurgePackages)
	err = deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		if len(plan.PurgePackages) > 0 {
			log.Info("phase nvidia-driver: purging conflicting packages",
				"packages", plan.PurgePackages)
			if err := tc.Purge(ctx, plan.PurgePackages); err != nil {
				return fmt.Errorf("purge conflicting: %w", err)
			}
		}
		if err := tc.Update(ctx); err != nil {
			return err
		}
		return tc.Install(ctx, pkgs)
	})
	if err != nil {
		deps.State.MarkFailed(NvidiaDriverName, "apt install failed", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %w", err)
	}

	// 5. Re-gather evidence + read dmesg snippet for post-install
	//    verdict.
	postEv, _ := gather(nvidia.EvidenceOptions{DriverMajor: driverMajor})
	dmesg := ""
	if p.DmesgFn != nil {
		dmesg = p.DmesgFn(ctx, deps)
	} else {
		dmesg = readDmesgTail(ctx, deps)
	}
	verdict := nvidia.EvaluatePostInstall(family, postEv, dmesg)

	details := map[string]any{
		"gpu":             ev.GPUName,
		"pci_id":          ev.PCIID,
		"package_family":  string(family),
		"driver_package":  pkgs[0],
		"dkms_package":    pkgs[1],
		"kernel_module":   moduleKind(family),
		"already_present": false,
		"cleanup_action":  plan.Action.String(),
		"post_install":    verdict.Reason,
	}

	switch {
	case verdict.OK:
		deps.State.MarkDone(NvidiaDriverName, details)
		_ = deps.PersistState()
		log.Info("phase nvidia-driver: install + nvidia-smi confirmed; no reboot needed")
		return nil
	case verdict.WrongFlavor:
		// The selector chose the wrong family. This is fatal because
		// a reboot won't fix it; the operator needs to re-run with a
		// different profile / hint.
		deps.State.MarkFailed(NvidiaDriverName,
			"wrong driver family installed; dmesg reports the GPU requires open kernel modules",
			fmt.Errorf("dmesg wrong-flavor signal: %s", verdict.Reason), true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %s", verdict.Reason)
	case verdict.RebootRequired:
		details["reboot_needed"] = true
		deps.State.MarkRunning(NvidiaDriverName)
		deps.State.Get(NvidiaDriverName).Details = details
		deps.State.SetRebootNeeded(true, NvidiaDriverName)
		_ = deps.PersistState()
		log.Warn("phase nvidia-driver: driver installed but nvidia-smi not yet working; reboot required to load new kernel module")
		return ErrRebootRequired
	default:
		// Something else went wrong. Surface fatally.
		deps.State.MarkFailed(NvidiaDriverName, "post-install verdict not OK", fmt.Errorf("%s", verdict.Reason), true)
		_ = deps.PersistState()
		return fmt.Errorf("phase nvidia-driver: %s", verdict.Reason)
	}
}

// readDmesgTail invokes `dmesg | tail -n 200` via the runner and
// returns the captured stdout. Best-effort: returns "" on any error.
func readDmesgTail(ctx context.Context, deps *Deps) string {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"dmesg", "--ctime"},
		LogFile: "-",
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return ""
	}
	return res.Stdout
}

// ErrRebootRequired is the sentinel apply() inspects to decide
// whether to invoke internal/reboot.Schedule.
var ErrRebootRequired = errors.New("phase: reboot required to continue")

// effectiveMajor returns the major to use when the profile didn't
// pin one. Matches the runner-level default.
func effectiveMajor(profileMajor string) string {
	if profileMajor != "" {
		return profileMajor
	}
	return "580"
}

func moduleKind(f nvidia.Family) string {
	if f.IsOpen() {
		return "open"
	}
	return "closed"
}
