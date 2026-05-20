package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoConfigDir returns the absolute path to the repo's config/
// directory by walking up from this test file. Keeps the test
// agnostic to where `go test` is invoked from.
func repoConfigDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	// internal/config -> internal -> repo root.
	repoRoot := filepath.Clean(filepath.Join(dir, "..", ".."))
	return filepath.Join(repoRoot, "config")
}

// TestAllProfilesParseAndValidate makes sure every YAML in
// config/profiles/ loads cleanly and passes ValidateProfile.
func TestAllProfilesParseAndValidate(t *testing.T) {
	dir := repoConfigDir(t)
	profiles, err := LoadAllProfiles(dir)
	if err != nil {
		t.Fatalf("LoadAllProfiles: %v", err)
	}
	if len(profiles) == 0 {
		t.Fatalf("expected at least one profile under %s/profiles", dir)
	}
	for _, p := range profiles {
		t.Run(p.Profile, func(t *testing.T) {
			if err := ValidateProfile(p); err != nil {
				t.Errorf("ValidateProfile: %v", err)
			}
		})
	}
}

// TestHDRProfileSpec locks in the hdr-4k120 profile's invariants:
// fork pin, HDR env. cuda.mode=none stays the default for the
// minimal HDR profile; hdr-4k120-cuda owns the CUDA-required variant.
func TestHDRProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if !p.Display.HDR {
		t.Fatal("hdr-4k120: display.hdr must be true")
	}
	if !p.Sunshine.ForceAV1HDR10 {
		t.Error("hdr-4k120: sunshine.force_av1_hdr10 must be true")
	}
	if !p.Sunshine.SynthesizeHDR10Metadata {
		t.Error("hdr-4k120: sunshine.synthesize_hdr10_metadata must be true")
	}
	if strings.ToLower(p.CUDA.Mode) != "none" {
		t.Errorf("hdr-4k120: cuda.mode must be 'none' (minimal HDR profile); got %q", p.CUDA.Mode)
	}
	const wantCommit = "464bccf1b6e33bf35138136c6138fd9851e6d906"
	if p.Sunshine.ForkCommit != wantCommit {
		t.Errorf("hdr-4k120: sunshine.fork_commit pin moved unexpectedly; got %q want %q", p.Sunshine.ForkCommit, wantCommit)
	}
	if p.Sunshine.Source != "fork" {
		t.Errorf("hdr-4k120: sunshine.source must be 'fork' (we build from the pinned commit), got %q", p.Sunshine.Source)
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile: %v", err)
	}
}

