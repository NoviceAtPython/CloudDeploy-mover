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

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/edid"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Profile (config/profiles/<name>.yaml)
// -----------------------------------------------------------------------------

// Profile is the high-level operator intent loaded from
// config/profiles/<name>.yaml.
type Profile struct {
	Profile       string          `yaml:"profile"`
	UbuntuVersion string          `yaml:"ubuntu_version"`
	Display       DisplayConfig   `yaml:"display"`
	NVIDIA        NVIDIAConfig    `yaml:"nvidia"`
	CUDA          CUDAConfig      `yaml:"cuda"`
	Sunshine      SunshineConfig  `yaml:"sunshine"`
	KWin          KWinConfig      `yaml:"kwin"`
	Desktop       DesktopConfig   `yaml:"desktop"`
	Tailscale     TailscaleConfig `yaml:"tailscale"`
	Audio         AudioConfig     `yaml:"audio"`
	Deploy        DeployConfig    `yaml:"deploy"`
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

	// UbuntuSelectionPolicy controls how the ubuntu-upgrade phase
	// picks an OS target from `UbuntuCandidates`. Mirrors the
	// CUDA selection-policy model:
	//
	//   ""                  - alias for "latest-compatible".
	//   "latest-compatible" - walk `UbuntuCandidates` newest-first;
	//                         pick the first one that is a v3-
	//                         supported version + passes gates.
	//   "exact"             - require a single candidate that matches
	//                         exactly. Equivalent to setting
	//                         Profile.UbuntuVersion and leaving
	//                         UbuntuCandidates empty.
	//   "min-version"       - filter out candidates below
	//                         UbuntuMinVersion (e.g. min "25.10"
	//                         excludes 24.04).
	//   "any"               - accept any known candidate (still
	//                         requires a known codename + non-LTS
	//                         gate).
	UbuntuSelectionPolicy string `yaml:"ubuntu_selection_policy"`

	// UbuntuCandidates is the ordered list of Ubuntu VERSION_IDs the
	// resolver tries newest-first. Empty defaults to "use
	// Profile.UbuntuVersion only" (the pre-resolver behavior).
	//
	// Example (hdr-4k120-auto):
	//   ubuntu_candidates: ["26.04", "25.10", "24.04"]
	UbuntuCandidates []string `yaml:"ubuntu_candidates"`

	// UbuntuMinVersion is the lower bound under
	// UbuntuSelectionPolicy="min-version". Empty otherwise.
	UbuntuMinVersion string `yaml:"ubuntu_min_version"`

	// PreferLTS is a soft preference recorded in state.Details for
	// audit. The resolver does NOT reorder the candidate list -- the
	// operator's list order is authoritative -- but doctor + state
	// show surface this so an operator can see what the profile
	// asked for.
	PreferLTS bool `yaml:"prefer_lts"`

	// Unattended lets apply/resume run the deploy as a true autopilot:
	// noninteractive package installs, automatic reboot scheduling,
	// and continuation-service resume without operator prompts.
	Unattended bool `yaml:"unattended"`

	// MaxAutoReboots caps unattended reboot loops. Zero means the
	// documented default (8).
	MaxAutoReboots int `yaml:"max_auto_reboots"`

	// OptionalApps enables the final nonfatal game/app install phase.
	OptionalApps bool `yaml:"optional_apps"`

	// UsePrebuilt makes the sunshine-build and kwin-patch phases install
	// prebuilt artifacts (the compiled Sunshine fork + patched KWin .debs)
	// from a GitHub release instead of compiling from source -- the single
	// biggest deploy-time saver. It only engages when a release bundle
	// matching the host's Ubuntu version + arch AND the profile's pinned
	// versions exists; otherwise (or on any download/install error) it
	// falls back to compiling. nil defaults to true.
	UsePrebuilt *bool `yaml:"use_prebuilt"`

	// PrebuiltRepo is the owner/name of the GitHub repo whose releases
	// host the prebuilt bundles. Empty defaults to DefaultPrebuiltRepo.
	PrebuiltRepo string `yaml:"prebuilt_repo"`
}

// DefaultPrebuiltRepo is the public repo whose releases host the prebuilt
// Sunshine + KWin bundles (see the prebuilt-<os>-<arch> release tags).
const DefaultPrebuiltRepo = "NoviceAtPython/CloudDeploy-mover"

// UsePrebuiltValue resolves DeployConfig.UsePrebuilt; nil -> true.
func (d DeployConfig) UsePrebuiltValue() bool {
	if d.UsePrebuilt == nil {
		return true
	}
	return *d.UsePrebuilt
}

// PrebuiltRepoValue resolves the prebuilt repo; empty -> DefaultPrebuiltRepo.
func (d DeployConfig) PrebuiltRepoValue() string {
	if strings.TrimSpace(d.PrebuiltRepo) == "" {
		return DefaultPrebuiltRepo
	}
	return d.PrebuiltRepo
}

