// Package nvidia owns NVIDIA driver package-family selection.
//
// The driver package matrix on Ubuntu:
//
//	server          nvidia-driver-${MAJOR}-server         closed kernel module
//	server-open     nvidia-driver-${MAJOR}-server-open    open kernel module
//	non-server      nvidia-driver-${MAJOR}                closed kernel module
//	non-server-open nvidia-driver-${MAJOR}-open           open kernel module
//
// Picking the wrong family wedges the deploy. Three v2-era failure
// modes drive the design here:
//
//   - RTX 5090 / GB202 cannot use the closed kernel module on current
//     driver branches. The deploy MUST pick an *-open package.
//   - v2's validator demanded `nvidia-driver-580-server` even when the
//     operator had `nvidia-driver-580-server-open` installed and
//     working. A working installed family must be preserved.
//   - The selector must not pick a family that apt cannot install
//     ("server-open" is the right answer only if `server-open` is
//     actually available in the configured apt sources).
//
// SelectFamily takes a typed Evidence record and returns a typed
// Family + reason + error. Pure function; all I/O lives in host.go
// and the wider deploy phase.
package nvidia

import (
	"errors"
	"fmt"
	"strings"
)

// Family is the NVIDIA driver package family.
type Family string

const (
	FamilyServer        Family = "server"
	FamilyServerOpen    Family = "server-open"
	FamilyNonServer     Family = "non-server"
	FamilyNonServerOpen Family = "non-server-open"
	FamilyUnknown       Family = ""
)

// AllFamilies returns every concrete family in a stable order. Used
// by config validators and by the doctor report.
func AllFamilies() []Family {
	return []Family{FamilyServer, FamilyServerOpen, FamilyNonServer, FamilyNonServerOpen}
}

// IsOpen reports whether the family uses the NVIDIA open kernel module.
// Blackwell consumer (RTX 50-series) requires this.
func (f Family) IsOpen() bool {
	return f == FamilyServerOpen || f == FamilyNonServerOpen
}

// IsServer reports whether the family is in the *-server variant.
// Data-center deploys typically want this.
func (f Family) IsServer() bool {
	return f == FamilyServer || f == FamilyServerOpen
}

// DriverPackage returns the apt package name for this family at the
// requested driver major version.
func (f Family) DriverPackage(major string) string {
	switch f {
	case FamilyServer:
		return fmt.Sprintf("nvidia-driver-%s-server", major)
	case FamilyServerOpen:
		return fmt.Sprintf("nvidia-driver-%s-server-open", major)
	case FamilyNonServer:
		return fmt.Sprintf("nvidia-driver-%s", major)
	case FamilyNonServerOpen:
		return fmt.Sprintf("nvidia-driver-%s-open", major)
	}
	return ""
}

// DkmsPackage returns the dkms package name for this family.
func (f Family) DkmsPackage(major string) string {
	switch f {
	case FamilyServer:
		return fmt.Sprintf("nvidia-dkms-%s-server", major)
	case FamilyServerOpen:
		return fmt.Sprintf("nvidia-dkms-%s-server-open", major)
	case FamilyNonServer:
		return fmt.Sprintf("nvidia-dkms-%s", major)
	case FamilyNonServerOpen:
		return fmt.Sprintf("nvidia-dkms-%s-open", major)
	}
	return ""
}

// ErrNoOpenAvailable is the structured error returned when the GPU
// requires an open kernel module family but no *-open package family
// is available in apt.
var ErrNoOpenAvailable = errors.New("nvidia: GPU requires the NVIDIA open kernel module but no -open driver package family is available from apt")

// ErrNoFamilyAvailable is returned when none of the four families is
// available in apt. The driver-major may simply be wrong (e.g. asking
// for 580 on a release that only ships up to 570).
var ErrNoFamilyAvailable = errors.New("nvidia: no NVIDIA driver package family is available from apt for the requested driver major")

