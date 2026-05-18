package cuda

import "testing"

func TestParseNvccRelease(t *testing.T) {
	out := `nvcc: NVIDIA (R) Cuda compiler driver
Copyright (c) 2005-2024 NVIDIA Corporation
Built on Wed_Aug_14_10:10:22_PDT_2024
Cuda compilation tools, release 13.0, V13.0.88
Build cuda_13.0.r13.0/compiler.34714021_0
`
	got := ParseNvccRelease(out)
	if got.Major != "13" || got.Minor != "0" {
		t.Errorf("got %+v want major=13 minor=0", got)
	}
	if got.Full() != "13.0" {
		t.Errorf("Full(): got %q want 13.0", got.Full())
	}
}

func TestParseNvccRelease_Cuda12(t *testing.T) {
	out := "Cuda compilation tools, release 12.4, V12.4.131\n"
	got := ParseNvccRelease(out)
	if got.Major != "12" || got.Minor != "4" {
		t.Errorf("got %+v want major=12 minor=4", got)
	}
}

func TestParseNvccRelease_Unparseable(t *testing.T) {
	got := ParseNvccRelease("oh no the binary segfaulted\n")
	if got.Major != "" || got.Minor != "" {
		t.Errorf("got %+v want zero NvccRelease", got)
	}
	if got.Full() != "" {
		t.Errorf("Full() of zero release should be empty; got %q", got.Full())
	}
}

func TestExpectedMajor_FromPackage(t *testing.T) {
	cases := map[string]string{
		"cuda-toolkit-13-0":   "13",
		"cuda-toolkit-12-4":   "12",
		"cuda-toolkit-13":     "13",
		"cuda-toolkit":        "", // metapackage: unpinned
		"nvidia-cuda-toolkit": "", // ubuntu archive: unpinned
		"":                    "",
	}
	for in, want := range cases {
		if got := ExpectedMajor(in, ""); got != want {
			t.Errorf("ExpectedMajor(%q,\"\"): got %q want %q", in, got, want)
		}
	}
}

func TestExpectedMajor_FromRunfileURL(t *testing.T) {
	url := "https://developer.download.nvidia.com/compute/cuda/13.0.2/local_installers/cuda_13.0.2_580.95.05_linux.run"
	if got := ExpectedMajor("", url); got != "13" {
		t.Errorf("ExpectedMajor from runfile URL: got %q want 13", got)
	}
}

func TestExpectedMajor_PackageWinsOverURL(t *testing.T) {
	// A package_name pin should override a URL pin (the operator
	// explicitly wants this apt package version).
	if got := ExpectedMajor("cuda-toolkit-12-4", "https://example/cuda/13.0.2/x.run"); got != "12" {
		t.Errorf("package should win; got %q", got)
	}
}

func TestMajorMatches(t *testing.T) {
	r13 := NvccRelease{Major: "13", Minor: "0"}
	r12 := NvccRelease{Major: "12", Minor: "4"}
	if !MajorMatches(r13, "13") {
		t.Error("13 should match 13")
	}
	if MajorMatches(r12, "13") {
		t.Error("12 should NOT match 13")
	}
	// empty expected = any installed major satisfies, but a missing
	// installed major does not.
	if !MajorMatches(r13, "") {
		t.Error("any nvcc should satisfy empty expected")
	}
	if MajorMatches(NvccRelease{}, "") {
		t.Error("zero NvccRelease must not match empty expected")
	}
}

func TestProfileSnippetBody(t *testing.T) {
	body := ProfileSnippetBody()
	for _, want := range []string{"/usr/local/cuda/bin", "/usr/local/cuda/lib64", "clouddeployctl"} {
		if !contains(body, want) {
			t.Errorf("ProfileSnippetBody missing %q: %q", want, body)
		}
	}
}

// Tiny local contains to avoid importing strings just here.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}
func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
