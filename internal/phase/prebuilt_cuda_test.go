package phase

import "testing"

// A prebuilt bundle may only satisfy a CUDA-requiring profile when it
// positively declares a CUDA-enabled Sunshine. Older bundles omit the
// field entirely -- and one such published bundle really does contain a
// CUDA-less binary, which silently degraded capture to GPU -> RAM -> GPU
// (~40fps at 4K120 HDR) on every deploy that accepted it.
func TestPrebuiltManifestHasCUDA(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name string
		m    PrebuiltManifest
		want bool
	}{
		{"declares true", PrebuiltManifest{CUDA: &tr}, true},
		{"declares false", PrebuiltManifest{CUDA: &fa}, false},
		{"absent (legacy bundle) must not count as CUDA", PrebuiltManifest{}, false},
	}
	for _, c := range cases {
		if got := c.m.HasCUDA(); got != c.want {
			t.Errorf("%s: HasCUDA() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPrebuiltManifestCUDAClaimIsDiagnostic(t *testing.T) {
	tr, fa := true, false
	if got := (PrebuiltManifest{}).CUDAClaim(); got == "true" || got == "false" {
		t.Errorf("absent CUDA must render as unknown, got %q", got)
	}
	if got := (PrebuiltManifest{CUDA: &tr}).CUDAClaim(); got != "true" {
		t.Errorf("CUDAClaim() = %q, want true", got)
	}
	if got := (PrebuiltManifest{CUDA: &fa}).CUDAClaim(); got != "false" {
		t.Errorf("CUDAClaim() = %q, want false", got)
	}
}