// Evidence is the input to SelectFamily. All fields are zero-value
// safe; an empty Evidence is "no evidence", not "all false".
type Evidence struct {
	// ---- Hardware ----
	PCIID               string // e.g. "10de:2b85"
	GPUName             string // e.g. "NVIDIA GeForce RTX 5090"
	IsBlackwellConsumer bool   // GB202 / RTX 50-series GeForce; closed module unsupported
	IsBlackwellPro      bool   // RTX PRO Blackwell workstation; closed module unsupported
	IsBlackwellDC       bool   // B100 / B200 / GB200 datacenter Blackwell; closed module unsupported
	IsDataCenter        bool   // L4 / L40 / A10 / A100 / H100 / etc.
	IsLegacyPascal      bool   // P100 / P40 / P4: no AV1, no HDR; surface a warning

	// ---- Kernel evidence ----
	// dmesg NVRM line "requires use of NVIDIA open kernel modules" /
	// "requires use of the NVIDIA open kernel modules". Highest-priority
	// signal: if the kernel said so, the kernel said so.
	DmesgRequiresOpenKernelModule bool

	// ---- dpkg state ----
	InstalledServer        bool
	InstalledServerOpen    bool
	InstalledNonServer     bool
	InstalledNonServerOpen bool

	// ---- apt availability ----
	// AvailabilityKnown distinguishes "we have not run apt-cache yet
	// (read-only doctor mode; assume optimistic)" from "we did run
	// apt-cache and it returned what's in the AvailableXxx fields
	// (which may all be false, meaning nothing is installable)".
	//
	// When false:
	//   IsAvailable() returns true for every family (optimistic).
	//   SelectFamily() will not surface ErrNoFamilyAvailable solely
	//   because every AvailableXxx is zero.
	// When true:
	//   IsAvailable() honors the AvailableXxx flags exactly.
	//   SelectFamily() will return ErrNoFamilyAvailable when no
	//   family is installable (or ErrNoOpenAvailable when open is
	//   required but no *-open family is installable).
	//
	// This is the v2-bug fix: previously a host with no apt-cache
	// scan looked indistinguishable from a host where apt-cache
	// reported nothing available, so the selector either over-failed
	// or over-succeeded depending on which side of the ambiguity
	// you read.
	AvailabilityKnown bool

	AvailableServer        bool
	AvailableServerOpen    bool
	AvailableNonServer     bool
	AvailableNonServerOpen bool

	// ---- Runtime ----
	NvidiaSmiWorks bool

	// ---- Profile hints (from config/profiles/<name>.yaml) ----
	//
	// PreferOpenFamily is SOFT. It biases the selector toward
	// server-open -> non-server-open, but if neither *-open is
	// available AND the GPU does not REQUIRE the open kernel module
	// (i.e. not Blackwell, no dmesg signal), the selector falls back
	// to server -> non-server rather than failing.
	//
	// PreferServerFamily is SOFT. It biases the selector toward
	// server -> server-open -> non-server -> non-server-open. Server
	// family is the canonical NVIDIA recommendation for data-center
	// deploys.
	//
	// The "hard open requirement" is hardware-driven only: dmesg
	// signal OR Blackwell consumer/pro/datacenter.
	PreferOpenFamily   bool
	PreferServerFamily bool
}

// AlreadyInstalled returns the family currently installed on this
// host, or FamilyUnknown if zero or more than one is installed.
func (e Evidence) AlreadyInstalled() Family {
	count := 0
	var got Family
	if e.InstalledServer {
		count++
		got = FamilyServer
	}
	if e.InstalledServerOpen {
		count++
		got = FamilyServerOpen
	}
	if e.InstalledNonServer {
		count++
		got = FamilyNonServer
	}
	if e.InstalledNonServerOpen {
		count++
		got = FamilyNonServerOpen
	}
	if count == 1 {
		return got
	}
	return FamilyUnknown
}

// IsAvailable reports whether a specific family is available from apt.
//
// When AvailabilityKnown is false (we have not consulted apt-cache
// yet), every family is treated as available. This is the optimistic
// read-only mode used by `doctor` on a developer host that does not
// have NVIDIA's apt sources configured.
//
// When AvailabilityKnown is true, the AvailableXxx flag is honoured
// exactly. All-false is a meaningful "nothing available" state.
func (e Evidence) IsAvailable(f Family) bool {
	if !e.AvailabilityKnown {
		return true
	}
	switch f {
	case FamilyServer:
		return e.AvailableServer
	case FamilyServerOpen:
		return e.AvailableServerOpen
	case FamilyNonServer:
		return e.AvailableNonServer
	case FamilyNonServerOpen:
		return e.AvailableNonServerOpen
	}
	return false
}

// HasAvailabilityInfo is retained as a convenience predicate. Returns
// true when AvailabilityKnown is set OR any AvailableXxx flag is true.
// Tests that don't care about the trinary use this; production code
// should use AvailabilityKnown directly.
func (e Evidence) HasAvailabilityInfo() bool {
	return e.AvailabilityKnown || e.AvailableServer || e.AvailableServerOpen ||
		e.AvailableNonServer || e.AvailableNonServerOpen
}

