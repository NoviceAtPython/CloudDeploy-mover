// Package nvidia owns NVIDIA driver package-family selection.
//
// The driver package matrix on Ubuntu:
//
//	server          nvidia-driver-${MAJOR}-server         closed kernel module
//	server-open     nvidia-driver-${MAJOR}-server-open    open kernel module
//	non-server      nvidia-driver-${MAJOR}                closed kernel module
//	non-server-open nvidia-driver-${MAJOR}-open           open kernel module
//
// Picking the wrong family wedges the deploy. The RTX 5090 / GB202
// case from the v2 deploy is the motivating example: the standard
// closed kernel module rejects Blackwell consumer GPUs; the install
// must use server-open (or non-server-open). v2's selector picked
// server and then v2's validator refused to accept the manual
// server-open install the operator performed.
//
// SelectFamily takes a typed Evidence record and returns a typed
// Family + a human-readable reason. The decision tree is data, not
// nested if-statements, so it can be table-tested.
//
// Inputs (Evidence struct):
//
//   - PCIID / GPUName (from lspci -nn)
//   - IsBlackwellConsumer (heuristic: GeForce RTX 50-series consumer
//     SKU; closed kernel module unsupported)
//   - IsDataCenter (L4, A10, A100, H100 - prefer server family)
//   - DmesgRequiresOpenKernelModule (NVRM message in dmesg)
//   - InstalledServer / InstalledServerOpen / InstalledNonServer /
//     InstalledNonServerOpen (dpkg state)
//   - NvidiaSmiWorks (does nvidia-smi succeed against the running
//     module?)
//   - PreferOpenFamily (profile hint, defaults to true for HDR-4K120)
//   - PreferServerFamily (profile hint for data-center deploys)
//
// Validation rules:
//
//   - Once a family is selected, the system is considered "correct"
//     when that family's package is installed AND nvidia-smi works.
//   - The selector MUST NOT demand server packages when server-open
//     is the chosen family. v2's bug.
//
// All decisions are pure functions of Evidence. Side-effect code lives
// in apply.go / install.go (Milestone 4).
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

// Evidence is the input to SelectFamily.
type Evidence struct {
	// Hardware
	PCIID               string // e.g. "10de:2b85"
	GPUName             string // e.g. "NVIDIA GeForce RTX 5090"
	IsBlackwellConsumer bool   // GB202 / RTX 50-series GeForce
	IsDataCenter        bool   // L4 / A10 / A100 / H100

	// Kernel evidence
	DmesgRequiresOpenKernelModule bool // NVRM: "requires use of NVIDIA open kernel modules"

	// dpkg state (mutually-exclusive in practice, but we record all)
	InstalledServer        bool
	InstalledServerOpen    bool
	InstalledNonServer     bool
	InstalledNonServerOpen bool

	// Runtime
	NvidiaSmiWorks bool

	// Profile hints (from config/profiles/<name>.yaml)
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

// SelectFamily returns the family the deploy should use, plus a
// human-readable reason explaining the decision.
//
// Precedence (highest wins):
//  1. dmesg "requires use of NVIDIA open kernel modules" -> open family.
//  2. Blackwell consumer GPU -> open family.
//  3. Working installed family + nvidia-smi works -> keep it.
//  4. Data-center GPU + PreferServerFamily -> server.
//  5. PreferOpenFamily profile hint -> open family.
//  6. PreferServerFamily profile hint -> server family.
//  7. Default -> server-open (safest modern default).
//
// The open vs closed axis takes precedence over the server vs
// non-server axis because picking the wrong open/closed choice can
// brick the install; picking the wrong server/non-server choice
// usually still produces a working driver, just from a non-ideal apt
// channel.
func SelectFamily(e Evidence) (Family, string) {
	// 1. dmesg evidence is the strongest signal: the kernel itself
	//    told us the closed module was rejected.
	if e.DmesgRequiresOpenKernelModule {
		return openVariant(e), "dmesg: NVIDIA driver requires open kernel modules"
	}

	// 2. Blackwell consumer GPUs (GB202 = RTX 50-series GeForce)
	//    cannot use the closed module at all on current driver
	//    branches; force open regardless of profile hint.
	if e.IsBlackwellConsumer {
		return openVariant(e), "Blackwell consumer GPU (e.g. RTX 5090): closed kernel module unsupported"
	}

	// 3. If exactly one family is installed AND nvidia-smi is
	//    working, the safest thing is to keep it. v2 violated this
	//    rule for server-open and ended up uninstalling a working
	//    driver to "fix" a misdetected family.
	if installed := e.AlreadyInstalled(); installed != FamilyUnknown && e.NvidiaSmiWorks {
		return installed, fmt.Sprintf("installed family %q already loaded and nvidia-smi works", installed)
	}

	// 4. Data-center GPU + profile prefers server: server (closed) is
	//    the canonical NVIDIA recommendation for headless/data-center
	//    deploys. Open module also works on these but the deploy is
	//    less likely to surprise an operator who's expecting the
	//    closed module.
	if e.IsDataCenter && e.PreferServerFamily {
		return FamilyServer, "data-center GPU + profile prefers server family"
	}

	// 5. Open family explicitly preferred by profile.
	if e.PreferOpenFamily {
		return openVariant(e), "profile prefers open family"
	}

	// 6. Server family explicitly preferred by profile (non-data-center).
	if e.PreferServerFamily {
		return FamilyServer, "profile prefers server family"
	}

	// 7. Default: server-open. Open module works on every GPU
	//    NVIDIA supports today, server channel has the longest
	//    backport window.
	return FamilyServerOpen, "default: server-open is the safest modern choice"
}

// openVariant returns server-open or non-server-open based on
// data-center status and profile hints.
func openVariant(e Evidence) Family {
	if e.IsDataCenter || e.PreferServerFamily {
		return FamilyServerOpen
	}
	if e.PreferOpenFamily {
		// Profile hint alone leans server-open for the open family.
		return FamilyServerOpen
	}
	return FamilyServerOpen
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
		// Either nothing installed, or multiple families installed.
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
	// We don't want to pull in regexp for this; do it by hand.
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

// IsBlackwellConsumerPCIID reports true for known GeForce RTX 50-series
// PCI device IDs. We do not attempt to be exhaustive: only the SKUs
// CloudDeploy has actually validated against. New SKUs should be added
// here once seen.
func IsBlackwellConsumerPCIID(pciID string) bool {
	switch strings.ToLower(pciID) {
	case "10de:2b85", // GB202 RTX 5090
		"10de:2b87", // GB202 RTX 5090 D (China SKU)
		"10de:2c02": // GB203 RTX 5080
		return true
	}
	return false
}

// IsDataCenterPCIID reports true for known NVIDIA data-center PCI device
// IDs that CloudDeploy has validated (L4, etc.).
func IsDataCenterPCIID(pciID string) bool {
	switch strings.ToLower(pciID) {
	case "10de:27b8", // L4
		"10de:2235", // A10
		"10de:20b5": // A100 (80GB PCIe)
		return true
	}
	return false
}
