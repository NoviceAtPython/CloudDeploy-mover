package phase

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const defaultSysClassDRM = "/sys/class/drm"

var drmCardNameRE = regexp.MustCompile(`^card[0-9]+$`)
var drmRenderNameRE = regexp.MustCompile(`^renderD[0-9]+$`)

// RenderNodeForCard resolves the render node that belongs to a DRM primary
// node by matching sysfs PCI identity. Card numbers and render-node numbers are
// not arithmetic siblings; live RTX 3090 evidence had card1 -> renderD128.
func RenderNodeForCard(card string) string {
	return RenderNodeForCardWithSysfs(card, defaultSysClassDRM)
}

func RenderNodeForCardWithSysfs(card, sysClassDRM string) string {
	card = strings.TrimSpace(card)
	if card == "" {
		return ""
	}
	base := filepath.Base(card)
	if !drmCardNameRE.MatchString(base) {
		return ""
	}
	if strings.TrimSpace(sysClassDRM) == "" {
		sysClassDRM = defaultSysClassDRM
	}
	cardDevice, err := resolveDRMDeviceIdentity(filepath.Join(sysClassDRM, base, "device"))
	if err != nil || cardDevice == "" {
		return ""
	}
	entries, err := os.ReadDir(sysClassDRM)
	if err != nil {
		return ""
	}
	var renders []string
	for _, e := range entries {
		name := e.Name()
		if drmRenderNameRE.MatchString(name) {
			renders = append(renders, name)
		}
	}
	sort.Strings(renders)
	for _, render := range renders {
		renderDevice, err := resolveDRMDeviceIdentity(filepath.Join(sysClassDRM, render, "device"))
		if err != nil || renderDevice == "" {
			continue
		}
		if sameDRMDeviceIdentity(cardDevice, renderDevice) {
			return "/dev/dri/" + render
		}
	}
	return ""
}

func resolveDRMDeviceIdentity(path string) (string, error) {
	if st, statErr := os.Lstat(path); statErr == nil && st.Mode().IsRegular() {
		// Unit tests model sysfs with ordinary files containing the PCI identity
		// target. Production sysfs normally exposes device as a symlink.
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", fmt.Errorf("resolve %s: read=%v", path, readErr)
		}
		return strings.TrimSpace(string(b)), nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", fmt.Errorf("resolve %s: eval=%v read=%v", path, err, readErr)
	}
	return strings.TrimSpace(string(b)), nil
}

func sameDRMDeviceIdentity(a, b string) bool {
	a = strings.TrimSpace(filepath.ToSlash(filepath.Clean(a)))
	b = strings.TrimSpace(filepath.ToSlash(filepath.Clean(b)))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// Compare the PCI address tail as a fallback. The card and render sysfs
	// symlinks may resolve through different absolute roots in tests/chroots,
	// but the final PCI identifier is the stable identity.
	return filepath.Base(a) == filepath.Base(b)
}