// TestHDRCudaProfileSpec locks the strict CUDA-13-only invariants for
// hdr-4k120-cuda: exact-major=13, no Ubuntu archive fallback, compile
// smoke test on, runfile pin held empty pending a known-good artifact.
func TestHDRCudaProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-cuda")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-cuda): %v", err)
	}
	if !p.Display.HDR {
		t.Fatal("hdr-4k120-cuda: display.hdr must be true")
	}
	if strings.ToLower(p.CUDA.Mode) != "required" {
		t.Errorf("hdr-4k120-cuda: cuda.mode must be 'required', got %q", p.CUDA.Mode)
	}
	if strings.ToLower(p.CUDA.Method) != "apt" {
		t.Errorf("hdr-4k120-cuda: cuda.method must be 'apt' while no known-good runfile is pinned; got %q", p.CUDA.Method)
	}
	if strings.ToLower(p.CUDA.SelectionPolicy) != "exact-major" {
		t.Errorf("hdr-4k120-cuda: cuda.selection_policy must be exact-major (strict CUDA 13); got %q", p.CUDA.SelectionPolicy)
	}
	if p.CUDA.ExpectedMajor != "13" {
		t.Errorf("hdr-4k120-cuda: cuda.expected_major must be \"13\"; got %q", p.CUDA.ExpectedMajor)
	}
	if p.CUDA.AllowUbuntuArchiveFallback {
		t.Error("hdr-4k120-cuda: cuda.allow_ubuntu_archive_fallback must be false (Ubuntu archive ships CUDA 12)")
	}
	if !p.CUDA.CompileSmokeTest {
		t.Error("hdr-4k120-cuda: cuda.compile_smoke_test must be true (CUDA-required must prove nvcc works)")
	}
	if !p.CUDA.AllowCrossDistroCudaRepo {
		t.Error("hdr-4k120-cuda: cuda.allow_cross_distro_cuda_repo must be true (cross-distro CUDA 13 via ubuntu2404 is the supported path on 25.10)")
	}
	wantCandidates := []string{"auto-host", "ubuntu2404"}
	if len(p.CUDA.CudaRepoDistroCandidates) != len(wantCandidates) {
		t.Errorf("hdr-4k120-cuda: cuda.cuda_repo_distro_candidates got %v want %v", p.CUDA.CudaRepoDistroCandidates, wantCandidates)
	} else {
		for i, c := range wantCandidates {
			if p.CUDA.CudaRepoDistroCandidates[i] != c {
				t.Errorf("hdr-4k120-cuda: cuda.cuda_repo_distro_candidates[%d] got %q want %q", i, p.CUDA.CudaRepoDistroCandidates[i], c)
			}
		}
	}
	if strings.TrimSpace(p.CUDA.RunfileURL) != "" {
		t.Errorf("hdr-4k120-cuda: cuda.runfile_url must be empty (CUDA 13.0.2 mirror is corrupt); got %q", p.CUDA.RunfileURL)
	}
	if strings.TrimSpace(p.CUDA.RunfileSHA256) != "" {
		t.Errorf("hdr-4k120-cuda: cuda.runfile_sha256 must be empty until a known-good artifact is captured; got %q", p.CUDA.RunfileSHA256)
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-cuda): %v", err)
	}
}

// TestHDRCudaCompatibleProfileSpec locks the broad-compat invariants
// for hdr-4k120-cuda-compatible: latest-compatible policy, min_major=12,
// Ubuntu archive fallback allowed, compile smoke test on.
func TestHDRCudaCompatibleProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-cuda-compatible")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-cuda-compatible): %v", err)
	}
	if strings.ToLower(p.CUDA.Mode) != "required" {
		t.Errorf("compat: cuda.mode must be 'required'; got %q", p.CUDA.Mode)
	}
	if strings.ToLower(p.CUDA.SelectionPolicy) != "latest-compatible" {
		t.Errorf("compat: cuda.selection_policy must be latest-compatible; got %q", p.CUDA.SelectionPolicy)
	}
	if p.CUDA.MinMajor != "12" {
		t.Errorf("compat: cuda.min_major must be \"12\"; got %q", p.CUDA.MinMajor)
	}
	if !p.CUDA.AllowUbuntuArchiveFallback {
		t.Error("compat: cuda.allow_ubuntu_archive_fallback must be true")
	}
	if !p.CUDA.CompileSmokeTest {
		t.Error("compat: cuda.compile_smoke_test must be true")
	}
	if !p.CUDA.AllowCrossDistroCudaRepo {
		t.Error("compat: cuda.allow_cross_distro_cuda_repo must be true")
	}
	if p.CUDA.PreferMajor != "13" {
		t.Errorf("compat: cuda.prefer_major must be \"13\"; got %q", p.CUDA.PreferMajor)
	}
	if !p.CUDA.PreferNewest {
		t.Error("compat: cuda.prefer_newest must be true")
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-cuda-compatible): %v", err)
	}
}

// TestEffectiveDesktop_AppliesDefaults documents the default
// substitution: empty user -> "cloudgamer", empty groups -> the
// canonical list, empty shell -> /bin/bash.
func TestEffectiveDesktop_AppliesDefaults(t *testing.T) {
	p := &Profile{}
	got := p.EffectiveDesktop()
	if got.User != DefaultDesktopUser {
		t.Errorf("user: got %q want %q", got.User, DefaultDesktopUser)
	}
	if got.Shell != DefaultDesktopShell {
		t.Errorf("shell: got %q want %q", got.Shell, DefaultDesktopShell)
	}
	if len(got.Groups) != len(DefaultDesktopGroups) {
		t.Errorf("groups: got %v want %v", got.Groups, DefaultDesktopGroups)
	}
}

