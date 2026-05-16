package nvidia

import (
	"errors"
	"fmt"
	"strings"
)

// CleanupAction describes what NvidiaDriver should do before installing
// the target family. Three concrete actions:
//
//   - KeepInstalled: the installed family already matches the
//     selector's choice (and nvidia-smi works). Skip the install.
//   - PurgeConflicting: a conflicting family is installed and is
//     broken; the planner names the packages to purge before installing
//     the target.
//   - RefuseMultipleFamilies: more than one family is installed and
//     the operator hasn't opted into --repair-driver-family. Surface
//     a clear error rather than guess.
//   - Install: the normal path; no cleanup needed.
type CleanupAction int

const (
	CleanupInstall CleanupAction = iota
	CleanupKeepInstalled
	CleanupPurgeConflicting
	CleanupRefuseMultipleFamilies
)

// CleanupPlan is the output of PlanCleanup.
type CleanupPlan struct {
	Action          CleanupAction
	TargetFamily    Family
	PurgePackages   []string // populated when Action == CleanupPurgeConflicting
	ConflictingFams []Family // populated when Action == CleanupPurgeConflicting / RefuseMultipleFamilies
	Reason          string
	Err             error // non-nil for RefuseMultipleFamilies (without --repair flag)
}

// ErrMultipleFamiliesInstalled is the structured error for the
// RefuseMultipleFamilies action.
var ErrMultipleFamiliesInstalled = errors.New("nvidia: multiple driver families installed; pass --repair-driver-family to let CloudDeploy clean up")

// PlanCleanup decides what action to take given the selected target
// family, the current evidence, and whether the operator passed
// --repair-driver-family.
//
// Rules (in order):
//
//  1. Installed-family matches target + nvidia-smi works -> KeepInstalled.
//  2. Multiple families installed -> RefuseMultipleFamilies (unless
//     repair=true, in which case PurgeConflicting to leave only the
//     target family).
//  3. Wrong family installed + open module required (Blackwell or
//     dmesg) + nvidia-smi broken -> PurgeConflicting (purge the wrong
//     family before the target install).
//  4. Wrong family installed + nvidia-smi works -> Install (apt's
//     "install nvidia-driver-X" will purge nvidia-driver-Y if they
//     conflict; we don't pre-purge a working driver).
//  5. No family installed -> Install (normal path, nothing to clean).
//
// The function does NOT call apt. It returns a plan; the phase
// applies it via apt.Transaction.
func PlanCleanup(target Family, ev Evidence, repair bool) CleanupPlan {
	plan := CleanupPlan{Action: CleanupInstall, TargetFamily: target}

	// Rule 1: installed working family matches target.
	installed := ev.AlreadyInstalled()
	if installed == target && ev.NvidiaSmiWorks {
		plan.Action = CleanupKeepInstalled
		plan.Reason = fmt.Sprintf("installed family %q matches target and nvidia-smi works; no install needed", target)
		return plan
	}

	// Rule 2: multiple families installed.
	conflicting := ev.installedFamilies()
	if len(conflicting) > 1 {
		// Filter the target out of the conflict list; we want the
		// names to purge.
		var others []Family
		for _, f := range conflicting {
			if f != target {
				others = append(others, f)
			}
		}
		plan.ConflictingFams = others
		if !repair {
			plan.Action = CleanupRefuseMultipleFamilies
			plan.Reason = fmt.Sprintf("multiple NVIDIA driver families installed: %v; refusing to guess without --repair-driver-family", conflicting)
			plan.Err = ErrMultipleFamiliesInstalled
			return plan
		}
		plan.Action = CleanupPurgeConflicting
		plan.PurgePackages = packagesFor(others, ev)
		plan.Reason = fmt.Sprintf("--repair-driver-family: purging %v before installing %q", others, target)
		return plan
	}

	// Rule 3: wrong family + open required + nvidia-smi broken.
	if installed != FamilyUnknown && installed != target {
		openRequired := ev.requiresOpenKernelModule() || target.IsOpen()
		if openRequired && !ev.NvidiaSmiWorks {
			plan.Action = CleanupPurgeConflicting
			plan.ConflictingFams = []Family{installed}
			plan.PurgePackages = packagesFor([]Family{installed}, ev)
			plan.Reason = fmt.Sprintf("open kernel module required and installed family %q is broken (nvidia-smi fails); purging it before installing %q", installed, target)
			return plan
		}
	}

	// Rule 4 + 5 collapse to Install.
	plan.Reason = fmt.Sprintf("normal install path for %q", target)
	return plan
}