// DisplayConfig is the target output mode + HDR flag.
type DisplayConfig struct {
	Resolution      string `yaml:"resolution"`
	Refresh         int    `yaml:"refresh"`
	HDR             bool   `yaml:"hdr"`
	ForcedConnector string `yaml:"forced_connector"`
	// AllowFallback permits drm_display_validate to accept a lower
	// display target only after the strict target has been tried and
	// rejected. Default false: HDR/4K120 profiles are strict unless
	// the operator opts into fallback.
	AllowFallback bool `yaml:"allow_fallback"`
	// AllowSDRFallback permits an HDR profile to fall back to an SDR
	// target if HDR enablement is rejected by KScreen/KWin/NVIDIA.
	AllowSDRFallback bool `yaml:"allow_sdr_fallback"`
	// AllowLowerRefreshFallback permits refresh fallback, e.g.
	// 3840x2160@120 -> 3840x2160@60. Default false.
	AllowLowerRefreshFallback bool `yaml:"allow_lower_refresh_fallback"`
	// Edid is the filename (under /lib/firmware/edid/) that the edid
	// phase writes and that drm.edid_firmware= on the kernel cmdline
	// references. drm_display_validate uses it to confirm the
	// installed edid blob matches the operator's intent. Empty
	// means "no specific edid expected" - the validator only checks
	// connector / mode.
	Edid string `yaml:"edid"`
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
	// with the freshly-installed nvcc before MarkDone, then run it
	// under a short timeout. Required mode fails the deploy on
	// compile/run timeout/failure; optional mode records a degraded
	// terminal skip and continues because NVENC/Sunshine validation is
	// independent of the CUDA toolkit smoke binary.
	CompileSmokeTest bool `yaml:"compile_smoke_test"`

	// PreferMajor is a *soft* preference. Under latest-compatible /
	// min-major / any policies the candidate ladder is reordered so
	// PreferMajor's packages come first. Under exact-major the
	// preference is ignored (ExpectedMajor is authoritative).
	// Operationally: v2 demonstrated CUDA 13 works on Ubuntu 25.10 via
	// NVIDIA's `ubuntu2404` repo, so the compat profile sets
	// prefer_major="13" to keep that newest-known-good selection even
	// when an older-major archive package is also available.
	PreferMajor string `yaml:"prefer_major"`

	// PreferNewest documents intent under latest-compatible (which
	// already picks newest). Recorded in state.Details so operators
	// reading `state show` can see what the profile asked for; the
	// candidate ladder is unchanged.
	PreferNewest bool `yaml:"prefer_newest"`

	// AllowCrossDistroCudaRepo unlocks the "try ubuntu2404 from a
	// 25.10 host" trick v2 uses. When false (default), only the
	// host-native NVIDIA CUDA apt repo (or `auto-host` resolved to
	// the host slug) is considered. When true, the
	// CudaRepoDistroCandidates list is walked in order.
	AllowCrossDistroCudaRepo bool `yaml:"allow_cross_distro_cuda_repo"`

	// CudaRepoDistroCandidates is the ordered list of NVIDIA CUDA
	// apt-repo distro slugs the phase probes. The literal
	// "auto-host" expands to the host's slug at probe time. Empty
	// defaults to ["auto-host"], which preserves single-distro
	// behavior. Non-auto-host entries are dropped when
	// AllowCrossDistroCudaRepo=false.
	//
	// Example (broad compat / strict modern):
	//   cuda_repo_distro_candidates:
	//     - auto-host    # ubuntu2510 on 25.10, ubuntu2404 on 24.04, ...
	//     - ubuntu2404   # cross-distro fallback when the host slug 404s
	//
	// Example (native-only diagnostic profile):
	//   cuda_repo_distro_candidates:
	//     - auto-host
	CudaRepoDistroCandidates []string `yaml:"cuda_repo_distro_candidates"`
}

// SunshineConfig captures the Sunshine fork pin + HDR knobs.
//
// Codec advertisement (hevc_mode + av1_mode) is the load-bearing
// piece for HDR-Main10 streaming. Sunshine's docs / source:
//
//	hevc_mode = 0    auto-advertise based on encoder probe (UNSAFE
//	                 for HDR: the live-VM probe left active_hevc_mode
//	                 below 3, so Moonlight never saw HEVC Main10 and
//	                 fell back to H.264 with HDR/10-bit still forced
//	                 -> h264_nvenc rejected the negotiation).
//	hevc_mode = 1    HEVC disabled.
//	hevc_mode = 2    HEVC Main only (8-bit).
//	hevc_mode = 3    HEVC Main + Main10 (HDR-capable).
//
//	av1_mode = 0     auto-advertise based on encoder probe.
//	av1_mode = 1     AV1 disabled.
//	av1_mode = 2     AV1 Main only (8-bit).
//	av1_mode = 3     AV1 Main + Main10 (HDR-capable).
//
// HDR profiles MUST pin hevc_mode=3 (HEVC Main10 fallback). AV1 remains
// preferred when available, and profiles may keep av1_mode=3 so AV1-capable
// GPUs advertise Main10 too, but Ampere/A5000/A6000 deployments must not
// fail solely because NVENC AV1 is unavailable.
type SunshineConfig struct {
	Source                  string   `yaml:"source"`
	ForkRepo                string   `yaml:"fork_repo"`
	ForkBranch              string   `yaml:"fork_branch"`
	ForkCommit              string   `yaml:"fork_commit"`
	Encoder                 string   `yaml:"encoder"`
	Capture                 string   `yaml:"capture"`
	Gamepad                 string   `yaml:"gamepad"`
	MotionAsDS4             bool     `yaml:"motion_as_ds4"`
	TouchpadAsDS4           bool     `yaml:"touchpad_as_ds4"`
	DS4BackAsTouchpadClick  bool     `yaml:"ds4_back_as_touchpad_click"`
	DisableLegacyJoydev     *bool    `yaml:"disable_legacy_joydev"`
	HevcMode                *int     `yaml:"hevc_mode"`
	Av1Mode                 *int     `yaml:"av1_mode"`
	ForceAV1HDR10           bool     `yaml:"force_av1_hdr10"`
	SynthesizeHDR10Metadata bool     `yaml:"synthesize_hdr10_metadata"`
	BuildDir                string   `yaml:"build_dir"`
	InstallBin              string   `yaml:"install_bin"`
	ConfigPath              string   `yaml:"config_path"`
	BuildJobs               int      `yaml:"build_jobs"`
	RequireCUDA             bool     `yaml:"require_cuda"`
	EnableCUDA              string   `yaml:"enable_cuda"`
	CUDAFailOnMissing       bool     `yaml:"cuda_fail_on_missing"`
	CUDARoot                string   `yaml:"cuda_root"`
	ExtraCSRFAllowedOrigins []string `yaml:"extra_csrf_allowed_origins"`
	DoxygenVersion          string   `yaml:"doxygen_version"`
	DoxygenURL              string   `yaml:"doxygen_url"`
	DoxygenSHA256           string   `yaml:"doxygen_sha256"`
	DoxygenInstallDir       string   `yaml:"doxygen_install_dir"`
}

