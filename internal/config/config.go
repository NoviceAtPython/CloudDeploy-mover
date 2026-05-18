// Package config loads CloudDeploy YAML profiles and GPU overlays
// into a single typed Effective config.
//
// Layering (precedence: later overrides earlier):
//  1. config/profiles/<profile>.yaml
//  2. config/gpus/<gpu>.yaml (matched against detected GPU)
//  3. runtime overrides from `clouddeployctl --set foo=bar` (TBD)
//
// This file currently implements (1) + (2) loading and schema
// validation. The runtime-merge logic into a single Effective struct
// lands in Milestone 2 once internal/gpu detection is real.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Profile (config/profiles/<name>.yaml)
// -----------------------------------------------------------------------------

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
	Deploy        DeployConfig   `yaml:"deploy"`
}

// DeployConfig is operator-knob territory: how the deploy itself
// behaves, independent of the streaming target.
type DeployConfig struct {
	// AutoReboot, when true, lets clouddeployctl apply call
	// `systemctl reboot` automatically after installing the
	// continuation service. Off by default - the operator opts in
	// because rebooting a cloud VM is a destructive action.
	AutoReboot bool `yaml:"auto_reboot"`

	// AutoUpgradeUbuntu, when true, lets the ubuntu-upgrade phase
	// perform an Ubuntu release upgrade to reach
	// `profile.ubuntu_version`. Off by default; an operator opts in
	// because dist-upgrade of a running VM is destructive.
	AutoUpgradeUbuntu bool `yaml:"auto_upgrade_ubuntu"`

	// AcceptNonLTS unlocks non-LTS target releases (anything ending
	// in `.10`: 24.10, 25.10, 26.10). Off by default; v3 refuses
	// non-LTS hops unless the operator explicitly accepts them.
	AcceptNonLTS bool `yaml:"accept_non_lts"`

	// DirectAptCodenameUpgrade controls whether the ubuntu-upgrade
	// phase uses the direct apt-source codename rewrite (the only
	// path that works for 24.04 LTS -> 25.10 questing, since
	// `do-release-upgrade -d` refuses that hop).
	//
	//   "auto"  - default. Use direct rewrite for known-failing hops
	//             (currently 24.04 -> 25.10), do-release-upgrade
	//             otherwise.
	//   "force" - always use direct rewrite.
	//   "off"   - never use direct rewrite; rely on
	//             do-release-upgrade.
	DirectAptCodenameUpgrade string `yaml:"direct_apt_codename_upgrade"`
}

// DisplayConfig is the target output mode + HDR flag.
type DisplayConfig struct {
	Resolution      string `yaml:"resolution"`
	Refresh         int    `yaml:"refresh"`
	HDR             bool   `yaml:"hdr"`
	ForcedConnector string `yaml:"forced_connector"`
}

// NVIDIAConfig governs driver selection.
type NVIDIAConfig struct {
	DriverMajor      string `yaml:"driver_major"`
	PreferOpenFamily bool   `yaml:"prefer_open_family"`
	PreferServer     bool   `yaml:"prefer_server"`
}