func TestEffectiveDesktop_RespectsProfileOverrides(t *testing.T) {
	p := &Profile{Desktop: DesktopConfig{
		User:   "operator",
		Shell:  "/usr/bin/fish",
		Groups: []string{"video", "render"},
	}}
	got := p.EffectiveDesktop()
	if got.User != "operator" {
		t.Errorf("user: got %q want operator", got.User)
	}
	if got.Shell != "/usr/bin/fish" {
		t.Errorf("shell: got %q want /usr/bin/fish", got.Shell)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "video" || got.Groups[1] != "render" {
		t.Errorf("groups: got %v want [video render]", got.Groups)
	}
}

func TestEffectiveDesktop_EnableLingerDefaultsTrue(t *testing.T) {
	p := &Profile{} // empty -> no enable_linger key at all
	got := p.EffectiveDesktop()
	if !got.LingerEnabled() {
		t.Errorf("missing enable_linger: LingerEnabled should be true")
	}
	if p.DesktopLingerExplicit() {
		t.Errorf("missing enable_linger: DesktopLingerExplicit should be false")
	}
}

func TestEffectiveDesktop_EnableLingerExplicitFalseIsRespected(t *testing.T) {
	f := false
	p := &Profile{Desktop: DesktopConfig{EnableLinger: &f}}
	got := p.EffectiveDesktop()
	if got.LingerEnabled() {
		t.Errorf("explicit enable_linger: false should NOT be flipped to true")
	}
	if !p.DesktopLingerExplicit() {
		t.Errorf("explicit enable_linger: false should mark DesktopLingerExplicit true")
	}
}

func TestEffectiveDesktop_SessionBackendDefaultsRealvt(t *testing.T) {
	p := &Profile{}
	got := p.EffectiveDesktop()
	if got.SessionBackend != DefaultSessionBackend {
		t.Errorf("session_backend default: got %q want %q", got.SessionBackend, DefaultSessionBackend)
	}
	if got.CompositorMode != DefaultCompositorMode {
		t.Errorf("compositor_mode default: got %q want %q", got.CompositorMode, DefaultCompositorMode)
	}
	if got.KwinVT != DefaultKwinVT {
		t.Errorf("kwin_vt default: got %d want %d", got.KwinVT, DefaultKwinVT)
	}
	if got.KwinDRMDevice != DefaultKwinDRMDevice {
		t.Errorf("kwin_drm_device default: got %q want %q", got.KwinDRMDevice, DefaultKwinDRMDevice)
	}
}

func TestEffectiveKWin_DefaultsPatchedHDR(t *testing.T) {
	t.Run("stock profile skips patch by default", func(t *testing.T) {
		p := &Profile{}
		got := p.EffectiveKWin()
		if got.PatchedHDR {
			t.Errorf("PatchedHDR default should be false")
		}
		if got.Patch != DefaultKWinPatchPath {
			t.Errorf("Patch default: got %q want %q", got.Patch, DefaultKWinPatchPath)
		}
		if got.SourceMode != DefaultKWinSourceMode {
			t.Errorf("SourceMode default: got %q want %q", got.SourceMode, DefaultKWinSourceMode)
		}
		if got.BuildDir != DefaultKWinBuildDir {
			t.Errorf("BuildDir default: got %q want %q", got.BuildDir, DefaultKWinBuildDir)
		}
		if got.RequirePatchEnabled() {
			t.Errorf("RequirePatch should follow PatchedHDR=false")
		}
	})
	t.Run("hdr defaults fail closed", func(t *testing.T) {
		p := &Profile{KWin: KWinConfig{PatchedHDR: true}}
		got := p.EffectiveKWin()
		if !got.RequirePatchEnabled() {
			t.Errorf("RequirePatch should follow PatchedHDR=true")
		}
		if !got.ValidatePatch {
			t.Errorf("ValidatePatch should default true for patched HDR")
		}
		if !got.HoldPackages {
			t.Errorf("HoldPackages should default true for patched HDR")
		}
	})
}

// TestHDRAutoProfileSpec exercises the OS-resolver-driven HDR profile.
// ubuntu_version is intentionally empty; the resolver picks the OS.
func TestHDRAutoProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-auto")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-auto): %v", err)
	}
	if p.UbuntuVersion != "" {
		t.Errorf("auto profile must leave ubuntu_version empty; got %q", p.UbuntuVersion)
	}
	if p.Deploy.UbuntuSelectionPolicy != "latest-compatible" {
		t.Errorf("ubuntu_selection_policy: got %q want latest-compatible", p.Deploy.UbuntuSelectionPolicy)
	}
	want := []string{"26.04", "25.10", "24.04"}
	if len(p.Deploy.UbuntuCandidates) != len(want) {
		t.Errorf("ubuntu_candidates: got %v want %v", p.Deploy.UbuntuCandidates, want)
	} else {
		for i, v := range want {
			if p.Deploy.UbuntuCandidates[i] != v {
				t.Errorf("ubuntu_candidates[%d]: got %q want %q", i, p.Deploy.UbuntuCandidates[i], v)
			}
		}
	}
	if strings.ToLower(p.CUDA.Mode) != "none" {
		t.Errorf("auto profile cuda.mode should be none; got %q", p.CUDA.Mode)
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-auto): %v", err)
	}
}

