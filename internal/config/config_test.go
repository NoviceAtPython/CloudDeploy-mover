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
// fork pin, HDR env, cuda.mode=none.
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
		t.Errorf("hdr-4k120: cuda.mode must be 'none', got %q", p.CUDA.Mode)
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
		{"hdr cuda required", func(p *Profile) {
			p.Display.HDR = true
			p.CUDA.Mode = "required"
		}, "cuda.mode=none"},
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
