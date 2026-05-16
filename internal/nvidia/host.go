package nvidia

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// GatherEvidenceFromHost shells out to read enough state from the
// running system to feed SelectFamily / ValidateInstalled.
//
// Tolerates missing tools (lspci, dpkg, nvidia-smi, apt-cache) so it
// can run on a developer box too. Missing evidence is Evidence's
// zero value, not an error.
func GatherEvidenceFromHost() (Evidence, error) {
	ev := Evidence{}

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

	// ---- dpkg state ----
	if dpkg, err := exec.LookPath("dpkg-query"); err == nil {
		isInstalled := func(pkg string) bool {
			out, err := exec.Command(dpkg, "-W", "-f=${db:Status-Abbrev}\n", pkg).Output()
			if err != nil {
				return false
			}
			return strings.HasPrefix(strings.TrimSpace(string(out)), "ii ") || strings.TrimSpace(string(out)) == "ii"
		}
		for _, major := range []string{"580", "570", "560", "550", "535"} {
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

	// ---- apt-cache availability ----
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
		// Same major-list scan as for dpkg. Any major being available
		// is enough to set the flag.
		for _, major := range []string{"580", "570", "560", "550", "535"} {
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
