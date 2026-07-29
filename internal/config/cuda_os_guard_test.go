package config

import "testing"

// Regression guard for the 4K120-HDR throughput bug.
//
// History: a profile could set cuda.mode=required, install the toolkit,
// and still ship a Sunshine binary built with SUNSHINE_ENABLE_CUDA=OFF,
// because a failed CUDA cmake-configure silently retries without CUDA
// under enable_cuda="auto". The deploy reported success while capture
// ran on the GPU -> RAM -> GPU path: one core pinned, NVENC idle, the
// whole 4K120 HDR session capped near 40fps.
//
// The fix is a linkage, not an OS allowlist: cuda.mode=required
// promotes sunshine.enable_cuda to "true" so the build fails loudly.

func TestWantsCUDA(t *testing.T) {
	cases := []struct {
		mode, method string
		want         bool
	}{
		{"required", "", true},
		{"optional", "apt", true},
		{"REQUIRED", "", true}, // case-insensitive
		{"none", "", false},
		{"", "", false},
		// method=none disables the install even when mode says otherwise.
		{"required", "none", false},
	}
	for _, c := range cases {
		p := &Profile{CUDA: CUDAConfig{Mode: c.mode, Method: c.method}}
		if got := p.WantsCUDA(); got != c.want {
			t.Errorf("WantsCUDA(mode=%q, method=%q) = %v, want %v", c.mode, c.method, got, c.want)
		}
	}
}

func TestWantsCUDARequired(t *testing.T) {
	cases := []struct {
		mode, method string
		want         bool
	}{
		{"required", "", true},
		{"Required", "apt", true},
		{"optional", "apt", false}, // optional must NOT hard-fail the build
		{"none", "", false},
		{"required", "none", false},
	}
	for _, c := range cases {
		p := &Profile{CUDA: CUDAConfig{Mode: c.mode, Method: c.method}}
		if got := p.WantsCUDARequired(); got != c.want {
			t.Errorf("WantsCUDARequired(mode=%q, method=%q) = %v, want %v", c.mode, c.method, got, c.want)
		}
	}
}

// The core regression test: required CUDA must not be able to degrade
// into a silent no-CUDA Sunshine build.
func TestEffectiveSunshine_RequiredCUDAForbidsSilentFallback(t *testing.T) {
	p := &Profile{
		Profile: "t",
		CUDA:    CUDAConfig{Mode: "required", Method: "apt"},
	}
	if got := p.EffectiveSunshine().EnableCUDA; got != "true" {
		t.Fatalf("EnableCUDA = %q, want \"true\" so a failed CUDA configure fails the deploy instead of silently shipping GPU->RAM->GPU capture", got)
	}
}

func TestEffectiveSunshine_ExplicitOperatorChoiceWins(t *testing.T) {
	// An operator who wants the toolkit for other work but does not
	// want the Sunshine build to hard-fail must keep that ability.
	p := &Profile{
		Profile:  "t",
		CUDA:     CUDAConfig{Mode: "required", Method: "apt"},
		Sunshine: SunshineConfig{EnableCUDA: "auto"},
	}
	if got := p.EffectiveSunshine().EnableCUDA; got != "auto" {
		t.Fatalf("EnableCUDA = %q, want \"auto\" (explicit profile value must be honored)", got)
	}
}

func TestEffectiveSunshine_OptionalCUDAKeepsAutoDefault(t *testing.T) {
	p := &Profile{
		Profile: "t",
		CUDA:    CUDAConfig{Mode: "optional", Method: "apt"},
	}
	if got := p.EffectiveSunshine().EnableCUDA; got != DefaultSunshineEnableCUDA {
		t.Fatalf("EnableCUDA = %q, want %q (optional means best-effort)", got, DefaultSunshineEnableCUDA)
	}
}
