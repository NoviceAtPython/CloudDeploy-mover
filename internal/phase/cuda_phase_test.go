package phase

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// cudaTestProfile returns a profile pre-set to mode=required + method,
// pointing at the HDR-4k120-cuda style fork pin so HDR validation
// passes.
func cudaTestProfile(method string, runfileURL string) *config.Profile {
	return &config.Profile{
		Profile: "hdr-4k120-cuda-test",
		NVIDIA:  config.NVIDIAConfig{DriverMajor: "580"},
		CUDA: config.CUDAConfig{
			Mode:          "required",
			Method:        method,
			RunfileURL:    runfileURL,
			RunfileSHA256: "",
		},
	}
}

// cudaPhaseDeps clones newDeps and wires a StatePath so persistence
// is real, plus a logger that emits to stderr for test debugging.
func cudaPhaseDeps(t *testing.T, p *config.Profile) *Deps {
	t.Helper()
	d := newDeps(t, p, nil)
	d.StatePath = filepath.Join(t.TempDir(), "state.json")
	d.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	return d
}

// stagedNvccProbe returns a probe function that yields the first
// release on call 1 and the second on call 2+, used to model
// "missing before install, present after".
func stagedNvccProbe(before, after cuda.NvccRelease) func(context.Context, *Deps) cuda.NvccRelease {
	calls := 0
	return func(context.Context, *Deps) cuda.NvccRelease {
		calls++
		if calls == 1 {
			return before
		}
		return after
	}
}

// -----------------------------------------------------------------------------
// already-installed short-circuit
// -----------------------------------------------------------------------------

func TestCudaPhase_AlreadyInstalledShortCircuits(t *testing.T) {
	deps := cudaPhaseDeps(t, cudaTestProfile("auto", "https://example/cuda/13.0.2/x.run"))
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")
	ph := Cuda{
		// nvcc returns 13.0; layout OK; profile pinned 13 via the
		// runfile URL ExpectedMajor parser.
		NvccProbeFn:                func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{Major: "13", Minor: "0"} },
		CudaLayoutOKFn:             func() bool { return true },
		ProfileSnippetPathOverride: snippet,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(CudaName).Details
	if d["source"] != "already-installed" {
		t.Errorf("source: got %v want already-installed", d["source"])
	}
	if d["nvcc_version"] != "13.0" {
		t.Errorf("nvcc_version: got %v want 13.0", d["nvcc_version"])
	}
	// Profile snippet must be written to the override path.
	body, err := os.ReadFile(snippet)
	if err != nil {
		t.Fatalf("read snippet: %v", err)
	}
	if !strings.Contains(string(body), "/usr/local/cuda/bin") {
		t.Errorf("snippet missing CUDA PATH export: %q", body)
	}
}