// TestHDRCudaAutoProfileSpec exercises the combined OS+CUDA-resolver
// profile: both the Ubuntu candidate list AND the cross-distro CUDA
// candidate list are populated.
func TestHDRCudaAutoProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-cuda-auto")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-cuda-auto): %v", err)
	}
	if p.UbuntuVersion != "" {
		t.Errorf("cuda-auto profile must leave ubuntu_version empty; got %q", p.UbuntuVersion)
	}
	if p.Deploy.UbuntuSelectionPolicy != "latest-compatible" {
		t.Errorf("ubuntu_selection_policy: got %q want latest-compatible", p.Deploy.UbuntuSelectionPolicy)
	}
	wantOS := []string{"26.04", "25.10", "24.04"}
	for i, v := range wantOS {
		if i >= len(p.Deploy.UbuntuCandidates) || p.Deploy.UbuntuCandidates[i] != v {
			t.Errorf("ubuntu_candidates[%d]: got %v want %q", i, p.Deploy.UbuntuCandidates, v)
		}
	}
	if !p.CUDA.AllowCrossDistroCudaRepo {
		t.Error("cuda.allow_cross_distro_cuda_repo must be true")
	}
	if p.CUDA.PreferMajor != "13" {
		t.Errorf("cuda.prefer_major: got %q want 13", p.CUDA.PreferMajor)
	}
	if !p.CUDA.CompileSmokeTest {
		t.Error("cuda.compile_smoke_test must be true")
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-cuda-auto): %v", err)
	}
}

// TestHDRCudaNativeProfileSpec exercises the diagnostic native-only
// profile. It pins auto-host with no cross-distro / archive / runfile
// fallback so a failure is the answer to "does NVIDIA officially
// support the host's Ubuntu version yet?".
func TestHDRCudaNativeProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-cuda-native")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-cuda-native): %v", err)
	}
	if p.CUDA.AllowCrossDistroCudaRepo {
		t.Error("native: cuda.allow_cross_distro_cuda_repo must be false (host-only)")
	}
	if p.CUDA.AllowUbuntuArchiveFallback {
		t.Error("native: cuda.allow_ubuntu_archive_fallback must be false")
	}
	wantCandidates := []string{"auto-host"}
	if len(p.CUDA.CudaRepoDistroCandidates) != len(wantCandidates) || p.CUDA.CudaRepoDistroCandidates[0] != wantCandidates[0] {
		t.Errorf("native: cuda.cuda_repo_distro_candidates got %v want %v", p.CUDA.CudaRepoDistroCandidates, wantCandidates)
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-cuda-native): %v", err)
	}
}