// DefaultHevcMode + DefaultAv1Mode are the safe HDR-Main10 defaults
// applied when the profile omits the keys. v3's old "0" auto-probe
// default is the bug we're closing; an absent key now means "give
// us the modes that advertise HDR Main10 to Moonlight".
const (
	DefaultHevcMode = 3
	DefaultAv1Mode  = 3
)

// HevcModeValue / Av1ModeValue resolve the *int fields against the
// HDR-Main10 defaults. Profiles can still pin a different value
// (e.g. an 8-bit-only diagnostic profile) by setting hevc_mode: 2 /
// av1_mode: 2 explicitly.
func (s SunshineConfig) HevcModeValue() int {
	if s.HevcMode == nil {
		return DefaultHevcMode
	}
	return *s.HevcMode
}

func (s SunshineConfig) Av1ModeValue() int {
	if s.Av1Mode == nil {
		return DefaultAv1Mode
	}
	return *s.Av1Mode
}

// AdvertisesHEVCMain10 / AdvertisesAV1Main10 report whether the
// resolved mode bit is high enough for Sunshine to advertise the
// Main10 capability to Moonlight. Used by validation + doctor.
func (s SunshineConfig) AdvertisesHEVCMain10() bool { return s.HevcModeValue() >= 3 }
func (s SunshineConfig) AdvertisesAV1Main10() bool  { return s.Av1ModeValue() >= 3 }

const DefaultSunshineBuildDir = "/opt/sunshine-src"
const DefaultSunshineInstallBin = "/usr/local/bin/sunshine-clouddeploy"
const DefaultSunshineConfigName = "sunshine.conf"
const DefaultSunshineBuildJobs = 2
const DefaultSunshineEnableCUDA = "auto"

// DefaultSunshineGamepad deliberately uses Sunshine's client-reported
// metadata path instead of forcing every controller to Xbox. The v3
// profiles keep motion_as_ds4/touchpad_as_ds4 disabled, so unknown
// third-party pads still fall back to Xbox instead of being guessed as
// PlayStation from noisy capability bits.
const DefaultSunshineGamepad = "auto"
const DefaultDoxygenVersion = "1.17.0"
const DefaultDoxygenURL = "https://www.doxygen.nl/files/doxygen-1.17.0.linux.bin.tar.gz"
const DefaultDoxygenSHA256 = "75419ef4f446fc1c24ef12514b574e66e898ee6f527c6ae2ad84f91a905823c2"
const DefaultDoxygenInstallDir = "/opt/doxygen-1.17.0"

func (p *Profile) EffectiveSunshine() SunshineConfig {
	out := SunshineConfig{}
	if p != nil {
		out = p.Sunshine
	}
	if strings.TrimSpace(out.Source) == "" {
		out.Source = "fork"
	}
	if strings.TrimSpace(out.Encoder) == "" {
		out.Encoder = "nvenc"
	}
	if strings.TrimSpace(out.Capture) == "" {
		out.Capture = "kms"
	}
	out.Gamepad = out.GamepadValue()
	if strings.TrimSpace(out.BuildDir) == "" {
		out.BuildDir = DefaultSunshineBuildDir
	}
	if strings.TrimSpace(out.InstallBin) == "" {
		out.InstallBin = DefaultSunshineInstallBin
	}
	if out.BuildJobs <= 0 {
		out.BuildJobs = DefaultSunshineBuildJobs
	}
	if strings.TrimSpace(out.EnableCUDA) == "" {
		out.EnableCUDA = DefaultSunshineEnableCUDA
	}
	// cuda.mode=required means the operator is paying for the toolkit
	// because capture needs it. Under enable_cuda="auto" a failed CUDA
	// configure silently reconfigures with SUNSHINE_ENABLE_CUDA=OFF and
	// the deploy reports success while Sunshine copies every frame
	// through system RAM (GPU -> RAM -> GPU): one core pinned, NVENC
	// idle, ~40fps at 4K120 HDR. That silent downgrade is the bug this
	// promotion closes - "required" now means the build fails loudly
	// instead of shipping a capture-crippled binary.
	//
	// An explicit sunshine.enable_cuda is always honored, so a profile
	// can still say "install the toolkit for other work, but do not
	// hard-fail the Sunshine build" via enable_cuda: "auto"/"false".
	if p.WantsCUDARequired() && strings.EqualFold(strings.TrimSpace(p.Sunshine.EnableCUDA), "") {
		out.EnableCUDA = "true"
	}
	if strings.TrimSpace(out.DoxygenVersion) == "" {
		out.DoxygenVersion = DefaultDoxygenVersion
	}
	if strings.TrimSpace(out.DoxygenURL) == "" {
		out.DoxygenURL = DefaultDoxygenURL
	}
	if strings.TrimSpace(out.DoxygenSHA256) == "" {
		out.DoxygenSHA256 = DefaultDoxygenSHA256
	}
	if strings.TrimSpace(out.DoxygenInstallDir) == "" {
		out.DoxygenInstallDir = DefaultDoxygenInstallDir
	}
	return out
}

