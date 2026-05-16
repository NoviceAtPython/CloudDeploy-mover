package nvidia

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// EvidenceOptions tunes GatherEvidenceFromHost.
//
// DriverMajor scopes the dpkg / apt-cache scan to one specific NVIDIA
// driver major (e.g. "580"). Defaults to "580" because that is what
// the hdr-4k120 profile pins. Set to "" to scan a small fallback set
// of recent majors (580, 570, 560, 550, 535); this is approximate
// and `doctor` should warn the operator that availability is a
// best-effort guess in that case.
//
// Future fields land here without breaking callers.
type EvidenceOptions struct {
	DriverMajor string
}

// defaultDriverMajor is what we scan when EvidenceOptions.DriverMajor
// is empty. Matches the hdr-4k120 profile's nvidia.driver_major.
const defaultDriverMajor = "580"

// fallbackDriverMajors is the approximate scan when no major is
// configured. Order: newest first.
var fallbackDriverMajors = []string{"580", "570", "560", "550", "535"}

// GatherEvidenceFromHost shells out to read enough state from the
// running system to feed SelectFamily / ValidateInstalled.
//
// Tolerates missing tools (lspci, dpkg, nvidia-smi, apt-cache) so it
// can run on a developer box too. Missing evidence is Evidence's
// zero value, not an error.
//
// The result has AvailabilityKnown=true iff apt-cache was available
// and the scan completed. Read-only doctor on a dev box without apt
// gets AvailabilityKnown=false so SelectFamily takes the optimistic
// path.
func GatherEvidenceFromHost(opts EvidenceOptions) (Evidence, error) {
	ev := Evidence{}

	majors := []string{strings.TrimSpace(opts.DriverMajor)}
	if majors[0] == "" {
		majors = fallbackDriverMajors
	}

	// ---- lspci → PCI ID + GPU name ----
	if lspci, err := exec.LookPath("lspci"); err == nil {
		out, err := exec.Command(lspci, "-nn").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, "NVIDIA Corporation") {
					continue
				}
				if !strings.Contains(line, "VGA") && !strings.Contains(line, "3D controller") && !strings.Contains(line, "Display controller") {
					continue
				}
				ev.PCIID = ParsePCIID(line)
				ev.GPUName = extractGPUName(line)
				break
			}
		}
	}

	// ---- Classify into Category / Kind + hard-evidence flags ----
	cls := Classify(ev.GPUName, ev.PCIID)
	if cls.HardEvidence.IsBlackwellConsumer {
		ev.IsBlackwellConsumer = true
	}
	if cls.HardEvidence.IsBlackwellPro {
		ev.IsBlackwellPro = true
	}
	if cls.HardEvidence.IsBlackwellDC {
		ev.IsBlackwellDC = true
	}
	if cls.HardEvidence.IsDataCenter {
		ev.IsDataCenter = true
	}
	if cls.HardEvidence.IsLegacyPascal {
		ev.IsLegacyPascal = true
	}

	// ---- dmesg / kern.log: open-kernel-module requirement ----
	if dmesg, err := exec.LookPath("dmesg"); err == nil {
		out, err := exec.Command(dmesg, "--ctime", "-l", "warn,err,crit,alert,emerg").Output()
		if err == nil {
			ev.DmesgRequiresOpenKernelModule = bytesContainOpenKernelModuleNotice(out)
		}
	}
	if !ev.DmesgRequiresOpenKernelModule {
		if f, err := os.Open("/var/log/kern.log"); err == nil {
			ev.DmesgRequiresOpenKernelModule = scanReaderForOpenKernelModuleNotice(f)
			_ = f.Close()
		}
	}

	// ---- dpkg state (scoped to majors) ----
	if dpkg, err := exec.LookPath("dpkg-query"); err == nil {
		isInstalled := func(pkg string) bool {
			out, err := exec.Command(dpkg, "-W", "-f=${db:Status-Abbrev}\n", pkg).Output()
			if err != nil {
				return false
			}
			return strings.HasPrefix(strings.TrimSpace(string(out)), "ii ") || strings.TrimSpace(string(out)) == "ii"
		}
		for _, major := range majors {
			if isInstalled("nvidia-driver-" + major + "-server") {
				ev.InstalledServer = true
			}
			if isInstalled("nvidia-driver-" + major + "-server-open") {
				ev.InstalledServerOpen = true
			}
			if isInstalled("nvidia-driver-" + major) {
				ev.InstalledNonServer = true
			}
			if isInstalled("nvidia-driver-" + major + "-open") {
				ev.InstalledNonServerOpen = true
			}
		}
	}

	// ---- apt-cache availability (scoped to majors) ----
	if aptCache, err := exec.LookPath("apt-cache"); err == nil {
		hasCandidate := func(pkg string) bool {
			out, err := exec.Command(aptCache, "policy", pkg).Output()
			if err != nil {
				return false
			}
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Candidate:") {
					cand := strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
					return cand != "" && cand != "(none)"
				}
			}
			return false
		}
		// apt-cache scan completed (even if everything is unavailable).
		ev.AvailabilityKnown = true
		for _, major := range majors {
			if hasCandidate("nvidia-driver-" + major + "-server") {
				ev.AvailableServer = true
			}
			if hasCandidate("nvidia-driver-" + major + "-server-open") {
				ev.AvailableServerOpen = true
			}
			if hasCandidate("nvidia-driver-" + major) {
				ev.AvailableNonServer = true
			}
			if hasCandidate("nvidia-driver-" + major + "-open") {
				ev.AvailableNonServerOpen = true
			}
		}
	}

	// ---- nvidia-smi smoke test ----
	if smi, err := exec.LookPath("nvidia-smi"); err == nil {
		if err := exec.Command(smi, "-L").Run(); err == nil {
			ev.NvidiaSmiWorks = true
		}
	}

	if ev.PCIID == "" && !ev.NvidiaSmiWorks && !anyInstalled(ev) {
		return ev, errors.New("nvidia: no NVIDIA GPU or driver evidence found on this host")
	}
	return ev, nil
}