// TestHDRCuda2404ProfileSpec exercises the Ubuntu 24.04 LTS variant
// added because NVIDIA does not publish a CUDA apt repo for Ubuntu
// 25.10 and the 13.0.2 runfile is corrupt. This profile is the
// known-working v3 CUDA-required test target until the 25.10 path is
// unblocked.
func TestHDRCuda2404ProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "hdr-4k120-cuda-ubuntu2404")
	if err != nil {
		t.Fatalf("LoadProfile(hdr-4k120-cuda-ubuntu2404): %v", err)
	}
	if p.UbuntuVersion != "24.04" {
		t.Errorf("ubuntu_version must be \"24.04\"; got %q", p.UbuntuVersion)
	}
	if !p.Display.HDR {
		t.Error("display.hdr must be true")
	}
	if strings.ToLower(p.CUDA.Mode) != "required" {
		t.Errorf("cuda.mode must be 'required'; got %q", p.CUDA.Mode)
	}
	if strings.ToLower(p.CUDA.Method) != "apt" {
		t.Errorf("cuda.method must be 'apt' (the ubuntu2404 NVIDIA CUDA repo is the only known-good path); got %q", p.CUDA.Method)
	}
	if strings.TrimSpace(p.CUDA.RunfileURL) != "" {
		t.Errorf("cuda.runfile_url must be empty for the apt-only profile; got %q", p.CUDA.RunfileURL)
	}
	if p.NVIDIA.DriverMajor != "580" {
		t.Errorf("nvidia.driver_major must be 580 (pairs with CUDA 13); got %q", p.NVIDIA.DriverMajor)
	}
	if p.Deploy.AutoUpgradeUbuntu {
		t.Error("deploy.auto_upgrade_ubuntu must be false: this profile must NOT upgrade off 24.04")
	}
	if strings.ToLower(p.CUDA.SelectionPolicy) != "exact-major" {
		t.Errorf("cuda.selection_policy must be exact-major (strict CUDA 13); got %q", p.CUDA.SelectionPolicy)
	}
	if p.CUDA.ExpectedMajor != "13" {
		t.Errorf("cuda.expected_major must be \"13\"; got %q", p.CUDA.ExpectedMajor)
	}
	if p.CUDA.AllowUbuntuArchiveFallback {
		t.Error("cuda.allow_ubuntu_archive_fallback must be false on the strict profile")
	}
	if !p.CUDA.CompileSmokeTest {
		t.Error("cuda.compile_smoke_test must be true")
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile(hdr-4k120-cuda-ubuntu2404): %v", err)
	}
}