func (s SunshineConfig) GamepadValue() string {
	mode := strings.ToLower(strings.TrimSpace(s.Gamepad))
	switch mode {
	case "":
		return DefaultSunshineGamepad
	case "xbox", "xinput", "x360":
		return "xone"
	case "ps", "playstation", "ps4", "ds4", "dualshock", "dualshock4", "ps5", "dualsense", "dualshock5":
		// Sunshine's Linux backend exposes Xbox One, DualSense, and
		// Switch virtual devices. Explicit PS aliases force the only
		// Linux PlayStation virtual backend; gamepad=auto remains the
		// client-reported recognition path for mixed PS/Xbox setups.
		return "ds5"
	}
	return mode
}

func (s SunshineConfig) DisableLegacyJoydevValue() bool {
	if s.DisableLegacyJoydev == nil {
		return true
	}
	return *s.DisableLegacyJoydev
}

// TailscaleConfig controls the optional network overlay phase.
type TailscaleConfig struct {
	Enabled    *bool  `yaml:"enabled"`
	AuthKeyEnv string `yaml:"authkey_env"`
	SSH        bool   `yaml:"ssh"`
}

func (p *Profile) EffectiveTailscale() TailscaleConfig {
	out := TailscaleConfig{}
	if p != nil {
		out = p.Tailscale
	}
	if out.Enabled == nil {
		v := true
		out.Enabled = &v
	}
	if strings.TrimSpace(out.AuthKeyEnv) == "" {
		out.AuthKeyEnv = "TAILSCALE_AUTHKEY"
	}
	out.SSH = true
	return out
}

func (t TailscaleConfig) EnabledValue() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// AudioConfig controls the PipeWire virtual audio substrate.
type AudioConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	VirtualSink string `yaml:"virtual_sink"`
	Rate        int    `yaml:"rate"`
	Channels    int    `yaml:"channels"`
	ChannelMap  string `yaml:"channel_map"`
}

func (p *Profile) EffectiveAudio() AudioConfig {
	out := AudioConfig{}
	if p != nil {
		out = p.Audio
	}
	if out.Enabled == nil {
		v := true
		out.Enabled = &v
	}
	if strings.TrimSpace(out.VirtualSink) == "" {
		out.VirtualSink = "clouddeploy-surround71"
	}
	if out.Rate <= 0 {
		out.Rate = 48000
	}
	if out.Channels <= 0 {
		out.Channels = 8
	}
	if strings.TrimSpace(out.ChannelMap) == "" {
		out.ChannelMap = "front-left,front-right,rear-left,rear-right,front-center,lfe,side-left,side-right"
	}
	return out
}

func (a AudioConfig) EnabledValue() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// KWinConfig governs patched-KWin install + private HDR.
type KWinConfig struct {
	PatchedHDR            bool   `yaml:"patched_hdr"`
	Patch                 string `yaml:"patch"`
	SourceMode            string `yaml:"source_mode"`
	BuildDir              string `yaml:"build_dir"`
	InstallMode           string `yaml:"install_mode"`
	RuntimeBin            string `yaml:"runtime_bin"`
	RequirePatch          *bool  `yaml:"require_patch"`
	AllowPackagedFallback bool   `yaml:"allow_packaged_fallback"`
	ValidatePatch         bool   `yaml:"validate_patch"`
	HoldPackages          bool   `yaml:"hold_packages"`
}

const DefaultKWinPatchPath = "patches/kwin-clouddeploy-nvidia-private-hdr.patch"
const DefaultKWinSourceMode = "auto"
const DefaultKWinBuildDir = "/opt/clouddeploy-kwin-src"
const DefaultKWinInstallMode = "packages"

// EffectiveKWin applies the documented KWin defaults. Missing
// require_patch follows patched_hdr; HDR profiles therefore fail
// closed unless they explicitly opt into packaged fallback.
func (p *Profile) EffectiveKWin() KWinConfig {
	out := KWinConfig{}
	if p != nil {
		out = p.KWin
	}
	if strings.TrimSpace(out.Patch) == "" {
		out.Patch = DefaultKWinPatchPath
	}
	if strings.TrimSpace(out.SourceMode) == "" {
		out.SourceMode = DefaultKWinSourceMode
	}
	if strings.TrimSpace(out.BuildDir) == "" {
		out.BuildDir = DefaultKWinBuildDir
	}
	if strings.TrimSpace(out.InstallMode) == "" {
		out.InstallMode = DefaultKWinInstallMode
	}
	if out.RequirePatch == nil {
		v := out.PatchedHDR
		out.RequirePatch = &v
	}
	// Validate by default when patched HDR is requested. Stock SDR
	// profiles skip the phase before this matters.
	if out.PatchedHDR && !out.ValidatePatch {
		out.ValidatePatch = true
	}
	if out.PatchedHDR && !out.HoldPackages {
		out.HoldPackages = true
	}
	return out
}

func (k KWinConfig) RequirePatchEnabled() bool {
	if k.RequirePatch == nil {
		return k.PatchedHDR
	}
	return *k.RequirePatch
}