func TestCudaPhase_AlreadyInstalledWrongMajor_RequiredFails(t *testing.T) {
	deps := cudaPhaseDeps(t, cudaTestProfile("auto", "https://example/cuda/13.0.2/x.run"))
	tmpdir := t.TempDir()
	// nvcc 12.x present but profile expects CUDA 13. Short-circuit
	// must NOT fire; with no apt repo and a failing download, the
	// path errors out fatally.
	called := 0
	ph := Cuda{
		NvccProbeFn: func(context.Context, *Deps) cuda.NvccRelease {
			called++
			return cuda.NvccRelease{Major: "12", Minor: "4"}
		},
		CudaLayoutOKFn:         func() bool { return true },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(context.Context, *Deps, string, string) (int64, error) {
			return 0, errors.New("simulated network error")
		},
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
	if called == 0 {
		t.Errorf("NvccProbeFn should have been consulted")
	}
}

// -----------------------------------------------------------------------------
// mode=none / method=none skip cleanly
// -----------------------------------------------------------------------------

func TestCudaPhase_ModeNoneSkipsCleanly(t *testing.T) {
	p := cudaTestProfile("auto", "")
	p.CUDA.Mode = "none"
	deps := cudaPhaseDeps(t, p)
	if err := (Cuda{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
}

func TestCudaPhase_MethodNoneSkipsCleanly(t *testing.T) {
	p := cudaTestProfile("none", "")
	p.CUDA.Mode = "required" // mode says required but method=none disables
	deps := cudaPhaseDeps(t, p)
	if err := (Cuda{}).Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
}

// -----------------------------------------------------------------------------
// runfile path
// -----------------------------------------------------------------------------

func TestCudaPhase_RunfilePath_Required_NoURL_FailsFatal(t *testing.T) {
	p := cudaTestProfile("runfile", "")
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutOKFn: func() bool { return false },
		NvccProbeFn:    func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "runfile_url") {
		t.Errorf("error should name the missing knob: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_RunfilePath_Optional_NoURL_SkipsNonfatal(t *testing.T) {
	p := cudaTestProfile("runfile", "")
	p.CUDA.Mode = "optional"
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutOKFn: func() bool { return false },
		NvccProbeFn:    func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
}

func TestCudaPhase_RunfilePath_HappyPath(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	gotToolkitOnly := false
	ph := Cuda{
		CudaLayoutOKFn: func() bool { return true },
		// nvcc release missing pre-install, present post-install.
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			if err := os.WriteFile(dst, []byte("fake-runfile-bytes"), 0o644); err != nil {
				return 0, err
			}
			return cuda.MinRunfileBytes + 42, nil
		},
		RunfileCheckFn: func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn: func(_ context.Context, _ *Deps, runfile, tmp string) error {
			gotToolkitOnly = true
			_ = runfile
			_ = tmp
			return nil
		},
		ProfileSnippetPathOverride: snippet,
		RunfileTmpDirOverride:      tmpdir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	if !gotToolkitOnly {
		t.Errorf("RunfileInstallFn was not called; phase did not reach install step")
	}
	d := deps.State.Get(CudaName).Details
	if d["source"] != "runfile" {
		t.Errorf("source: got %v want runfile", d["source"])
	}
	if d["nvcc_version"] != "13.0" {
		t.Errorf("nvcc_version: got %v want 13.0", d["nvcc_version"])
	}
}

func TestCudaPhase_RunfilePath_RefusesSameSHARetry(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 5
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	checkCalls := 0
	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			// Always write the same bytes so the SHA matches across
			// attempts. The hash is computed by the phase from the
			// file contents.
			if err := os.WriteFile(dst, []byte("identical-corrupt-runfile-bytes"), 0o644); err != nil {
				return 0, err
			}
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn: func(context.Context, *Deps, string, string) error {
			checkCalls++
			return errors.New("simulated check failure: MD5 mismatch")
		},
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error for required + corrupt runfile")
	}
	if !strings.Contains(err.Error(), "retry refused") && !strings.Contains(err.Error(), "exhausted attempts") {
		t.Errorf("error should mention retry-refused or exhausted attempts; got: %v", err)
	}
	// RetryDecider should fire after the first failed --check.
	if checkCalls > 2 {
		t.Errorf("RetryDecider should have refused after attempt 1; got %d --check calls", checkCalls)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_RunfilePath_HTMLDisguise(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("<html>error 503</html>"), 0o644)
			return 1024, nil // way under MinRunfileBytes
		},
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error for HTML-disguise + required mode")
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_RunfilePath_SHAMismatch(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	// Operator pinned a SHA that won't match what we download.
	p.CUDA.RunfileSHA256 = "00000000000000000000000000000000000000000000000000000000abadcafe"
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("not-the-pinned-bytes"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error for SHA mismatch + required")
	}
	if !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "exhausted attempts") {
		t.Errorf("error should mention sha256/mismatch/exhausted; got %v", err)
	}
}

// -----------------------------------------------------------------------------
// apt path
// -----------------------------------------------------------------------------

func TestCudaPhase_AptPath_FallsBackToRunfileOnRequiredMissingCandidate(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("apt", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	probedNames := []string{}
	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return true },
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return true },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn: func(pkg string) bool {
			probedNames = append(probedNames, pkg)
			return false // no candidate available
		},
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:        func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn:      func(context.Context, *Deps, string, string) error { return nil },
		RunfileTmpDirOverride: tmpdir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(CudaName).Details
	if d["source"] != "runfile" {
		t.Errorf("source: got %v want runfile (fallback from apt)", d["source"])
	}
	// We must have skipped Ubuntu's nvidia-cuda-toolkit when expected=13.
	for _, n := range probedNames {
		if n == "nvidia-cuda-toolkit" {
			t.Errorf("apt candidate ladder must not probe nvidia-cuda-toolkit when CUDA 13 required; got %v", probedNames)
		}
	}
}

func TestCudaPhase_AptPath_RefusesDriverMetaPackage(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("apt", url)
	// Operator (or attacker) pinned a driver meta-package.
	p.CUDA.PackageName = "cuda-drivers"
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return true },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn:                func(string) bool { return true }, // claim it's installable
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected refusal to install cuda-drivers")
	}
	if !strings.Contains(err.Error(), "driver meta-package") {
		t.Errorf("error should explain the refusal: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_AptPath_MethodAptNoRepo_RequiredFails(t *testing.T) {
	p := cudaTestProfile("apt", "")
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error: method=apt + no repo + required")
	}
	if !strings.Contains(err.Error(), "apt repo") {
		t.Errorf("error should mention apt repo: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_AptPath_OptionalSkipsWhenRepoMissing(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.Mode = "optional"
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return false },
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
}

// -----------------------------------------------------------------------------
// verification edge cases
// -----------------------------------------------------------------------------

func TestCudaPhase_VerifyFailsWhenLayoutMissing(t *testing.T) {
	url := "https://example/cuda/13.0.2/x.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	calls := 0
	ph := Cuda{
		CudaLayoutOKFn: func() bool {
			calls++
			// Always false: pre-check (alreadyInstalledMatches) and
			// post-install verifyAndFinish both fail.
			return false
		},
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{Major: "13", Minor: "0"} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:        func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn:      func(context.Context, *Deps, string, string) error { return nil },
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal verification failure")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Errorf("error should mention verification: %v", err)
	}
	if calls < 2 {
		t.Errorf("CudaLayoutOKFn should have been called pre- and post-install; got %d calls", calls)
	}
}

func TestCudaPhase_VerifyFailsWhenMajorMismatch(t *testing.T) {
	url := "https://example/cuda/13.0.2/x.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return true },
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:        func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn:      func(context.Context, *Deps, string, string) error { return nil },
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal mismatch error")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch: %v", err)
	}
}

// -----------------------------------------------------------------------------
// state shape
// -----------------------------------------------------------------------------

func TestCudaPhase_PersistsDetailsOnSuccess(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	ph := Cuda{
		CudaLayoutOKFn:         func() bool { return true },
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 17, nil
		},
		RunfileCheckFn:             func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn:           func(context.Context, *Deps, string, string) error { return nil },
		ProfileSnippetPathOverride: snippet,
		RunfileTmpDirOverride:      tmpdir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	for _, k := range []string{"mode", "method", "source", "nvcc_version", "cuda_major", "expected_major", "verified_layout", "profile_snippet"} {
		if _, ok := d[k]; !ok {
			t.Errorf("details missing key %q: %+v", k, d)
		}
	}
	if d["expected_major"] != "13" {
		t.Errorf("expected_major: got %v want 13", d["expected_major"])
	}
	if d["profile_snippet"] != snippet {
		t.Errorf("profile_snippet: got %v want %s", d["profile_snippet"], snippet)
	}
	if d["mode"] != "required" {
		t.Errorf("mode: got %v want required", d["mode"])
	}
	if d["method"] != "runfile" {
		t.Errorf("method: got %v want runfile", d["method"])
	}
}