// TestHDRWithCudaRequiredValidates is the regression-prevention test
// for the "HDR forced cuda.mode=none" rule we just removed: HDR +
// cuda.mode=required must validate cleanly.
func TestHDRWithCudaRequiredValidates(t *testing.T) {
	p := &Profile{
		Profile: "hdr-required-test",
		NVIDIA:  NVIDIAConfig{DriverMajor: "580"},
		CUDA:    CUDAConfig{Mode: "required", Method: "auto"},
		Display: DisplayConfig{HDR: true},
		Sunshine: SunshineConfig{
			Source:                  "fork",
			ForkRepo:                "https://example/Sunshine",
			ForkBranch:              "main",
			ForkCommit:              "abc",
			ForceAV1HDR10:           true,
			SynthesizeHDR10Metadata: true,
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Fatalf("HDR + cuda.mode=required must validate; got: %v", err)
	}
}

// TestSDRProfileSpec sanity-checks the safe SDR profile.
func TestSDRProfileSpec(t *testing.T) {
	dir := repoConfigDir(t)
	p, err := LoadProfile(dir, "sdr-safe")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if p.Display.HDR {
		t.Error("sdr-safe: display.hdr must be false")
	}
	if p.Sunshine.Source != "deb" {
		t.Errorf("sdr-safe: sunshine.source should be 'deb', got %q", p.Sunshine.Source)
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile: %v", err)
	}
}

// TestAllGPUProfilesParseAndValidate makes sure every YAML in
// config/gpus/ loads cleanly and passes ValidateGPU.
func TestAllGPUProfilesParseAndValidate(t *testing.T) {
	dir := repoConfigDir(t)
	gpus, err := LoadAllGPUs(dir)
	if err != nil {
		t.Fatalf("LoadAllGPUs: %v", err)
	}
	if len(gpus) == 0 {
		t.Fatalf("expected at least one GPU overlay under %s/gpus", dir)
	}
	for _, g := range gpus {
		t.Run(g.Filename, func(t *testing.T) {
			if err := ValidateGPU(g); err != nil {
				t.Errorf("ValidateGPU: %v", err)
			}
		})
	}
}

// TestKnownGPUCategoriesPresent enforces coverage: the brief's GPU
// categories must each be represented by at least one config/gpus/
// YAML. Catches accidental file deletions and reminds us to add a
// GPU profile when a new category is supported.
func TestKnownGPUCategoriesPresent(t *testing.T) {
	dir := repoConfigDir(t)
	gpus, err := LoadAllGPUs(dir)
	if err != nil {
		t.Fatalf("LoadAllGPUs: %v", err)
	}
	want := []string{
		"blackwell-consumer", // RTX 50-series
		"ada-consumer",       // RTX 40-series
		"ampere-consumer",    // RTX 30-series
		"turing-rtx-consumer",
		"turing16-consumer",
		"blackwell-pro",
		"ada-pro",
		"ampere-a-series",
		"l4",
		"l40",
		"ampere-a10-a40",
		"a100",
		"hopper",
		"blackwell-dc",
		"t4",
		"v100",
		"pascal-legacy",
	}
	got := map[string]bool{}
	for _, g := range gpus {
		got[strings.TrimSuffix(g.Filename, ".yaml")] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing config/gpus/%s.yaml", name)
		}
	}
}