// DesktopConfig governs the headless KDE/KWin Wayland substrate
// (Milestone 4A). All three phases that consume it - headless_user,
// desktop_runtime, kwin_session - read these fields. Defaults are
// applied lazily (the phases substitute their own defaults when the
// profile leaves a field empty).
//
//	user           account that owns the desktop session. Default
//	               "cloudgamer". A non-empty value MUST be a safe
//	               POSIX login name (see ValidateProfile rules).
//	enable_linger  loginctl enable-linger <user> so the systemd user
//	               bus + /run/user/<uid> stays alive when no one is
//	               logged in. Default true; required for the
//	               clouddeploy-kwin-wayland.service path.
//	groups         supplementary groups the user MUST be in to
//	               access DRM nodes, audio, input, etc. Default
//	               ["video", "render", "input", "audio",
//	               "systemd-journal"]. Empty list means "use the
//	               defaults". A profile can extend the list, e.g.
//	               add "kvm" for nested-virt workloads.
//	shell          login shell. Default /bin/bash. Honored only when
//	               headless_user actually creates the user.
//
// DesktopConfig is the operator-facing knob set. EnableLinger is a
// pointer so the YAML parser can distinguish "not set" (nil ->
// default-true) from "explicitly false". Live VM regressions
// repeatedly hit "linger=false" because the old bool defaulted to
// false on a missing key.
type DesktopConfig struct {
	User         string   `yaml:"user"`
	EnableLinger *bool    `yaml:"enable_linger"`
	Groups       []string `yaml:"groups"`
	Shell        string   `yaml:"shell"`

	// SessionBackend selects the KWin/Plasma session model. v2's
	// "realvt" backend is required for the headless NVIDIA Wayland
	// path to actually open /dev/dri/card*; the simpler "user"
	// backend (dbus-run-session + kwin_wayland) failed in production
	// with "Failed to activate login1 session" and "No suitable DRM
	// devices have been found". Default: "realvt".
	//
	//   "realvt"  - chvt to KwinVT, real logind session on /dev/tty<N>,
	//               PAMName=login, TTY*, UtmpIdentifier/Mode. Required
	//               for headless NVIDIA Wayland (Milestone 4B).
	//   "user"    - the Milestone 4A simple system service. Kept as
	//               an alternative for non-NVIDIA hosts where logind
	//               session activation isn't a barrier.
	//   "weston"  - diagnostic fallback. Brings up Weston in KMS mode
	//               instead of KWin so the operator can prove the
	//               DRM/KMS path independently of KWin/logind.
	SessionBackend string `yaml:"session_backend"`

	// KwinVT is the virtual terminal number realvt sessions claim
	// (and chvt to). Default 7 (matches v2).
	KwinVT int `yaml:"kwin_vt"`

	// CompositorMode selects which binary the realvt service launches.
	//   "plasma"  - startplasma-wayland (Plasma session, includes
	//               kwin_wayland). Default; matches v2's
	//               plasma-realvt.service.
	//   "kwin"    - kwin_wayland --drm --no-lockscreen directly. Use
	//               when Plasma isn't installed (small images).
	//   "weston"  - weston --backend=drm-backend.so. Diagnostic.
	CompositorMode string `yaml:"compositor_mode"`

	// KwinDRMDevice is the explicit /dev/dri/cardN the compositor
	// should target via KWIN_DRM_DEVICES. Empty defaults to
	// /dev/dri/card1 (NVIDIA's preferred card on the validated VM
	// after the edid phase) and falls back to /dev/dri/card0 if
	// card1 is absent at runtime.
	KwinDRMDevice string `yaml:"kwin_drm_device"`
}

// DefaultDesktopUser is the account headless_user creates when the
// profile leaves desktop.user empty.
const DefaultDesktopUser = "cloudgamer"

// DefaultDesktopShell is the login shell when the profile leaves
// desktop.shell empty.
const DefaultDesktopShell = "/bin/bash"

// DefaultSessionBackend is the v2-parity backend (real-VT logind).
const DefaultSessionBackend = "realvt"

// DefaultCompositorMode is what realvt launches by default.
//
// Live-VM evidence (2026-05-21): the direct kwin_wayland real-VT
// path produced a working DP-1 3840x2160@120 + private HDR + WCG
// session on Ubuntu 25.10 + NVIDIA 580 + RTX A6000. The full Plasma
// path (startplasma-wayland) exited with status=4 on the same host,
// even though the underlying KWin runtime was the patched one we
// just built. So `kwin` is now the validated Milestone-4 default;
// `plasma` is kept as an opt-in for future Plasma-UX work.
const DefaultCompositorMode = "kwin"

// DefaultKwinVT is the virtual terminal the realvt service claims.
const DefaultKwinVT = 7

// DefaultKwinDRMDevice is the NVIDIA card the validated VM exposes
// after the edid phase. The kwin_session probe falls back to
// /dev/dri/card0 if card1 is missing at runtime.
const DefaultKwinDRMDevice = "/dev/dri/card1"

// DefaultDesktopGroups is the supplementary-group list headless_user
// uses when the profile leaves desktop.groups empty. These groups
// MUST exist on every Ubuntu the deploy supports today.
//
//	video, render    DRM device access (/dev/dri/*).
//	input            evdev / libinput access (mostly for completeness;
//	                 a headless deploy doesn't need a keyboard).
//	audio            PipeWire / Pulse access.
//	systemd-journal  read journalctl without sudo - useful for
//	                 collect-logs and operator debugging.
var DefaultDesktopGroups = []string{"video", "render", "input", "audio", "systemd-journal"}

