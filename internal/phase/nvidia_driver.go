package phase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
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

	// 3. If the installed family already matches AND nvidia-smi works,
	//    mark done. This is the v2-regression-test rule.
	if ev.AlreadyInstalled() == family && ev.NvidiaSmiWorks {
		details := map[string]any{
			"gpu":             ev.GPUName,
			"pci_id":          ev.PCIID,
			"package_family":  string(family),
			"driver_package":  family.DriverPackage(effectiveMajor(driverMajor)),
			"dkms_package":    family.DkmsPackage(effectiveMajor(driverMajor)),
			"kernel_module":   moduleKind(family),
			"already_present": true,
		}
		deps.State.MarkDone(NvidiaDriverName, details)
		_ = deps.PersistState()
		log.Info("phase nvidia-driver: installed family matches; keeping",
			"family", string(family))
		return nil
	}

	// 4. Install via apt.Transaction.
	major := effectiveMajor(driverMajor)
	pkgs := []string{family.DriverPackage(major), family.DkmsPackage(major)}
	log.Info("phase nvidia-driver: installing", "packages", pkgs)
	err = deps.APT.Run(ctx, func(tc *apt.TxContext) error {
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

	// 5. Re-gather evidence and decide whether a reboot is needed.
	postEv, _ := gather(nvidia.EvidenceOptions{DriverMajor: driverMajor})
	details := map[string]any{
		"gpu":             ev.GPUName,
		"pci_id":          ev.PCIID,
		"package_family":  string(family),
		"driver_package":  pkgs[0],
		"dkms_package":    pkgs[1],
		"kernel_module":   moduleKind(family),
		"already_present": false,
	}
	if postEv.NvidiaSmiWorks {
		// Module loaded successfully without a reboot. Rare on a
		// fresh install (DKMS usually needs a kernel re-link) but
		// possible on a re-deploy of an already-up host.
		deps.State.MarkDone(NvidiaDriverName, details)
		_ = deps.PersistState()
		log.Info("phase nvidia-driver: install + nvidia-smi confirmed; no reboot needed")
		return nil
	}

	// 6. Reboot needed. Set the marker so resume picks up after boot.
	details["reboot_needed"] = true
	deps.State.MarkRunning(NvidiaDriverName)
	// Carry over the details for state show.
	deps.State.Get(NvidiaDriverName).Details = details
	deps.State.SetRebootNeeded(true, NvidiaDriverName)
	_ = deps.PersistState()

	log.Warn("phase nvidia-driver: driver installed but nvidia-smi not yet working; reboot required to load new kernel module")
	return ErrRebootRequired
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