// TestProfileValidatorRejectsBadInputs covers the negative space:
// the validator must NOT accept obvious mistakes.
func TestProfileValidatorRejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Profile)
		want string
	}{
		{"empty profile name", func(p *Profile) { p.Profile = "" }, "profile name is empty"},
		{"empty driver_major", func(p *Profile) { p.NVIDIA.DriverMajor = "" }, "driver_major"},
		{"bad cuda mode", func(p *Profile) { p.CUDA.Mode = "yes-please" }, "cuda.mode"},
		{"bad sunshine source", func(p *Profile) { p.Sunshine.Source = "snap" }, "sunshine.source"},
		{"fork without repo", func(p *Profile) { p.Sunshine.ForkRepo = "" }, "sunshine.fork_repo"},
		{"hdr without fork commit", func(p *Profile) {
			p.Display.HDR = true
			p.Sunshine.ForkCommit = ""
		}, "sunshine.fork_commit"},
		{"hdr without force_av1_hdr10", func(p *Profile) {
			p.Display.HDR = true
			p.Sunshine.ForceAV1HDR10 = false
		}, "force_av1_hdr10"},
		{"bad cuda method", func(p *Profile) { p.CUDA.Method = "snap" }, "cuda.method"},
		{"bad cuda selection_policy", func(p *Profile) { p.CUDA.SelectionPolicy = "yolo" }, "cuda.selection_policy"},
		{"bad cuda expected_major", func(p *Profile) { p.CUDA.ExpectedMajor = "13.0" }, "cuda.expected_major"},
		{"bad cuda min_major", func(p *Profile) { p.CUDA.MinMajor = "twelve" }, "cuda.min_major"},
		{"exact-major missing expected_major", func(p *Profile) {
			p.CUDA.SelectionPolicy = "exact-major"
			p.CUDA.ExpectedMajor = ""
		}, "expected_major"},
		{"min-major missing min_major", func(p *Profile) {
			p.CUDA.SelectionPolicy = "min-major"
			p.CUDA.MinMajor = ""
		}, "min_major"},
		{"bad cuda prefer_major", func(p *Profile) { p.CUDA.PreferMajor = "13.0" }, "cuda.prefer_major"},
		{"bad cuda repo distro candidate", func(p *Profile) {
			p.CUDA.AllowCrossDistroCudaRepo = true
			p.CUDA.CudaRepoDistroCandidates = []string{"redhat9"}
		}, "cuda_repo_distro_candidates"},
		{"cross-distro candidate without flag", func(p *Profile) {
			p.CUDA.AllowCrossDistroCudaRepo = false
			p.CUDA.CudaRepoDistroCandidates = []string{"auto-host", "ubuntu2404"}
		}, "allow_cross_distro_cuda_repo"},
		{"bad ubuntu_selection_policy", func(p *Profile) {
			p.Deploy.UbuntuSelectionPolicy = "yolo"
		}, "ubuntu_selection_policy"},
		{"bad ubuntu_candidates entry", func(p *Profile) {
			p.Deploy.UbuntuCandidates = []string{"24.04", "ubuntu2510"}
		}, "ubuntu_candidates"},
		{"min-version without min_version", func(p *Profile) {
			p.Deploy.UbuntuSelectionPolicy = "min-version"
			p.Deploy.UbuntuMinVersion = ""
		}, "ubuntu_min_version"},
		{"bad ubuntu_min_version", func(p *Profile) {
			p.Deploy.UbuntuMinVersion = "questing"
		}, "ubuntu_min_version"},
		{"unsafe desktop.user", func(p *Profile) {
			p.Desktop.User = "../etc/passwd"
		}, "desktop.user"},
		{"unsafe desktop.groups entry", func(p *Profile) {
			p.Desktop.User = "cloudgamer"
			p.Desktop.Groups = []string{"video", "evil; rm -rf /"}
		}, "desktop.groups"},
		{"bad desktop.session_backend", func(p *Profile) {
			p.Desktop.SessionBackend = "kvm"
		}, "session_backend"},
		{"bad desktop.compositor_mode", func(p *Profile) {
			p.Desktop.CompositorMode = "gnome"
		}, "compositor_mode"},
		{"out-of-range desktop.kwin_vt", func(p *Profile) {
			p.Desktop.KwinVT = 99
		}, "kwin_vt"},
		{"bad kwin.source_mode", func(p *Profile) {
			p.KWin.SourceMode = "neon"
		}, "kwin.source_mode"},
		{"bad kwin.install_mode", func(p *Profile) {
			p.KWin.InstallMode = "overwrite-system"
		}, "kwin.install_mode"},
	}
	base := func() *Profile {
		return &Profile{
			Profile: "test",
			NVIDIA:  NVIDIAConfig{DriverMajor: "580"},
			CUDA:    CUDAConfig{Mode: "none"},
			Sunshine: SunshineConfig{
				Source:                  "fork",
				ForkRepo:                "https://example/Sunshine",
				ForkBranch:              "main",
				ForkCommit:              "abc",
				ForceAV1HDR10:           true,
				SynthesizeHDR10Metadata: true,
			},
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mut(p)
			err := ValidateProfile(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestGPUValidatorRejectsBadInputs(t *testing.T) {
	t.Run("no match clauses", func(t *testing.T) {
		g := &GPUProfile{Filename: "empty.yaml"}
		if err := ValidateGPU(g); err == nil {
			t.Fatalf("expected error for empty match")
		}
	})
	t.Run("bad pci_id", func(t *testing.T) {
		g := &GPUProfile{
			Filename: "bad-pciid.yaml",
			Match:    GPUMatch{PCIIDs: []string{"not-a-pciid"}},
		}
		err := ValidateGPU(g)
		if err == nil || !strings.Contains(err.Error(), "pci_id") {
			t.Fatalf("expected pci_id error, got %v", err)
		}
	})
	t.Run("bad streaming value", func(t *testing.T) {
		g := &GPUProfile{
			Filename:  "bad-streaming.yaml",
			Match:     GPUMatch{NamePatterns: []string{"X"}},
			Streaming: GPUStreamingFlag{AV1Encode: "maybe"},
		}
		err := ValidateGPU(g)
		if err == nil || !strings.Contains(err.Error(), "av1_encode") {
			t.Fatalf("expected av1_encode error, got %v", err)
		}
	})
}