// EffectiveDesktop returns the DesktopConfig with profile-empty fields
// filled by the documented defaults. Phases call this instead of
// reading Profile.Desktop directly so default behavior is consistent
// across phases.
//
// Note: EnableLinger comes back as a non-nil *bool. nil in the input
// (missing YAML key) becomes pointer-to-true (the safe default for
// the headless session); a profile that explicitly sets
// `enable_linger: false` keeps the false pointer.
func (p *Profile) EffectiveDesktop() DesktopConfig {
	out := DesktopConfig{}
	if p != nil {
		out = p.Desktop
	}
	if strings.TrimSpace(out.User) == "" {
		out.User = DefaultDesktopUser
	}
	if strings.TrimSpace(out.Shell) == "" {
		out.Shell = DefaultDesktopShell
	}
	if len(out.Groups) == 0 {
		out.Groups = append([]string{}, DefaultDesktopGroups...)
	}
	if out.EnableLinger == nil {
		t := true
		out.EnableLinger = &t
	}
	if strings.TrimSpace(out.SessionBackend) == "" {
		out.SessionBackend = DefaultSessionBackend
	}
	if strings.TrimSpace(out.CompositorMode) == "" {
		out.CompositorMode = DefaultCompositorMode
	}
	if out.KwinVT == 0 {
		out.KwinVT = DefaultKwinVT
	}
	if strings.TrimSpace(out.KwinDRMDevice) == "" {
		out.KwinDRMDevice = DefaultKwinDRMDevice
	}
	return out
}

// LingerEnabled is a tiny convenience for callers that just want the
// bool value out of the *bool field. Safe on a zero DesktopConfig:
// returns true (the documented default).
func (d DesktopConfig) LingerEnabled() bool {
	if d.EnableLinger == nil {
		return true
	}
	return *d.EnableLinger
}

// DesktopLingerExplicit reports whether the operator put an explicit
// `enable_linger:` line in the profile YAML. Use this at the apply /
// phase entry point BEFORE calling EffectiveDesktop (which always
// fills the default pointer).
//
// State.Details["linger_defaulted"] should be `!DesktopLingerExplicit()`.
func (p *Profile) DesktopLingerExplicit() bool {
	if p == nil {
		return false
	}
	return p.Desktop.EnableLinger != nil
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

// WantsCUDARequired reports whether the profile hard-requires the CUDA
// toolkit (cuda.mode=required and the install is not disabled). Such a
// profile must not silently end up with a CUDA-less Sunshine build --
// see EffectiveSunshine.
func (p *Profile) WantsCUDARequired() bool {
	if p == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(p.CUDA.Mode), "required") {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(p.CUDA.Method), "none")
}