// CUDAConfig governs CUDA installation policy.
type CUDAConfig struct {
	// Mode: "none" | "optional" | "required".
	Mode string `yaml:"mode"`

	// PackageName: explicit apt package; empty = auto-discover via
	// internal/cuda.DiscoverCandidate.
	PackageName string `yaml:"package_name"`

	// Method controls *how* the toolkit is installed:
	//   "auto"     - default. Prefer apt (NVIDIA CUDA apt repo) when
	//                an official repo is available for the host's
	//                Ubuntu version; fall back to runfile otherwise.
	//   "apt"      - force apt. Fails (mode-aware) if no official
	//                repo is detected for this Ubuntu version.
	//   "runfile"  - force the toolkit-only NVIDIA runfile. Never
	//                installs `cuda-drivers` / driver meta-packages.
	//   "none"     - disable install entirely (equivalent to mode=none
	//                for the install side, but state still reflects
	//                the configured mode for diagnostics).
	Method string `yaml:"method"`

	// RunfileURL pins the toolkit-only NVIDIA runfile. Used by the
	// runfile install method. Example:
	//   https://developer.download.nvidia.com/compute/cuda/13.0.2/local_installers/cuda_13.0.2_580.95.05_linux.run
	RunfileURL string `yaml:"runfile_url"`

	// RunfileSHA256 is the SHA-256 the install path will log and
	// match against any prior `--check`-failing attempt. Optional but
	// strongly recommended; with it set, repeated downloads of an
	// upstream-corrupted artifact are caught after one attempt.
	RunfileSHA256 string `yaml:"runfile_sha256"`

	// RunfileMaxAttempts caps download+check retry attempts for the
	// runfile path. Zero falls back to the
	// `CLOUDDEPLOY_CUDA_RUNFILE_MAX_ATTEMPTS` env var, then 3.
	RunfileMaxAttempts int `yaml:"runfile_max_attempts"`

	// ExpectedMajor is the strict "this CUDA major must be installed
	// after the phase succeeds" pin. Used only when SelectionPolicy
	// is "exact-major". Empty otherwise.
	ExpectedMajor string `yaml:"expected_major"`

	// MinMajor is the lower-bound major when SelectionPolicy is
	// "min-major". Example: "12" means CUDA 12.x and CUDA 13.x both
	// satisfy. Empty otherwise.
	MinMajor string `yaml:"min_major"`

	// SelectionPolicy governs how the candidate ladder picks among
	// available CUDA toolkit packages and how post-install
	// verification gates the deploy:
	//
	//   ""                 - alias for "latest-compatible".
	//   "latest-compatible" - default. Prefer the newest official
	//                         NVIDIA toolkit major compatible with
	//                         the installed driver; fall through to
	//                         older majors / metapackage / archive
	//                         (if allowed).
	//   "exact-major"      - require ExpectedMajor exactly. Apt
	//                         ladder is filtered to that major only;
	//                         post-install nvcc must report that
	//                         major or the phase fails.
	//   "min-major"        - accept any toolkit major >= MinMajor.
	//                         Apt ladder includes all majors >=
	//                         MinMajor (descending preference).
	//   "any"              - accept any installable toolkit and any
	//                         nvcc that parses. Only useful for
	//                         broad-compatibility deploys.
	SelectionPolicy string `yaml:"selection_policy"`

	// AllowUbuntuArchiveFallback governs whether Ubuntu's
	// `nvidia-cuda-toolkit` package (CUDA 12.x as of 2026-05) is
	// allowed in the candidate ladder. Off by default for the strict
	// HDR/CUDA profile; on for the compatibility profile.
	AllowUbuntuArchiveFallback bool `yaml:"allow_ubuntu_archive_fallback"`

	// CompileSmokeTest, when true, makes the phase write a trivial
	// .cu file under /var/tmp/clouddeploy-cuda-smoke and compile it
	// with the freshly-installed nvcc before MarkDone. Required mode
	// fails the deploy on compile failure; optional mode records a
	// nonfatal skip. Running the compiled binary is best-effort
	// (some headless cloud images have no usable CUDA device until
	// the next reboot loads the driver fully) and never gates the
	// phase status today.
	CompileSmokeTest bool `yaml:"compile_smoke_test"`
}

// SunshineConfig captures the Sunshine fork pin + HDR knobs.
type SunshineConfig struct {
	Source                  string `yaml:"source"`
	ForkRepo                string `yaml:"fork_repo"`
	ForkBranch              string `yaml:"fork_branch"`
	ForkCommit              string `yaml:"fork_commit"`
	Encoder                 string `yaml:"encoder"`
	Capture                 string `yaml:"capture"`
	ForceAV1HDR10           bool   `yaml:"force_av1_hdr10"`
	SynthesizeHDR10Metadata bool   `yaml:"synthesize_hdr10_metadata"`
}

// KWinConfig governs patched-KWin install + private HDR.
type KWinConfig struct {
	PatchedHDR bool   `yaml:"patched_hdr"`
	Patch      string `yaml:"patch"`
}

// LoadProfile reads a single profile YAML by name from
// <dir>/profiles/<name>.yaml.
func LoadProfile(dir, name string) (*Profile, error) {
	path := filepath.Join(dir, "profiles", name+".yaml")
	return loadProfileFile(path, name)
}

func loadProfileFile(path, fallbackName string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if p.Profile == "" {
		p.Profile = fallbackName
	}
	return &p, nil
}

