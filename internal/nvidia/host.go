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
// This is the live-host counterpart to the unit-tested decision logic
// in family.go. It is intentionally tolerant of missing tools
// (`lspci`, `dpkg`, `nvidia-smi`) so it can run on a dev box too -
// missing evidence becomes Evidence's zero value, not an error.
func GatherEvidenceFromHost() (Evidence, error) {
	ev := Evidence{}

	if lspci, err := exec.LookPath("lspci"); err == nil {
		out, err := exec.Command(lspci, "-nn").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				// We only care about NVIDIA GPU rows.
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
	if ev.PCIID != "" {
		ev.IsBlackwellConsumer = IsBlackwellConsumerPCIID(ev.PCIID)
		ev.IsDataCenter = IsDataCenterPCIID(ev.PCIID)
	}

	// dmesg evidence. dmesg may be root-only; tolerate failure.
	if dmesg, err := exec.LookPath("dmesg"); err == nil {
		out, err := exec.Command(dmesg, "--ctime", "-l", "warn,err,crit,alert,emerg").Output()
		if err == nil {
			ev.DmesgRequiresOpenKernelModule = bytesContainOpenKernelModuleNotice(out)
		}
	}
	if !ev.DmesgRequiresOpenKernelModule {
		// Fallback: /var/log/kern.log may be readable to non-root.
		if f, err := os.Open("/var/log/kern.log"); err == nil {
			ev.DmesgRequiresOpenKernelModule = scanReaderForOpenKernelModuleNotice(f)
			_ = f.Close()
		}
	}

	// dpkg state. Missing dpkg -> all installed flags stay false,
	// which is correct on a non-Debian dev box.
	if dpkg, err := exec.LookPath("dpkg-query"); err == nil {
		query := func(pkg string) bool {
			out, err := exec.Command(dpkg, "-W", "-f=${db:Status-Abbrev}\n", pkg).Output()
			if err != nil {
				return false
			}
			return strings.HasPrefix(strings.TrimSpace(string(out)), "ii ") || strings.TrimSpace(string(out)) == "ii"
		}
		// Try every major version we currently care about. This is a
		// short list and rarely changes; expanding it is one PR away.
		for _, major := range []string{"580", "570", "560", "550", "535"} {
			if query("nvidia-driver-" + major + "-server") {
				ev.InstalledServer = true
			}
			if query("nvidia-driver-" + major + "-server-open") {
				ev.InstalledServerOpen = true
			}
			if query("nvidia-driver-" + major) {
				ev.InstalledNonServer = true
			}
			if query("nvidia-driver-" + major + "-open") {
				ev.InstalledNonServerOpen = true
			}
		}
	}

	if smi, err := exec.LookPath("nvidia-smi"); err == nil {
		if err := exec.Command(smi, "-L").Run(); err == nil {
			ev.NvidiaSmiWorks = true
		}
	}

	// At this point we've gathered what we can. Return the
	// half-filled Evidence; an empty/zero record is also valid (means
	// "no NVIDIA GPU detected here").
	if ev.PCIID == "" && !ev.NvidiaSmiWorks && !ev.InstalledServer && !ev.InstalledServerOpen && !ev.InstalledNonServer && !ev.InstalledNonServerOpen {
		return ev, errors.New("nvidia: no NVIDIA GPU or driver evidence found on this host")
	}
	return ev, nil
}

func extractGPUName(lspciLine string) string {
	// Format: "01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GB202 [GeForce RTX 5090] [10de:2b85] (rev a1)"
	// We want "NVIDIA GeForce RTX 5090" if possible, otherwise fall
	// back to the substring after "NVIDIA Corporation".
	const marker = "NVIDIA Corporation "
	idx := strings.Index(lspciLine, marker)
	if idx < 0 {
		return ""
	}
	tail := strings.TrimSpace(lspciLine[idx+len(marker):])
	// Pull the [bracketed marketing name] if present.
	if open := strings.IndexByte(tail, '['); open >= 0 {
		if close := strings.IndexByte(tail[open:], ']'); close > 0 {
			inner := tail[open+1 : open+close]
			if !strings.Contains(inner, ":") { // not a PCI ID bracket
				return "NVIDIA " + inner
			}
		}
	}
	// Strip trailing " [pciid] (rev ...)".
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
	// Some kern.log lines can be long.
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