// ScannedMajors reports which driver majors GatherEvidenceFromHost
// would scan for the given EvidenceOptions. Used by `doctor` to print
// "availability is approximate" when the operator did not pin a major.
func ScannedMajors(opts EvidenceOptions) []string {
	if m := strings.TrimSpace(opts.DriverMajor); m != "" {
		return []string{m}
	}
	out := make([]string, len(fallbackDriverMajors))
	copy(out, fallbackDriverMajors)
	return out
}

func extractGPUName(lspciLine string) string {
	const marker = "NVIDIA Corporation "
	idx := strings.Index(lspciLine, marker)
	if idx < 0 {
		return ""
	}
	tail := strings.TrimSpace(lspciLine[idx+len(marker):])
	if open := strings.IndexByte(tail, '['); open >= 0 {
		if close := strings.IndexByte(tail[open:], ']'); close > 0 {
			inner := tail[open+1 : open+close]
			if !strings.Contains(inner, ":") {
				return "NVIDIA " + inner
			}
		}
	}
	cut := strings.IndexByte(tail, '[')
	if cut > 0 {
		tail = strings.TrimSpace(tail[:cut])
	}
	return "NVIDIA " + tail
}

func bytesContainOpenKernelModuleNotice(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if lineMatchesOpenKernelModuleNotice(line) {
			return true
		}
	}
	return false
}

func scanReaderForOpenKernelModuleNotice(f *os.File) bool {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if lineMatchesOpenKernelModuleNotice(scanner.Text()) {
			return true
		}
	}
	return false
}

func lineMatchesOpenKernelModuleNotice(line string) bool {
	if !strings.Contains(line, "NVRM") {
		return false
	}
	lower := strings.ToLower(line)
	return strings.Contains(lower, "requires use of nvidia open kernel modules") ||
		strings.Contains(lower, "requires use of the nvidia open kernel modules")
}

// ---- Compatibility wrappers ----
//
// IsBlackwellConsumerPCIID / IsDataCenterPCIID predate the Classify
// API. They remain as thin wrappers so external tests and callers
// keep compiling while we move the rest of the code over.

// IsBlackwellConsumerPCIID reports true for known GeForce RTX 50-series
// PCI device IDs.
func IsBlackwellConsumerPCIID(pciID string) bool {
	c, ok := classifyByPCIID(pciID)
	return ok && c.HardEvidence.IsBlackwellConsumer
}

// IsDataCenterPCIID reports true for known NVIDIA data-center PCI
// device IDs.
func IsDataCenterPCIID(pciID string) bool {
	c, ok := classifyByPCIID(pciID)
	return ok && c.HardEvidence.IsDataCenter
}