// WantsCUDA reports whether the profile asks for the CUDA toolkit to
// be installed (mode required or optional). Profiles that want CUDA
// constrain OS selection, because Sunshine's CUDA capture module only
// builds on some Ubuntu releases - see sunshine.CUDAModuleSupported.
func (p *Profile) WantsCUDA() bool {
	if p == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.CUDA.Mode)) {
	case "required", "optional":
		return strings.ToLower(strings.TrimSpace(p.CUDA.Method)) != "none"
	}
	return false
}

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
	if !isNumericMajorOrEmpty(p.CUDA.PreferMajor) {
		return fmt.Errorf("config: profile %q: cuda.prefer_major must be a positive integer string (e.g. \"13\"), got %q", p.Profile, p.CUDA.PreferMajor)
	}
	for _, c := range p.CUDA.CudaRepoDistroCandidates {
		if !isValidRepoCandidate(c) {
			return fmt.Errorf("config: profile %q: cuda.cuda_repo_distro_candidates entry %q is not \"auto-host\" or a slug like \"ubuntu2404\"", p.Profile, c)
		}
	}
	if len(p.CUDA.CudaRepoDistroCandidates) > 0 && !p.CUDA.AllowCrossDistroCudaRepo {
		// Allow if every entry is the auto-host literal.
		for _, c := range p.CUDA.CudaRepoDistroCandidates {
			if strings.TrimSpace(strings.ToLower(c)) != "auto-host" {
				return fmt.Errorf("config: profile %q: cuda.cuda_repo_distro_candidates contains a non-auto-host entry (%q) but cuda.allow_cross_distro_cuda_repo is false", p.Profile, c)
			}
		}
	}
	osPolicy := strings.ToLower(strings.TrimSpace(p.Deploy.UbuntuSelectionPolicy))
	switch osPolicy {
	case "", "latest-compatible", "exact", "min-version", "any":
		// ok
	default:
		return fmt.Errorf("config: profile %q: deploy.ubuntu_selection_policy must be one of latest-compatible/exact/min-version/any, got %q", p.Profile, p.Deploy.UbuntuSelectionPolicy)
	}
	for _, v := range p.Deploy.UbuntuCandidates {
		if !looksLikeUbuntuVersion(v) {
			return fmt.Errorf("config: profile %q: deploy.ubuntu_candidates entry %q is not an Ubuntu VERSION_ID (e.g. \"24.04\", \"25.10\")", p.Profile, v)
		}
	}
	if osPolicy == "min-version" && strings.TrimSpace(p.Deploy.UbuntuMinVersion) == "" {
		return fmt.Errorf("config: profile %q: deploy.ubuntu_selection_policy=min-version requires deploy.ubuntu_min_version to be set", p.Profile)
	}
	if strings.TrimSpace(p.Deploy.UbuntuMinVersion) != "" && !looksLikeUbuntuVersion(p.Deploy.UbuntuMinVersion) {
		return fmt.Errorf("config: profile %q: deploy.ubuntu_min_version %q is not an Ubuntu VERSION_ID", p.Profile, p.Deploy.UbuntuMinVersion)
	}
	if strings.TrimSpace(p.Desktop.User) != "" && !looksLikePOSIXLogin(p.Desktop.User) {
		return fmt.Errorf("config: profile %q: desktop.user %q is not a safe POSIX login name", p.Profile, p.Desktop.User)
	}
	for _, g := range p.Desktop.Groups {
		if !looksLikePOSIXLogin(g) {
			return fmt.Errorf("config: profile %q: desktop.groups entry %q is not a safe POSIX group name", p.Profile, g)
		}
	}
	switch strings.ToLower(strings.TrimSpace(p.Desktop.SessionBackend)) {
	case "", "realvt", "user", "weston":
		// ok
	default:
		return fmt.Errorf("config: profile %q: desktop.session_backend must be one of realvt/user/weston, got %q", p.Profile, p.Desktop.SessionBackend)
	}
	switch strings.ToLower(strings.TrimSpace(p.Desktop.CompositorMode)) {
	case "", "plasma", "kwin", "weston":
		// ok
	default:
		return fmt.Errorf("config: profile %q: desktop.compositor_mode must be one of plasma/kwin/weston, got %q", p.Profile, p.Desktop.CompositorMode)
	}
	if p.Desktop.KwinVT < 0 || p.Desktop.KwinVT > 63 {
		return fmt.Errorf("config: profile %q: desktop.kwin_vt must be 0-63, got %d", p.Profile, p.Desktop.KwinVT)
	}
	switch strings.ToLower(strings.TrimSpace(p.KWin.SourceMode)) {
	case "", "auto", "packaged", "source-patch":
		// ok
	default:
		return fmt.Errorf("config: profile %q: kwin.source_mode must be one of auto/packaged/source-patch, got %q", p.Profile, p.KWin.SourceMode)
	}
	switch strings.ToLower(strings.TrimSpace(p.KWin.InstallMode)) {
	case "", "packages", "prefix":
		// ok
	default:
		return fmt.Errorf("config: profile %q: kwin.install_mode must be one of packages/prefix, got %q", p.Profile, p.KWin.InstallMode)
	}
	switch strings.ToLower(strings.TrimSpace(p.Sunshine.Source)) {
	case "", "fork", "deb":
		// ok
	default:
		return fmt.Errorf("config: profile %q: sunshine.source must be empty/fork/deb, got %q", p.Profile, p.Sunshine.Source)
	}
	if p.Sunshine.Source == "fork" {
		if strings.TrimSpace(p.Sunshine.ForkRepo) == "" {
			return fmt.Errorf("config: profile %q: sunshine.fork_repo required for source=fork", p.Profile)
		}
		if strings.TrimSpace(p.Sunshine.ForkBranch) == "" {
			return fmt.Errorf("config: profile %q: sunshine.fork_branch required for source=fork", p.Profile)
		}
	}
	if p.Sunshine.BuildJobs < 0 {
		return fmt.Errorf("config: profile %q: sunshine.build_jobs must be >= 0", p.Profile)
	}
	switch strings.ToLower(strings.TrimSpace(p.Sunshine.EnableCUDA)) {
	case "", "auto", "true", "false":
		// ok
	default:
		return fmt.Errorf("config: profile %q: sunshine.enable_cuda must be auto/true/false, got %q", p.Profile, p.Sunshine.EnableCUDA)
	}
	switch p.Sunshine.GamepadValue() {
	case "auto", "xone", "ds5", "switch":
		// ok
	default:
		return fmt.Errorf("config: profile %q: sunshine.gamepad must be empty/auto/xone/ds5/switch or a supported xbox/playstation alias, got %q", p.Profile, p.Sunshine.Gamepad)
	}
	audio := p.EffectiveAudio()
	if !looksLikePulseName(audio.VirtualSink) {
		return fmt.Errorf("config: profile %q: audio.virtual_sink %q is not a safe PulseAudio/PipeWire node name", p.Profile, audio.VirtualSink)
	}
	if p.Audio.Rate < 0 {
		return fmt.Errorf("config: profile %q: audio.rate must be >= 0, got %d", p.Profile, p.Audio.Rate)
	}
	if p.Audio.Channels < 0 {
		return fmt.Errorf("config: profile %q: audio.channels must be >= 0, got %d", p.Profile, p.Audio.Channels)
	}
	if strings.TrimSpace(audio.ChannelMap) != "" && !looksLikePulseChannelMap(audio.ChannelMap) {
		return fmt.Errorf("config: profile %q: audio.channel_map %q is not a safe PulseAudio/PipeWire channel map", p.Profile, audio.ChannelMap)
	}
	if p.Deploy.MaxAutoReboots < 0 {
		return fmt.Errorf("config: profile %q: deploy.max_auto_reboots must be >= 0", p.Profile)
	}
	// Display mode must be one the universal forced EDID can advertise.
	// Only enforced when a resolution is set; an empty resolution means
	// "use the host's real EDID" and the edid phase skips. When a forced
	// connector is set the refresh must be a supported value too, since
	// the EDID is the only thing telling KWin/NVIDIA which modes exist.
	if res := strings.TrimSpace(p.Display.Resolution); res != "" {
		if !edid.IsSupportedResolution(res) {
			return fmt.Errorf("config: profile %q: display.resolution %q is not supported; supported: %s",
				p.Profile, res, strings.Join(edid.SupportedResolutions(), ", "))
		}
		if p.Display.Refresh != 0 && !edid.SupportsMode(res, p.Display.Refresh) {
			return fmt.Errorf("config: profile %q: display mode %s@%dHz is not supported; supported modes: %s",
				p.Profile, res, p.Display.Refresh, strings.Join(edid.SupportedModes(), ", "))
		}
		if strings.TrimSpace(p.Display.ForcedConnector) != "" && !edid.SupportsMode(res, p.Display.Refresh) {
			return fmt.Errorf("config: profile %q: display.forced_connector=%q requires a supported display.resolution@refresh (got %s@%dHz); supported: %s",
				p.Profile, p.Display.ForcedConnector, res, p.Display.Refresh, strings.Join(edid.SupportedModes(), ", "))
		}
	}
	if p.Display.HDR {
		if p.Sunshine.ForkCommit == "" {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.fork_commit pin", p.Profile)
		}
		// NOTE: force_av1_hdr10 is no longer required for HDR profiles. The
		// default HDR path now AUTO-MATCHES the client: a Sunshine
		// global_prep_cmd (clouddeploy-hdr-match) enables host HDR/WCG only
		// when the connecting client requested HDR, so the encode follows the
		// client without being forced. Forcing HDR unconditionally
		// (force_av1_hdr10=true) broke SDR clients (the host emitted a 10-bit
		// PQ stream the SDR client decoded as garbage). force_av1_hdr10 remains
		// an opt-in override for the old always-HDR behavior; when it is true
		// the auto-match prep-cmd is not wired.
		if !p.Sunshine.SynthesizeHDR10Metadata {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.synthesize_hdr10_metadata=true", p.Profile)
		}
		// Codec advertisement gate. Live-VM bug: with hevc_mode=0 the
		// Sunshine probe left active_hevc_mode below 3, so Ampere-class
		// GPUs had no HEVC Main10 fallback and Moonlight fell back to
		// H.264 while the force-HDR env vars still demanded p010 10-bit.
		// AV1 Main10 remains preferred when the encoder supports it, but
		// A5000/A6000/Ampere cards do not expose NVENC AV1. Do not reject
		// those deployments at config time; stream_validate verifies that
		// either AV1 Main10 OR HEVC Main10 is actually advertised.
		if !p.Sunshine.AdvertisesHEVCMain10() {
			return fmt.Errorf("config: profile %q: HDR profile requires sunshine.hevc_mode>=3 (HEVC Main10); got %d. AV1 is preferred when available, but HEVC Main10 is the required Ampere-safe HDR fallback.",
				p.Profile, p.Sunshine.HevcModeValue())
		}
		// KMS+NVENC HDR streaming does not require CUDA for the streaming
		// path; the toolkit is optional. We allow cuda.mode = none /
		// optional / required so an HDR profile can also test the CUDA
		// install end-to-end (e.g. hdr-4k120-cuda). No further HDR-CUDA
		// constraint here.
	}
	// Range check the codec modes for ALL profiles (HDR or not). Sunshine
	// silently treats out-of-range as 0 (auto-probe), which is the bug we
	// just closed for HDR; same applies to SDR profiles that want
	// 8-bit-only.
	if hm := p.Sunshine.HevcModeValue(); hm < 0 || hm > 3 {
		return fmt.Errorf("config: profile %q: sunshine.hevc_mode must be 0..3, got %d", p.Profile, hm)
	}
	if am := p.Sunshine.Av1ModeValue(); am < 0 || am > 3 {
		return fmt.Errorf("config: profile %q: sunshine.av1_mode must be 0..3, got %d", p.Profile, am)
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

// looksLikePOSIXLogin accepts a conservative subset of POSIX login
// / group names: starts with [a-z_], then [a-z0-9_-] up to 32 chars,
// with an optional trailing '$' (useful for machine accounts).
// Stricter than the kernel's rules so anything shell-meaningful
// (spaces, $, ;, |, backticks, redirects) is rejected before it
// reaches useradd / usermod / loginctl.
func looksLikePOSIXLogin(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" {
		return false
	}
	if len(v) > 32 {
		return false
	}
	if v[len(v)-1] == '$' {
		v = v[:len(v)-1]
		if v == "" {
			return false
		}
	}
	if !((v[0] >= 'a' && v[0] <= 'z') || v[0] == '_') {
		return false
	}
	for i := 1; i < len(v); i++ {
		c := v[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func looksLikePulseName(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" || len(v) > 96 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func looksLikePulseChannelMap(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" || len(v) > 256 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !((c >= 'a' && c <= 'z') || c == '-' || c == ',') {
			return false
		}
	}
	return true
}

// looksLikeUbuntuVersion accepts a "YY.MM" Ubuntu VERSION_ID (e.g.
// "22.04", "24.04", "25.10", "26.04"). Used by the deploy.ubuntu_*
// validators.
func looksLikeUbuntuVersion(s string) bool {
	v := strings.TrimSpace(s)
	if len(v) != 5 || v[2] != '.' {
		return false
	}
	for _, idx := range []int{0, 1, 3, 4} {
		c := v[idx]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isValidRepoCandidate accepts "auto-host" (case-insensitive) or a
// slug shaped like ubuntuXXXX (X = digit). Anything else - a typo, a
// non-Ubuntu distro slug, etc. - is rejected at validation time.
func isValidRepoCandidate(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "auto-host" {
		return true
	}
	if !strings.HasPrefix(v, "ubuntu") {
		return false
	}
	rest := strings.TrimPrefix(v, "ubuntu")
	if len(rest) < 4 {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