// installedFamilies returns the families currently installed
// (zero, one, or many).
func (e Evidence) installedFamilies() []Family {
	var out []Family
	if e.InstalledServer {
		out = append(out, FamilyServer)
	}
	if e.InstalledServerOpen {
		out = append(out, FamilyServerOpen)
	}
	if e.InstalledNonServer {
		out = append(out, FamilyNonServer)
	}
	if e.InstalledNonServerOpen {
		out = append(out, FamilyNonServerOpen)
	}
	return out
}

// packagesFor returns the apt package names to purge for the given
// families. We attempt the driver + dkms for the driver major the
// host most likely has; if unknown, we use the standard scan list
// (580, 570, 560, 550, 535) so apt purges whichever is installed.
func packagesFor(fams []Family, _ Evidence) []string {
	// We don't know the exact driver major without inspecting
	// /var/lib/dpkg, so emit the package family name with a wildcard
	// the caller can pass directly to apt-get purge. apt-get accepts
	// `nvidia-driver-*-server-open` style globs.
	var out []string
	for _, f := range fams {
		switch f {
		case FamilyServer:
			out = append(out, "nvidia-driver-*-server", "nvidia-dkms-*-server")
		case FamilyServerOpen:
			out = append(out, "nvidia-driver-*-server-open", "nvidia-dkms-*-server-open")
		case FamilyNonServer:
			// "nvidia-driver-*" alone would match both server and
			// non-server. Be explicit: scan the standard majors.
			for _, m := range []string{"580", "570", "560", "550", "535"} {
				out = append(out, "nvidia-driver-"+m, "nvidia-dkms-"+m)
			}
		case FamilyNonServerOpen:
			for _, m := range []string{"580", "570", "560", "550", "535"} {
				out = append(out, "nvidia-driver-"+m+"-open", "nvidia-dkms-"+m+"-open")
			}
		}
	}
	return out
}

// String makes CleanupAction self-describing in log lines + tests.
func (a CleanupAction) String() string {
	switch a {
	case CleanupInstall:
		return "install"
	case CleanupKeepInstalled:
		return "keep-installed"
	case CleanupPurgeConflicting:
		return "purge-conflicting"
	case CleanupRefuseMultipleFamilies:
		return "refuse-multiple-families"
	}
	return "unknown"
}

// -----------------------------------------------------------------------------
// Post-install validation: parse evidence + dmesg snippet to decide
// whether the driver actually works or we need a different action.
// -----------------------------------------------------------------------------

// PostInstallVerdict is what the NvidiaDriver phase consults after
// apt-installing the target family.
type PostInstallVerdict struct {
	OK             bool   // nvidia-smi works; phase can mark Done
	RebootRequired bool   // DKMS module built but not loaded; reboot
	WrongFlavor    bool   // dmesg says "requires NVIDIA open kernel modules" but a closed driver was installed
	Reason         string // human-readable description
}

// EvaluatePostInstall returns the verdict for a host where the driver
// install just finished. dmesgTail is the raw text of the recent
// dmesg / journalctl-k snippet; the function does not invoke any
// system calls.
//
// Rules:
//
//   - nvidia-smi works -> OK.
//   - dmesg contains "requires NVIDIA open kernel modules" + the
//     installed family is closed -> WrongFlavor (the operator should
//     re-run with PreferOpenFamily or wait for the selector to pick
//     the open family next time).
//   - DKMS package is installed but nvidia-smi fails -> RebootRequired.
//   - Otherwise -> not OK + a generic reason.
func EvaluatePostInstall(target Family, ev Evidence, dmesgTail string) PostInstallVerdict {
	if ev.NvidiaSmiWorks {
		return PostInstallVerdict{OK: true, Reason: "nvidia-smi works"}
	}

	if dmesgRequiresOpen(dmesgTail) && !target.IsOpen() {
		return PostInstallVerdict{
			WrongFlavor: true,
			Reason:      "dmesg reports the GPU requires open kernel modules, but a closed driver family is installed; selector chose the wrong family",
		}
	}

	// If DKMS for the target family is installed, the module is most
	// likely built but not loaded yet (kernel cmdline / blacklist /
	// modesetting reasons). Reboot.
	if ev.InstalledServer || ev.InstalledServerOpen || ev.InstalledNonServer || ev.InstalledNonServerOpen {
		return PostInstallVerdict{
			RebootRequired: true,
			Reason:         "driver installed but nvidia-smi failed; reboot required to load the kernel module",
		}
	}

	return PostInstallVerdict{
		Reason: "driver install did not complete; nvidia-smi fails and no driver packages are marked installed",
	}
}

func dmesgRequiresOpen(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "requires use of nvidia open kernel modules") ||
		strings.Contains(lower, "requires use of the nvidia open kernel modules")
}
