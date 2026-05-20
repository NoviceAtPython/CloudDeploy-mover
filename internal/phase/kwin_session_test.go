package phase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKWinSession_resolveDRMDevice(t *testing.T) {
	tempDir := t.TempDir()
	sysfsDir := filepath.Join(tempDir, "sys")
	devdriDir := filepath.Join(tempDir, "dev", "dri")
	os.MkdirAll(sysfsDir, 0755)
	os.MkdirAll(devdriDir, 0755)

	for _, d := range []string{"card0", "card1", "card2"} {
		os.MkdirAll(filepath.Join(sysfsDir, "class/drm", d, "device"), 0755)
	}

	// Test 1: Explicitly requested overrides everything.
	s := KWinSession{
		SysfsRoot:  sysfsDir,
		DevDriRoot: devdriDir,
	}
	res, reason := s.resolveDRMDevice("/dev/dri/card2", "")
	if res != "/dev/dri/card2" || !strings.Contains(reason, "explicitly") {
		t.Fatalf("Expected explicit request to win, got: %s %s", res, reason)
	}

	// Test 2: Forced connector matched on card0.
	os.MkdirAll(filepath.Join(sysfsDir, "class/drm/card0/card0-DP-1"), 0755)
	res, reason = s.resolveDRMDevice("", "DP-1")
	if res != filepath.Join(devdriDir, "card0") || !strings.Contains(reason, "matched") {
		t.Fatalf("Expected DP-1 match on card0, got: %s %s", res, reason)
	}
	os.RemoveAll(filepath.Join(sysfsDir, "class/drm/card0/card0-DP-1"))

	// Test 3: NVIDIA vendor match.
	os.WriteFile(filepath.Join(sysfsDir, "class/drm/card1/device/vendor"), []byte("0x10de\n"), 0644)
	res, reason = s.resolveDRMDevice("", "")
	if res != filepath.Join(devdriDir, "card1") || !strings.Contains(reason, "nvidia") {
		t.Fatalf("Expected NVIDIA match on card1, got: %s %s", res, reason)
	}

	// Test 4: Fallback to card1.
	os.RemoveAll(filepath.Join(sysfsDir, "class/drm/card1/device/vendor"))
	os.WriteFile(filepath.Join(devdriDir, "card1"), []byte("mock"), 0644)
	res, reason = s.resolveDRMDevice("", "")
	if res != filepath.Join(devdriDir, "card1") || !strings.Contains(reason, "fallback card1 present") {
		t.Fatalf("Expected fallback card1, got: %s %s", res, reason)
	}

	// Test 5: Fallback to card0.
	os.RemoveAll(filepath.Join(devdriDir, "card1"))
	res, reason = s.resolveDRMDevice("", "")
	if res != filepath.Join(devdriDir, "card0") || !strings.Contains(reason, "fallback final") {
		t.Fatalf("Expected fallback final card0, got: %s %s", res, reason)
	}
}
