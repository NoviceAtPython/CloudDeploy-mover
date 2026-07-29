package config

import "testing"

// Regression guard for the 4K120-HDR throughput bug: a profile that
// installs the CUDA toolkit onto an Ubuntu release where Sunshine's
// CUDA capture module cannot be built deploys "green" and then streams
// on the GPU -> RAM -> GPU fallback (~40fps, one core pinned). The
// contradiction must be caught at config-parse time.

func cudaGuardProfile(mode, ubuntuVersion string, candidates []string) *Profile {
	return &Profile{
		Profile:       "test-cuda-guard",
		UbuntuVersion: ubuntuVersion,
		CUDA:          CUDAConfig{Mode: mode},
		Deploy:        DeployConfig{UbuntuCandidates: candidates},
	}
}

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

func TestValidateCUDAOSTarget_PinnedVersion(t *testing.T) {
	// 25.10 cannot build the module -> reject.
	if err := validateCUDAOSTarget(cudaGuardProfile("required", "25.10", nil)); err == nil {
		t.Fatal("expected error for cuda.mode=required pinned to 25.10")
	}
	// 24.04 can -> accept.
	if err := validateCUDAOSTarget(cudaGuardProfile("required", "24.04", nil)); err != nil {
		t.Fatalf("24.04 must be accepted, got %v", err)
	}
	// Unpinned -> nothing to check yet.
	if err := validateCUDAOSTarget(cudaGuardProfile("required", "", nil)); err != nil {
		t.Fatalf("empty ubuntu_version must not error, got %v", err)
	}
}

func TestValidateCUDAOSTarget_Candidates(t *testing.T) {
	// At least one buildable candidate -> accept (the resolver will
	// pick it; 26.04/25.10 get rejected at resolve time).
	if err := validateCUDAOSTarget(cudaGuardProfile("optional", "", []string{"26.04", "25.10", "24.04"})); err != nil {
		t.Fatalf("candidate list containing 24.04 must be accepted, got %v", err)
	}
	// No buildable candidate -> reject now rather than at deploy time.
	if err := validateCUDAOSTarget(cudaGuardProfile("optional", "", []string{"26.04", "25.10"})); err == nil {
		t.Fatal("expected error when no candidate can build the CUDA module")
	}
}

func TestValidateCUDAOSTarget_OptOut(t *testing.T) {
	// Toolkit wanted for non-streaming reasons (ML / diagnostics).
	p := cudaGuardProfile("required", "25.10", nil)
	p.CUDA.AllowUnbuildableSunshineModule = true
	if err := validateCUDAOSTarget(p); err != nil {
		t.Fatalf("explicit opt-out must be honored, got %v", err)
	}
	if p.RequiresSunshineCUDAOS() {
		t.Error("RequiresSunshineCUDAOS must be false once opted out, or the OS resolver would still reject candidates")
	}
}

func TestRequiresSunshineCUDAOS(t *testing.T) {
	if p := cudaGuardProfile("required", "24.04", nil); !p.RequiresSunshineCUDAOS() {
		t.Error("cuda.mode=required without opt-out must constrain OS selection")
	}
	if p := cudaGuardProfile("none", "24.04", nil); p.RequiresSunshineCUDAOS() {
		t.Error("cuda.mode=none must not constrain OS selection")
	}
}