// anyAvailable returns true if at least one family is reachable. When
// AvailabilityKnown is false, the optimistic IsAvailable shortcut
// makes this trivially true.
func (e Evidence) anyAvailable() bool {
	if !e.AvailabilityKnown {
		return true
	}
	return e.AvailableServer || e.AvailableServerOpen ||
		e.AvailableNonServer || e.AvailableNonServerOpen
}

// requiresOpenKernelModule returns true if any hard evidence makes
// the closed kernel module unusable.
func (e Evidence) requiresOpenKernelModule() bool {
	return e.DmesgRequiresOpenKernelModule ||
		e.IsBlackwellConsumer ||
		e.IsBlackwellPro ||
		e.IsBlackwellDC
}

// SelectFamily returns the family the deploy should use, plus a
// human-readable reason and an error.
//
// The "open kernel module required" axis is hardware-driven:
//   - dmesg "requires use of NVIDIA open kernel modules"
//   - Blackwell GPU (consumer, pro, datacenter)
//
// In both of those cases the selector MUST pick a *-open family or
// fail with ErrNoOpenAvailable.
//
// PreferOpenFamily on a non-Blackwell GPU is a SOFT bias: it sorts
// *-open ahead of closed in the preference order, but if no *-open
// is installable the selector still picks server or non-server when
// the GPU does not need open. This is the v3-brief behaviour change
// from Milestone 1.1's first cut.
//
// Precedence (highest wins):
//
//  1. dmesg requires open OR Blackwell -> hard open requirement.
//     Try server-open -> non-server-open. Anything else returns
//     ErrNoOpenAvailable.
//
//  2. One family installed + nvidia-smi works -> keep it.
//     (Skipped when 1 triggered, since the kernel said the
//     installed closed module is doomed.)
//
//  3. Data-center + PreferServerFamily -> closed-first preference
//     (server -> server-open -> non-server -> non-server-open).
//
//  4. PreferOpenFamily (soft) -> open-first preference
//     (server-open -> non-server-open -> server -> non-server).
//
//  5. PreferServerFamily (non-data-center) -> same closed-first
//     chain as 3.
//
//  6. Default -> server-open -> server -> non-server-open -> non-server.
//
// When AvailabilityKnown is true and no family is reachable at all,
// returns ErrNoFamilyAvailable.
func SelectFamily(e Evidence) (Family, string, error) {
	// 1: hard open requirement.
	if e.requiresOpenKernelModule() {
		f, reason := firstAvailable(e, openOnlyOrder)
		if f == FamilyUnknown {
			return FamilyUnknown, openRequiredReason(e), ErrNoOpenAvailable
		}
		return f, openRequiredReason(e) + "; " + reason, nil
	}

	// 2: working installed driver wins. (Not reached when 1 fired.)
	if installed := e.AlreadyInstalled(); installed != FamilyUnknown && e.NvidiaSmiWorks {
		return installed, fmt.Sprintf("installed family %q already loaded and nvidia-smi works", installed), nil
	}

	// Before falling through to preference-based selection, refuse
	// fast when AvailabilityKnown=true and nothing is installable.
	if !e.anyAvailable() {
		return FamilyUnknown, "apt-cache reports no NVIDIA driver family available for the configured driver major", ErrNoFamilyAvailable
	}

	// 3: data-center + prefer-server.
	if e.IsDataCenter && e.PreferServerFamily {
		f, reason := firstAvailable(e, serverFirstOrder)
		if f == FamilyUnknown {
			return FamilyUnknown, "data-center GPU + profile prefers server family", ErrNoFamilyAvailable
		}
		return f, "data-center GPU + profile prefers server family; " + reason, nil
	}

	// 4: prefer-open hint (SOFT).
	if e.PreferOpenFamily {
		f, reason := firstAvailable(e, openFirstOrder)
		if f == FamilyUnknown {
			return FamilyUnknown, "profile prefers open family", ErrNoFamilyAvailable
		}
		return f, "profile prefers open family (soft); " + reason, nil
	}

	// 5: prefer-server hint.
	if e.PreferServerFamily {
		f, reason := firstAvailable(e, serverFirstOrder)
		if f == FamilyUnknown {
			return FamilyUnknown, "profile prefers server family", ErrNoFamilyAvailable
		}
		return f, "profile prefers server family; " + reason, nil
	}

	// 6: default fall-through.
	f, reason := firstAvailable(e, defaultOrder)
	if f == FamilyUnknown {
		return FamilyUnknown, "default: no family available", ErrNoFamilyAvailable
	}
	return f, "default: " + reason, nil
}

