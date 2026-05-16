package nvidia

import (
	"errors"
	"strings"
	"testing"
)

func TestPlanCleanup_KeepInstalledWhenMatch(t *testing.T) {
	ev := Evidence{InstalledServerOpen: true, NvidiaSmiWorks: true}
	plan := PlanCleanup(FamilyServerOpen, ev, false)
	if plan.Action != CleanupKeepInstalled {
		t.Errorf("Action: got %s want keep-installed", plan.Action)
	}
}

func TestPlanCleanup_RefusesMultipleFamiliesWithoutRepair(t *testing.T) {
	ev := Evidence{
		InstalledServer:     true,
		InstalledServerOpen: true,
	}
	plan := PlanCleanup(FamilyServerOpen, ev, false)
	if plan.Action != CleanupRefuseMultipleFamilies {
		t.Errorf("Action: got %s want refuse-multiple-families", plan.Action)
	}
	if !errors.Is(plan.Err, ErrMultipleFamiliesInstalled) {
		t.Errorf("Err should wrap ErrMultipleFamiliesInstalled: got %v", plan.Err)
	}
}

func TestPlanCleanup_RepairsMultipleFamilies(t *testing.T) {
	ev := Evidence{
		InstalledServer:     true,
		InstalledServerOpen: true,
	}
	plan := PlanCleanup(FamilyServerOpen, ev, true)
	if plan.Action != CleanupPurgeConflicting {
		t.Errorf("Action: got %s want purge-conflicting", plan.Action)
	}
	if plan.Err != nil {
		t.Errorf("Err should be nil under --repair: got %v", plan.Err)
	}
	if len(plan.ConflictingFams) != 1 || plan.ConflictingFams[0] != FamilyServer {
		t.Errorf("ConflictingFams should be [server]: %v", plan.ConflictingFams)
	}
}

func TestPlanCleanup_PurgesConflictingClosedWhenOpenRequired(t *testing.T) {
	// RTX 5090 with broken closed-server install. Open family is
	// the new target; conflicting closed-server must be purged.
	ev := Evidence{
		IsBlackwellConsumer: true,
		InstalledServer:     true,
		NvidiaSmiWorks:      false,
	}
	plan := PlanCleanup(FamilyServerOpen, ev, false)
	if plan.Action != CleanupPurgeConflicting {
		t.Errorf("Action: got %s want purge-conflicting", plan.Action)
	}
	if !strings.Contains(plan.Reason, "open kernel module required") {
		t.Errorf("Reason should mention open kernel module: %q", plan.Reason)
	}
	hasServerPattern := false
	for _, p := range plan.PurgePackages {
		if strings.Contains(p, "-server") && !strings.Contains(p, "open") {
			hasServerPattern = true
			break
		}
	}
	if !hasServerPattern {
		t.Errorf("PurgePackages should mention nvidia-driver-*-server (closed): %v", plan.PurgePackages)
	}
}

func TestPlanCleanup_DoesNotPurgeWorkingMismatchedFamily(t *testing.T) {
	// Mismatched family but nvidia-smi works -> Rule 4: just Install
	// (let apt handle the swap).
	ev := Evidence{
		InstalledServer: true,
		NvidiaSmiWorks:  true,
	}
	plan := PlanCleanup(FamilyServerOpen, ev, false)
	if plan.Action != CleanupInstall {
		t.Errorf("Action: got %s want install", plan.Action)
	}
}

func TestPlanCleanup_NoFamilyInstalled(t *testing.T) {
	plan := PlanCleanup(FamilyServerOpen, Evidence{}, false)
	if plan.Action != CleanupInstall {
		t.Errorf("Action: got %s want install", plan.Action)
	}
}

// -----------------------------------------------------------------------------
// EvaluatePostInstall
// -----------------------------------------------------------------------------

func TestEvaluatePostInstall_OK(t *testing.T) {
	v := EvaluatePostInstall(FamilyServerOpen, Evidence{NvidiaSmiWorks: true}, "")
	if !v.OK {
		t.Errorf("OK should be true when nvidia-smi works")
	}
	if v.RebootRequired || v.WrongFlavor {
		t.Errorf("OK case should not flag reboot or wrong-flavor: %+v", v)
	}
}

func TestEvaluatePostInstall_WrongFlavor(t *testing.T) {
	// dmesg says open required, but selector chose closed.
	dmesg := "[ 12.345] NVRM: NVIDIA: This GPU requires use of NVIDIA open kernel modules"
	v := EvaluatePostInstall(FamilyServer, Evidence{InstalledServer: true}, dmesg)
	if !v.WrongFlavor {
		t.Errorf("expected WrongFlavor")
	}
	if v.RebootRequired {
		t.Errorf("WrongFlavor should not also be RebootRequired: %+v", v)
	}
	if !strings.Contains(v.Reason, "wrong family") {
		t.Errorf("Reason should explain the family mistake: %q", v.Reason)
	}
}

func TestEvaluatePostInstall_RebootRequired(t *testing.T) {
	// DKMS installed but nvidia-smi fails; not open-required.
	v := EvaluatePostInstall(FamilyServerOpen, Evidence{InstalledServerOpen: true}, "")
	if !v.RebootRequired {
		t.Errorf("expected RebootRequired")
	}
}

func TestEvaluatePostInstall_NothingInstalled(t *testing.T) {
	v := EvaluatePostInstall(FamilyServerOpen, Evidence{}, "")
	if v.OK || v.RebootRequired || v.WrongFlavor {
		t.Errorf("nothing-installed verdict should be a clean not-OK: %+v", v)
	}
}

func TestDmesgRequiresOpenCaseInsensitive(t *testing.T) {
	if !dmesgRequiresOpen("REQUIRES USE OF NVIDIA OPEN KERNEL MODULES") {
		t.Error("uppercase variant should match")
	}
	if !dmesgRequiresOpen("requires use of the NVIDIA open kernel modules") {
		t.Error("'the NVIDIA' variant should match")
	}
	if dmesgRequiresOpen("some other dmesg line") {
		t.Error("unrelated text should not match")
	}
}
