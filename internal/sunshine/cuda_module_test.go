package sunshine

import "testing"

// The Ubuntu releases where Sunshine's CUDA capture module cannot be
// compiled. This mirrors resolve_sunshine_cuda_module() in
// CloudDeploy-wayland.sh; if that function changes, this test is the
// tripwire that says "update the Go side too".
func TestCUDAModuleSupported(t *testing.T) {
	cases := map[string]bool{
		"22.04": true,
		"24.04": true,
		"25.04": true,
		"25.10": false,
		"26.04": false,
		"26.10": false,
		// Unknown/empty must not silently reject: callers that do not
		// know the target OS yet still need candidates to survive.
		"":       true,
		"99.99":  true,
		" 25.10": false, // whitespace tolerated
	}
	for ver, want := range cases {
		if got := CUDAModuleSupported(ver); got != want {
			t.Errorf("CUDAModuleSupported(%q) = %v, want %v", ver, got, want)
		}
	}
}

func TestCUDAModuleUnsupportedUbuntuVersionsIsSortedAndComplete(t *testing.T) {
	got := CUDAModuleUnsupportedUbuntuVersions()
	want := []string{"25.10", "26.04", "26.10"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	}
}