// LoadAllProfiles loads every <dir>/profiles/*.yaml. Returned slice
// is sorted by profile name. Errors include the offending filename.
func LoadAllProfiles(dir string) ([]*Profile, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "profiles"))
	if err != nil {
		return nil, fmt.Errorf("config: readdir %s/profiles: %w", dir, err)
	}
	var out []*Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		p, err := loadProfileFile(filepath.Join(dir, "profiles", e.Name()), name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out, nil
}

// ValidateProfile sanity-checks a profile's fields and returns the
// first violation. Used by tests and by the apply command's pre-flight.
func ValidateProfile(p *Profile) error {
	if p == nil {
		return fmt.Errorf("config: profile is nil")
	}
	if strings.TrimSpace(p.Profile) == "" {
		return fmt.Errorf("config: profile name is empty")
	}
	if strings.TrimSpace(p.NVIDIA.DriverMajor) == "" {
		return fmt.Errorf("config: profile %q: nvidia.driver_major is empty", p.Profile)
	}
	switch strings.ToLower(strings.TrimSpace(p.CUDA.Mode)) {
	case "none", "optional", "required":
		// ok
	default:
		return fmt.Errorf("config: profile %q: cuda.mode must be one of none/optional/required, got %q", p.Profile, p.CUDA.Mode)
	}
	switch strings.ToLower(strings.TrimSpace(p.CUDA.Method)) {
	case "", "auto", "apt", "runfile", "none":
		// ok
	default:
		return fmt.Errorf("config: profile %q: cuda.method must be one of auto/apt/runfile/none, got %q", p.Profile, p.CUDA.Method)
	}
	selPolicy := strings.ToLower(strings.TrimSpace(p.CUDA.SelectionPolicy))
	switch selPolicy {
	case "", "latest-compatible", "exact-major", "min-major", "any":
		// ok
	default:
		return fmt.Errorf("config: profile %q: cuda.selection_policy must be one of latest-compatible/exact-major/min-major/any, got %q", p.Profile, p.CUDA.SelectionPolicy)
	}
	if !isNumericMajorOrEmpty(p.CUDA.ExpectedMajor) {
		return fmt.Errorf("config: profile %q: cuda.expected_major must be a positive integer string (e.g. \"13\"), got %q", p.Profile, p.CUDA.ExpectedMajor)
	}
	if !isNumericMajorOrEmpty(p.CUDA.MinMajor) {
		return fmt.Errorf("config: profile %q: cuda.min_major must be a positive integer string (e.g. \"12\"), got %q", p.Profile, p.CUDA.MinMajor)
	}
	if selPolicy == "exact-major" && strings.TrimSpace(p.CUDA.ExpectedMajor) == "" {
		return fmt.Errorf("config: profile %q: cuda.selection_policy=exact-major requires cuda.expected_major to be set", p.Profile)
	}
	if selPolicy == "min-major" && strings.TrimSpace(p.CUDA.MinMajor) == "" {
		return fmt.Errorf("config: profile %q: cuda.selection_policy=min-major requires cuda.min_major to be set", p.Profile)
	}
	switch strings.ToLower(strings.TrimSpace(p.Sunshine.Source)) {
	case "fork", "deb":
		// ok
	default:
		return fmt.Errorf("config: profile %q: sunshine.source must be fork or deb, got %q", p.Profile, p.Sunshine.Source)
	}
	if p.Sunshine.Source == "fork" {
		if strings.TrimSpace(p.Sunshine.ForkRepo) == "" {
			return fmt.Errorf("config: profile %q: sunshine.fork_repo required for source=fork", p.Profile)
		}
		if strings.TrimSpace(p.Sunshine.ForkBranch) == "" {
			return fmt.Errorf("config: profile %q: sunshine.fork_branch required for source=fork", p.Profile)
		}
	}
	if p.Display.HDR {
		if p.Sunshine.ForkCommit == "" {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.fork_commit pin", p.Profile)
		}
		if !p.Sunshine.ForceAV1HDR10 {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.force_av1_hdr10=true", p.Profile)
		}
		if !p.Sunshine.SynthesizeHDR10Metadata {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.synthesize_hdr10_metadata=true", p.Profile)
		}
		// KMS+NVENC HDR streaming does not require CUDA for the streaming
		// path; the toolkit is optional. We allow cuda.mode = none /
		// optional / required so an HDR profile can also test the CUDA
		// install end-to-end (e.g. hdr-4k120-cuda). No further HDR-CUDA
		// constraint here.
	}
	return nil
}

