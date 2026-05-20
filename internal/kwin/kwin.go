// Package kwin owns the patched-KWin NVIDIA private HDR marker and
// small helpers shared by the kwin_patch phase and doctor commands.
package kwin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultMarkerPath is the canonical record that a patched KWin was
// built/installed by CloudDeploy.
const DefaultMarkerPath = "/var/lib/clouddeploy/kwin-patch.json"

// Marker is intentionally plain JSON so an operator can inspect it
// with jq when debugging a VM.
type Marker struct {
	PatchPath          string    `json:"patch_path"`
	PatchSHA256        string    `json:"patch_sha256"`
	KWinSourceVersion  string    `json:"kwin_source_version,omitempty"`
	BuildDir           string    `json:"build_dir"`
	InstalledPackages  []string  `json:"installed_packages,omitempty"`
	InstallMode        string    `json:"install_mode"`
	RuntimeBin         string    `json:"runtime_bin,omitempty"`
	Profile            string    `json:"profile,omitempty"`
	Timestamp          time.Time `json:"timestamp"`
	CloudDeployVersion string    `json:"clouddeploy_version,omitempty"`
	FallbackReason     string    `json:"fallback_reason,omitempty"`
}

// Matches reports whether this marker still satisfies the build
// inputs that matter for idempotency.
func (m Marker) Matches(patchSHA, sourceVersion, installMode, runtimeBin string) bool {
	if m.PatchSHA256 != patchSHA {
		return false
	}
	if sourceVersion != "" && m.KWinSourceVersion != sourceVersion {
		return false
	}
	if installMode != "" && m.InstallMode != installMode {
		return false
	}
	if runtimeBin != "" && m.RuntimeBin != runtimeBin {
		return false
	}
	return true
}

// HashFile returns the SHA-256 of path as lowercase hex.
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func ReadMarker(path string) (Marker, error) {
	if path == "" {
		path = DefaultMarkerPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, fmt.Errorf("kwin marker parse %s: %w", path, err)
	}
	return m, nil
}

func WriteMarker(path string, marker Marker) error {
	if path == "" {
		path = DefaultMarkerPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