// Preference orderings. The selector walks the slice in order and
// returns the first available family.
var (
	// openOnlyOrder is used when the GPU REQUIRES the open kernel
	// module. Only *-open families are considered; closed families
	// are deliberately absent so we hit ErrNoOpenAvailable rather
	// than silently installing a doomed closed driver.
	openOnlyOrder = []Family{FamilyServerOpen, FamilyNonServerOpen}

	// openFirstOrder is the SOFT PreferOpenFamily ordering: open
	// preferred, but closed accepted as fallback for GPUs that work
	// with either module.
	openFirstOrder = []Family{FamilyServerOpen, FamilyNonServerOpen, FamilyServer, FamilyNonServer}

	// serverFirstOrder is the data-center / PreferServerFamily
	// ordering: closed-first, then open as fallback.
	serverFirstOrder = []Family{FamilyServer, FamilyServerOpen, FamilyNonServer, FamilyNonServerOpen}

	// defaultOrder is what we pick when the operator expresses no
	// preference. server-open is the safest modern choice; closed
	// server is the second-safest; non-server-* is the last resort.
	defaultOrder = []Family{FamilyServerOpen, FamilyServer, FamilyNonServerOpen, FamilyNonServer}
)

// firstAvailable returns the first family in order that IsAvailable
// reports as installable, along with a one-line "why this one" reason.
func firstAvailable(e Evidence, order []Family) (Family, string) {
	for i, candidate := range order {
		if e.IsAvailable(candidate) {
			if i == 0 {
				return candidate, fmt.Sprintf("%s available", candidate)
			}
			return candidate, fmt.Sprintf("falling back to %s (preferred families ahead of it are not installable)", candidate)
		}
	}
	return FamilyUnknown, ""
}

func openRequiredReason(e Evidence) string {
	switch {
	case e.DmesgRequiresOpenKernelModule:
		return "dmesg: NVIDIA driver requires open kernel modules"
	case e.IsBlackwellConsumer:
		return "Blackwell consumer GPU (e.g. RTX 5090): closed kernel module unsupported"
	case e.IsBlackwellPro:
		return "Blackwell RTX PRO workstation GPU: closed kernel module unsupported"
	case e.IsBlackwellDC:
		return "Blackwell datacenter GPU (e.g. B100/B200/GB200): closed kernel module unsupported"
	}
	return "open kernel module required by hardware"
}

// ValidateInstalled checks that the installed dpkg state matches the
// expected family, and that nvidia-smi works. Returns nil when the
// system is in the expected state; returns a descriptive error when
// it is not.
//
// This is what `doctor nvidia` and the nvidia_driver phase's
// idempotency guard call.
func ValidateInstalled(want Family, e Evidence) error {
	if want == FamilyUnknown {
		return errors.New("nvidia: ValidateInstalled called with FamilyUnknown")
	}
	got := e.AlreadyInstalled()
	if got == FamilyUnknown {
		switch {
		case !anyInstalled(e):
			return fmt.Errorf("nvidia: expected family %q installed; nothing installed", want)
		default:
			return fmt.Errorf("nvidia: multiple NVIDIA driver families installed; expected %q", want)
		}
	}
	if got != want {
		return fmt.Errorf("nvidia: installed family %q does not match expected %q", got, want)
	}
	if !e.NvidiaSmiWorks {
		return fmt.Errorf("nvidia: family %q is installed but nvidia-smi does not work; suspect dkms/initramfs/kernel-module mismatch", want)
	}
	return nil
}

func anyInstalled(e Evidence) bool {
	return e.InstalledServer || e.InstalledServerOpen || e.InstalledNonServer || e.InstalledNonServerOpen
}

// ParsePCIID extracts the "vendor:device" pair from an lspci line such
// as "01:00.0 VGA compatible controller [0300]: NVIDIA Corporation
// GB202 [GeForce RTX 5090] [10de:2b85] (rev a1)". Returns "" if no
// match.
func ParsePCIID(lspciLine string) string {
	for i := 0; i < len(lspciLine); i++ {
		if lspciLine[i] != '[' {
			continue
		}
		closeIdx := strings.IndexByte(lspciLine[i:], ']')
		if closeIdx < 0 {
			continue
		}
		inner := lspciLine[i+1 : i+closeIdx]
		if isPCIID(inner) {
			return inner
		}
	}
	return ""
}

func isPCIID(s string) bool {
	if len(s) != 9 {
		return false
	}
	if s[4] != ':' {
		return false
	}
	for _, idx := range []int{0, 1, 2, 3, 5, 6, 7, 8} {
		c := s[idx]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