// -----------------------------------------------------------------------------
// GPU profile (config/gpus/<name>.yaml)
// -----------------------------------------------------------------------------

// GPUProfile is the per-GPU overlay loaded from config/gpus/<name>.yaml.
// It carries NVIDIA family hints and streaming-capability advertisement.
type GPUProfile struct {
	Match     GPUMatch         `yaml:"match"`
	NVIDIA    GPUNVIDIAConfig  `yaml:"nvidia"`
	Streaming GPUStreamingFlag `yaml:"streaming"`

	// Filename is populated by LoadAllGPUs; not present in the YAML.
	Filename string `yaml:"-"`
}

// GPUMatch is the hardware-matching criteria. A GPU matches if any of
// the listed PCI IDs match exactly OR any of the name patterns match
// (substring or regex, decided by the consumer).
type GPUMatch struct {
	PCIIDs       []string `yaml:"pci_ids"`
	NamePatterns []string `yaml:"name_patterns"`
}

// GPUNVIDIAConfig is the NVIDIA-side overlay.
type GPUNVIDIAConfig struct {
	PreferOpenFamily bool     `yaml:"prefer_open_family"`
	PreferServer     bool     `yaml:"prefer_server"`
	DriverMajorMin   string   `yaml:"driver_major_min"`
	Notes            []string `yaml:"notes"`
}

// GPUStreamingFlag advertises whether the GPU can do AV1 / HDR.
// The unmarshal-friendly representation is string ("yes" / "no" /
// "limited") to avoid bool vs trinary surprises.
type GPUStreamingFlag struct {
	AV1Encode string `yaml:"av1_encode"`
	HDR       string `yaml:"hdr"`
}

// LoadGPU reads one GPU overlay from <dir>/gpus/<name>.yaml.
func LoadGPU(dir, name string) (*GPUProfile, error) {
	path := filepath.Join(dir, "gpus", name+".yaml")
	return loadGPUFile(path)
}

func loadGPUFile(path string) (*GPUProfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var g GPUProfile
	if err := yaml.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	g.Filename = filepath.Base(path)
	return &g, nil
}

// LoadAllGPUs loads every <dir>/gpus/*.yaml, sorted by filename.
func LoadAllGPUs(dir string) ([]*GPUProfile, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "gpus"))
	if err != nil {
		return nil, fmt.Errorf("config: readdir %s/gpus: %w", dir, err)
	}
	var out []*GPUProfile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		g, err := loadGPUFile(filepath.Join(dir, "gpus", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

// ValidateGPU sanity-checks a GPU overlay.
func ValidateGPU(g *GPUProfile) error {
	if g == nil {
		return fmt.Errorf("config: gpu profile is nil")
	}
	if len(g.Match.PCIIDs) == 0 && len(g.Match.NamePatterns) == 0 {
		return fmt.Errorf("config: gpu profile %q: match must list at least one pci_id or name_pattern", g.Filename)
	}
	for _, id := range g.Match.PCIIDs {
		if !looksLikePCIID(id) {
			return fmt.Errorf("config: gpu profile %q: pci_id %q is not vendor:device hex", g.Filename, id)
		}
	}
	if g.Streaming.AV1Encode != "" && !isYesNoLimited(g.Streaming.AV1Encode) {
		return fmt.Errorf("config: gpu profile %q: streaming.av1_encode must be yes/no/limited, got %q", g.Filename, g.Streaming.AV1Encode)
	}
	if g.Streaming.HDR != "" && !isYesNoLimited(g.Streaming.HDR) {
		return fmt.Errorf("config: gpu profile %q: streaming.hdr must be yes/no/limited, got %q", g.Filename, g.Streaming.HDR)
	}
	return nil
}

// isNumericMajorOrEmpty accepts "" or a positive ASCII-digit major,
// e.g. "12", "13", "14". Rejects "12.4", "-3", "v13".
func isNumericMajorOrEmpty(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func looksLikePCIID(s string) bool {
	if len(s) != 9 || s[4] != ':' {
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

func isYesNoLimited(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "no", "limited", "true", "false":
		return true
	}
	return false
}
