// Package config loads CloudDeploy YAML profiles and GPU overlays
// into a single typed Effective config.
//
// Layering (precedence: later overrides earlier):
//  1. config/profiles/<profile>.yaml
//  2. config/gpus/<gpu>.yaml (matched against detected GPU)
//  3. runtime overrides from `clouddeployctl --set foo=bar` (TBD)
//
// This file currently implements (1) only; the GPU overlay merge
// lands in Milestone 2.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Profile is the high-level operator intent loaded from
// config/profiles/<name>.yaml.
type Profile struct {
	Profile       string         `yaml:"profile"`
	UbuntuVersion string         `yaml:"ubuntu_version"`
	Display       DisplayConfig  `yaml:"display"`
	NVIDIA        NVIDIAConfig   `yaml:"nvidia"`
	CUDA          CUDAConfig     `yaml:"cuda"`
	Sunshine      SunshineConfig `yaml:"sunshine"`
	KWin          KWinConfig     `yaml:"kwin"`
}

// DisplayConfig is the target output mode + HDR flag.
type DisplayConfig struct {
	Resolution      string `yaml:"resolution"`       // "3840x2160"
	Refresh         int    `yaml:"refresh"`          // 120
	HDR             bool   `yaml:"hdr"`              // true for HDR profiles
	ForcedConnector string `yaml:"forced_connector"` // "DP-1"
}

// NVIDIAConfig governs driver selection.
type NVIDIAConfig struct {
	DriverMajor      string `yaml:"driver_major"`        // "580"
	PreferOpenFamily bool   `yaml:"prefer_open_family"`  // true for Blackwell
	PreferServer     bool   `yaml:"prefer_server"`       // true for data-center
}

// CUDAConfig governs CUDA installation policy.
type CUDAConfig struct {
	Mode string `yaml:"mode"` // "none" | "optional" | "required"
}

// SunshineConfig captures the Sunshine fork pin + HDR knobs.
type SunshineConfig struct {
	Source                   string `yaml:"source"`                    // "fork" or "deb"
	ForkRepo                 string `yaml:"fork_repo"`
	ForkBranch               string `yaml:"fork_branch"`
	ForkCommit               string `yaml:"fork_commit"`
	Encoder                  string `yaml:"encoder"`                   // "nvenc"
	Capture                  string `yaml:"capture"`                   // "kms"
	ForceAV1HDR10            bool   `yaml:"force_av1_hdr10"`
	SynthesizeHDR10Metadata  bool   `yaml:"synthesize_hdr10_metadata"`
}

// KWinConfig governs patched-KWin install + private HDR.
type KWinConfig struct {
	PatchedHDR bool   `yaml:"patched_hdr"`
	Patch      string `yaml:"patch"` // relative path under repo
}

// LoadProfile reads a profile YAML by name from <dir>/profiles/<name>.yaml.
func LoadProfile(dir, name string) (*Profile, error) {
	path := filepath.Join(dir, "profiles", name+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if p.Profile == "" {
		p.Profile = name
	}
	return &p, nil
}
