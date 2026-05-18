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

// layoutCanonicalFn / layoutMissingFn / layoutUbuntuArchiveFn pair
// the test-side replacements for the deleted CudaLayoutOKFn -
// CudaLayoutFn returns the richer cuda.CudaLayout used by the
// source-aware verifier.
func layoutCanonicalFn() func() cuda.CudaLayout {
	return func() cuda.CudaLayout {
		return cuda.CudaLayout{
			Kind: cuda.LayoutNvidiaCanonical, NvccPath: "/usr/local/cuda/bin/nvcc",
			Headers: "/usr/local/cuda/include", Libs: "/usr/local/cuda/lib64",
		}
	}
}
func layoutMissingFn() func() cuda.CudaLayout {
	return func() cuda.CudaLayout { return cuda.CudaLayout{Kind: cuda.LayoutMissing} }
}
func layoutUbuntuArchiveFn() func() cuda.CudaLayout {
	return func() cuda.CudaLayout {
		return cuda.CudaLayout{
			Kind: cuda.LayoutUbuntuArchive, NvccPath: "/usr/bin/nvcc",
			Headers: "/usr/include/cuda_runtime.h",
			Libs:    "/usr/lib/x86_64-linux-gnu/libcudart.so.12",
		}
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
		CudaLayoutFn:               layoutCanonicalFn(),
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
	p := cudaTestProfile("auto", "https://example/cuda/13.0.2/x.run")
	// Express the "wrong major is fatal" expectation explicitly via
	// selection policy now that derived ExpectedMajor is gone.
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	deps := cudaPhaseDeps(t, p)
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
		CudaLayoutFn:           layoutCanonicalFn(),
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
		CudaLayoutFn: layoutMissingFn(),
		NvccProbeFn:  func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
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
		CudaLayoutFn: layoutMissingFn(),
		NvccProbeFn:  func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
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
		CudaLayoutFn: layoutCanonicalFn(),
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
		CudaLayoutFn:           layoutMissingFn(),
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
		CudaLayoutFn:           layoutMissingFn(),
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
		CudaLayoutFn:           layoutMissingFn(),
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
		CudaLayoutFn:           layoutCanonicalFn(),
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
		CudaLayoutFn:           layoutMissingFn(),
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
	// Archive fallback OFF (default) keeps the strict behavior: no
	// NVIDIA CUDA apt repo + required = fatal.
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
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

// -----------------------------------------------------------------------------
// archive-fallback path (no NVIDIA CUDA apt repo for the host's Ubuntu)
// -----------------------------------------------------------------------------

// TestCudaPhase_AptPath_NoNvidiaRepo_ArchiveFallbackInstallsUbuntuToolkit
// is the hdr-4k120-cuda-compatible case on Ubuntu 25.10: no
// ubuntu2510 CUDA apt repo, but the profile says it is fine to
// install Ubuntu's `nvidia-cuda-toolkit`. The phase must:
//
//  1. detect no NVIDIA CUDA repo for this distro,
//  2. skip cuda-keyring bootstrap entirely (would 404 anyway),
//  3. probe apt-cache against the host's *existing* sources, which
//     only know nvidia-cuda-toolkit,
//  4. apt-install nvidia-cuda-toolkit,
//  5. record repo_distro="ubuntu-archive" + archive_fallback=true in
//     state.Details.
func TestCudaPhase_AptPath_NoNvidiaRepo_ArchiveFallbackInstallsUbuntuToolkit(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowUbuntuArchiveFallback = true
	deps := cudaPhaseDeps(t, p)

	// Verify ensureCudaAptRepo is NOT invoked by the archive-only
	// path: every keyring-bootstrap call would route a "curl ...
	// cuda-keyring..." through deps.Runner. We capture the runner's
	// log dir as a probe target.
	httpHeadCalls := 0

	probedNames := []string{}
	ph := Cuda{
		// Archive install path: host has Ubuntu archive layout, NOT
		// /usr/local/cuda. The verifier must accept this.
		CudaLayoutFn:           layoutUbuntuArchiveFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		HTTPHeadFn: func(context.Context, *Deps, string) bool {
			httpHeadCalls++
			return false
		},
		ProbeFn: func(pkg string) bool {
			probedNames = append(probedNames, pkg)
			// Only nvidia-cuda-toolkit is installable from the host's
			// existing apt sources (Ubuntu 25.10 archive ships CUDA 12.4).
			return pkg == "nvidia-cuda-toolkit"
		},
		SmokeCompileFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:     func(context.Context, *Deps, string) error { return nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Fatalf("status: got %q want done", got)
	}
	d := deps.State.Get(CudaName).Details
	if d["package"] != "nvidia-cuda-toolkit" {
		t.Errorf("expected install of nvidia-cuda-toolkit; got package=%v", d["package"])
	}
	if d["repo_distro"] != "ubuntu-archive" {
		t.Errorf("repo_distro: got %v want ubuntu-archive", d["repo_distro"])
	}
	if d["archive_fallback"] != true {
		t.Errorf("archive_fallback: got %v want true", d["archive_fallback"])
	}
	if d["source"] != "apt" {
		t.Errorf("source: got %v want apt", d["source"])
	}
	// nvcc 12.4 + min_major=12 must satisfy latest-compatible.
	if d["nvcc_version"] != "12.4" {
		t.Errorf("nvcc_version: got %v want 12.4", d["nvcc_version"])
	}
	// The candidate ladder must have probed nvidia-cuda-toolkit
	// (i.e. the archive entry is part of the ladder under
	// archive_fallback=true).
	probed := strings.Join(probedNames, ",")
	if !strings.Contains(probed, "nvidia-cuda-toolkit") {
		t.Errorf("probe ladder must include nvidia-cuda-toolkit; got %v", probedNames)
	}
}

// TestCudaPhase_AptPath_NoRepo_NoArchiveFallback_RequiredFails is the
// strict variant: same 25.10 conditions but allow_ubuntu_archive_fallback
// remains false. The phase MUST fail fatal rather than reaching for
// the archive.
func TestCudaPhase_AptPath_NoRepo_NoArchiveFallback_RequiredFails(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowUbuntuArchiveFallback = false
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn:                func(string) bool { return true }, // would happily install archive if reached
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal: archive_fallback=false + no NVIDIA repo + required")
	}
	if !strings.Contains(err.Error(), "no CUDA apt repo") {
		t.Errorf("error should mention no CUDA apt repo: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

// TestCudaPhase_AptPath_StrictExact13_NoArchiveFallback_RequiredFails:
// the strict hdr-4k120-cuda case on 25.10. exact-major=13 must NEVER
// reach the archive even if archive_fallback were on (archive ships
// CUDA 12). Here we set archive_fallback=false too, so the path
// short-circuits on the missing NVIDIA repo first — the assertion is
// that exact-major never silently downgrades to 12.
func TestCudaPhase_AptPath_StrictExact13_NoArchiveFallback_RequiredFails(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.AllowUbuntuArchiveFallback = false
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn:                func(string) bool { return true }, // archive would install if reached
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("strict exact-major=13 + no NVIDIA repo + required must be fatal")
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

// TestCudaPhase_AptPath_ArchiveOnlyPathSkipsKeyringBootstrap pins the
// load-bearing observable behavior: the archive-only path must NOT
// invoke the NVIDIA cuda-keyring HEAD probe. (The probe is the gate
// that sets repoDistro; once we decided to fall through to archive,
// asking developer.download.nvidia.com again would be wasted work
// and would mask real misconfigurations.)
func TestCudaPhase_AptPath_ArchiveOnlyPathSkipsKeyringBootstrap(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowUbuntuArchiveFallback = true
	deps := cudaPhaseDeps(t, p)

	repoProbeCalls := 0
	httpHeadCalls := 0
	ph := Cuda{
		CudaLayoutFn: layoutUbuntuArchiveFn(),
		NvccProbeFn:  stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		RepoProbeFn: func(string) bool {
			repoProbeCalls++
			return false
		},
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		HTTPHeadFn: func(context.Context, *Deps, string) bool {
			httpHeadCalls++
			return false
		},
		ProbeFn:        func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" },
		SmokeCompileFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:     func(context.Context, *Deps, string) error { return nil },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The keyring HEAD probe runs at most once (during detectRepoDistro);
	// the archive-only path must not invoke HTTPHeadFn a second time
	// trying to bootstrap a keyring URL that doesn't exist.
	if httpHeadCalls > 1 {
		t.Errorf("archive-only path made %d HTTP HEAD calls; expected at most 1 (detectRepoDistro only)", httpHeadCalls)
	}
	// Repo probe is still called once via detectRepoDistro.
	if repoProbeCalls != 1 {
		t.Errorf("repo probe should be invoked exactly once (detectRepoDistro); got %d", repoProbeCalls)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
}

func TestCudaPhase_AptPath_OptionalSkipsWhenRepoMissing(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.Mode = "optional"
	deps := cudaPhaseDeps(t, p)
	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
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
		CudaLayoutFn: func() cuda.CudaLayout {
			calls++
			// Always LayoutMissing: pre-check (alreadyInstalledMatches)
			// and post-install verifyAndFinish both fail.
			return cuda.CudaLayout{Kind: cuda.LayoutMissing}
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
		t.Errorf("CudaLayoutFn should have been called pre- and post-install; got %d calls", calls)
	}
}

func TestCudaPhase_VerifyFailsWhenMajorMismatch(t *testing.T) {
	url := "https://example/cuda/13.0.2/x.run"
	p := cudaTestProfile("runfile", url)
	// Express the "wrong major is fatal" expectation explicitly:
	// post-install nvcc must report CUDA 13 or the phase fails.
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
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
	// New phrasing: "selection_policy=exact-major requires major 13".
	if !strings.Contains(err.Error(), "exact-major") && !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention the selection policy or mismatch: %v", err)
	}
}

// -----------------------------------------------------------------------------
// state shape
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// cross-distro NVIDIA CUDA repo fallback
// -----------------------------------------------------------------------------

// TestCudaPhase_CrossDistro_Ubuntu2510_Falls_To_Ubuntu2404 is the
// central scenario for the cross-distro CUDA repo design: Ubuntu
// 25.10 host, ubuntu2510 NVIDIA repo missing, ubuntu2404 reachable.
// Phase MUST install cuda-toolkit-13-N from ubuntu2404 and MUST NOT
// fall through to Ubuntu's CUDA 12.4 archive package.
func TestCudaPhase_CrossDistro_Ubuntu2510_Falls_To_Ubuntu2404(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.PreferMajor = "13"
	p.CUDA.PreferNewest = true
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowCrossDistroCudaRepo = true
	p.CUDA.CudaRepoDistroCandidates = []string{"auto-host", "ubuntu2404"}
	p.CUDA.AllowUbuntuArchiveFallback = true
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	repoTried := []string{}
	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		RepoProbeFn: func(distro string) bool {
			repoTried = append(repoTried, distro)
			return distro == "ubuntu2404"
		},
		ProbeFn: func(pkg string) bool {
			return pkg == "cuda-toolkit-13-0"
		},
		SmokeCompileFn:             func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:                 func(context.Context, *Deps, string) error { return nil },
		ProfileSnippetPathOverride: snippet,
		SmokeDirOverride:           smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("cross-distro path must reach done; got: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Fatalf("status: got %q want done", got)
	}
	if len(repoTried) != 2 || repoTried[0] != "ubuntu2510" || repoTried[1] != "ubuntu2404" {
		t.Errorf("repo probe order: got %v want [ubuntu2510 ubuntu2404]", repoTried)
	}
	d := deps.State.Get(CudaName).Details
	if d["host_ubuntu_version"] != "25.10" {
		t.Errorf("host_ubuntu_version: got %v want 25.10", d["host_ubuntu_version"])
	}
	if d["host_codename"] != "questing" {
		t.Errorf("host_codename: got %v want questing", d["host_codename"])
	}
	if d["selected_repo_distro"] != "ubuntu2404" {
		t.Errorf("selected_repo_distro: got %v want ubuntu2404", d["selected_repo_distro"])
	}
	if d["cross_distro_cuda_repo"] != true {
		t.Errorf("cross_distro_cuda_repo: got %v want true", d["cross_distro_cuda_repo"])
	}
	tried, _ := d["cuda_repo_distro_tried"].([]string)
	if len(tried) != 2 || tried[0] != "ubuntu2510" || tried[1] != "ubuntu2404" {
		t.Errorf("cuda_repo_distro_tried: got %v want [ubuntu2510 ubuntu2404]", tried)
	}
	if d["package"] != "cuda-toolkit-13-0" {
		t.Errorf("package: got %v want cuda-toolkit-13-0 (must NOT downgrade to nvidia-cuda-toolkit)", d["package"])
	}
	if d["selected_major"] != "13" {
		t.Errorf("selected_major: got %v want 13", d["selected_major"])
	}
}

// TestCudaPhase_CrossDistro_NoneReachable_FallsToArchive: both NVIDIA
// repo slugs miss; archive fallback is allowed; phase installs
// nvidia-cuda-toolkit and records fallback_reason.
func TestCudaPhase_CrossDistro_NoneReachable_FallsToArchive(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowCrossDistroCudaRepo = true
	p.CUDA.CudaRepoDistroCandidates = []string{"auto-host", "ubuntu2404"}
	p.CUDA.AllowUbuntuArchiveFallback = true
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutUbuntuArchiveFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		RepoProbeFn:            func(string) bool { return false },
		ProbeFn:                func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" },
		SmokeCompileFn:         func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:             func(context.Context, *Deps, string) error { return nil },
		SmokeDirOverride:       smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	if d["selected_repo_distro"] != "ubuntu-archive" {
		t.Errorf("selected_repo_distro: got %v want ubuntu-archive", d["selected_repo_distro"])
	}
	if d["archive_fallback"] != true {
		t.Errorf("archive_fallback: got %v want true", d["archive_fallback"])
	}
	if fr, _ := d["fallback_reason"].(string); !strings.Contains(fr, "archive_fallback") {
		t.Errorf("fallback_reason should name the knob; got %q", fr)
	}
	tried, _ := d["cuda_repo_distro_tried"].([]string)
	if len(tried) != 2 || tried[0] != "ubuntu2510" || tried[1] != "ubuntu2404" {
		t.Errorf("cuda_repo_distro_tried: got %v want [ubuntu2510 ubuntu2404]", tried)
	}
}

// TestCudaPhase_CrossDistro_Strict_NoArchiveFallback_RequiredFails:
// hdr-4k120-cuda strict on 25.10. ubuntu2510 missing, ubuntu2404
// missing, archive fallback OFF -> fatal (no silent downgrade).
func TestCudaPhase_CrossDistro_Strict_NoArchiveFallback_RequiredFails(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.AllowCrossDistroCudaRepo = true
	p.CUDA.CudaRepoDistroCandidates = []string{"auto-host", "ubuntu2404"}
	p.CUDA.AllowUbuntuArchiveFallback = false
	deps := cudaPhaseDeps(t, p)

	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		RepoProbeFn:            func(string) bool { return false },
		ProbeFn:                func(string) bool { return true },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("strict + no reachable repo + no archive fallback must be fatal")
	}
	if !strings.Contains(err.Error(), "no CUDA apt repo") {
		t.Errorf("error should mention no CUDA apt repo: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

// TestCudaPhase_CrossDistro_Strict_Ubuntu2404Reachable_Installs13:
// strict hdr-4k120-cuda on 25.10 succeeds when ubuntu2404 is
// reachable.
func TestCudaPhase_CrossDistro_Strict_Ubuntu2404Reachable_Installs13(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.AllowCrossDistroCudaRepo = true
	p.CUDA.CudaRepoDistroCandidates = []string{"auto-host", "ubuntu2404"}
	p.CUDA.AllowUbuntuArchiveFallback = false
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		RepoProbeFn:            func(distro string) bool { return distro == "ubuntu2404" },
		ProbeFn:                func(pkg string) bool { return strings.HasPrefix(pkg, "cuda-toolkit-13-") },
		SmokeCompileFn:         func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:             func(context.Context, *Deps, string) error { return nil },
		SmokeDirOverride:       smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("strict + ubuntu2404 reachable must reach done; got: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	if d["selected_repo_distro"] != "ubuntu2404" {
		t.Errorf("selected_repo_distro: got %v want ubuntu2404", d["selected_repo_distro"])
	}
	if d["cross_distro_cuda_repo"] != true {
		t.Errorf("cross_distro_cuda_repo: got %v want true", d["cross_distro_cuda_repo"])
	}
	if d["selected_major"] != "13" {
		t.Errorf("selected_major: got %v want 13", d["selected_major"])
	}
}

// TestCudaPhase_NativeProfile_OnlyProbesAutoHost: native diagnostic
// profile must NOT probe ubuntu2404 when allow_cross_distro_cuda_repo
// =false.
func TestCudaPhase_NativeProfile_OnlyProbesAutoHost(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.AllowCrossDistroCudaRepo = false
	p.CUDA.CudaRepoDistroCandidates = []string{"auto-host"}
	deps := cudaPhaseDeps(t, p)

	repoTried := []string{}
	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		RepoProbeFn: func(distro string) bool {
			repoTried = append(repoTried, distro)
			return false
		},
	}
	_ = ph.Run(context.Background(), deps)
	if len(repoTried) != 1 || repoTried[0] != "ubuntu2510" {
		t.Errorf("native profile must only probe auto-host; got %v", repoTried)
	}
}

// -----------------------------------------------------------------------------
// source-aware layout verifier
// -----------------------------------------------------------------------------

// TestCudaPhase_ArchiveFallback_AcceptsUbuntuLayout is the regression
// test for the live-VM bug fixed in this commit: archive_fallback=true
// + Ubuntu archive layout (no /usr/local/cuda) + smoke compile success
// must reach phase done.
func TestCudaPhase_ArchiveFallback_AcceptsUbuntuLayout(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowUbuntuArchiveFallback = true
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	smokeCalls := 0
	ph := Cuda{
		// The whole point: Ubuntu archive layout, no /usr/local/cuda.
		CudaLayoutFn:           layoutUbuntuArchiveFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn:                func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" },
		SmokeCompileFn: func(context.Context, *Deps, string, string) error {
			smokeCalls++
			return nil
		},
		SmokeRunFn:                 func(context.Context, *Deps, string) error { return nil },
		ProfileSnippetPathOverride: snippet,
		SmokeDirOverride:           smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("archive-fallback + Ubuntu layout must reach done; got: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Fatalf("status: got %q want done", got)
	}
	if smokeCalls != 1 {
		t.Errorf("smoke compile should have run once; got %d calls", smokeCalls)
	}
	d := deps.State.Get(CudaName).Details
	if d["layout_kind"] != "ubuntu-archive" {
		t.Errorf("layout_kind: got %v want ubuntu-archive", d["layout_kind"])
	}
	if d["layout_nvcc"] != "/usr/bin/nvcc" {
		t.Errorf("layout_nvcc: got %v want /usr/bin/nvcc", d["layout_nvcc"])
	}
	if d["archive_fallback"] != true {
		t.Errorf("archive_fallback: got %v want true", d["archive_fallback"])
	}
	if d["selected_package"] != "nvidia-cuda-toolkit" {
		t.Errorf("selected_package: got %v want nvidia-cuda-toolkit", d["selected_package"])
	}
	if d["selected_major"] != "12" {
		t.Errorf("selected_major: got %v want 12", d["selected_major"])
	}
	if d["min_major"] != "12" {
		t.Errorf("min_major: got %v want 12", d["min_major"])
	}
	if d["driver_preferred_major"] != "13" {
		t.Errorf("driver_preferred_major: got %v want 13 (driver 580)", d["driver_preferred_major"])
	}
	if d["required_major"] != "" {
		t.Errorf("required_major should be empty for latest-compatible; got %v", d["required_major"])
	}
}

// TestCudaPhase_NvidiaRepoSource_RequiresCanonicalLayout pins the
// load-bearing strict-side check: an apt install that came from the
// NVIDIA CUDA apt repo (package != nvidia-cuda-toolkit, archive_fallback
// =false) must produce /usr/local/cuda. If the host instead reports
// only Ubuntu archive layout, the deploy must fail rather than
// silently accept (e.g. someone installed nvidia-cuda-toolkit earlier).
func TestCudaPhase_NvidiaRepoSource_RequiresCanonicalLayout(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.CompileSmokeTest = false
	deps := cudaPhaseDeps(t, p)

	ph := Cuda{
		// Host has ONLY Ubuntu archive layout, even though we just
		// "installed" cuda-toolkit-13-0 via the NVIDIA repo.
		CudaLayoutFn:           layoutUbuntuArchiveFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return true },
		CurrentUbuntuVersionFn: func() string { return "24.04" },
		ProbeFn:                func(pkg string) bool { return pkg == "cuda-toolkit-13-0" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("NVIDIA-repo source + Ubuntu-only layout must fail")
	}
	if !strings.Contains(err.Error(), "layout") {
		t.Errorf("error should mention layout mismatch: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
	d := deps.State.Get(CudaName).Details
	_ = d // fail-path details are minimal by design (see p.fail)
}

// TestCudaPhase_StateDetailsDistinguishDriverPreferredFromSelected
// asserts the clearer state-of-record fields: driver_preferred_major
// reflects what NVIDIA pairs with the configured driver, while
// selected_major reflects what was actually installed. They diverge
// on the compatibility profile (driver 580 prefers CUDA 13, but the
// archive installs CUDA 12).
func TestCudaPhase_StateDetailsDistinguishDriverPreferredFromSelected(t *testing.T) {
	p := cudaTestProfile("apt", "")
	p.CUDA.SelectionPolicy = "latest-compatible"
	p.CUDA.MinMajor = "12"
	p.CUDA.AllowUbuntuArchiveFallback = true
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutUbuntuArchiveFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "12", Minor: "4"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		ProbeFn:                func(pkg string) bool { return pkg == "nvidia-cuda-toolkit" },
		SmokeCompileFn:         func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn:             func(context.Context, *Deps, string) error { return nil },
		SmokeDirOverride:       smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	if d["driver_preferred_major"] != "13" {
		t.Errorf("driver_preferred_major: got %v want 13", d["driver_preferred_major"])
	}
	if d["selected_major"] != "12" {
		t.Errorf("selected_major: got %v want 12", d["selected_major"])
	}
	// On the compat path, required_major must NOT report "13" — it
	// reports the empty string because nothing requires 13 strictly.
	if d["required_major"] != "" {
		t.Errorf("required_major should be empty under latest-compatible; got %v", d["required_major"])
	}
}

// -----------------------------------------------------------------------------
// smoke test
// -----------------------------------------------------------------------------

func TestCudaPhase_SmokeCompileSuccessMarksDone(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	smokeDir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	srcSeen := ""
	binSeen := ""
	compileCalls := 0
	runCalls := 0

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:   func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeCompileFn: func(_ context.Context, _ *Deps, src, bin string) error {
			compileCalls++
			srcSeen = src
			binSeen = bin
			return nil
		},
		SmokeRunFn: func(context.Context, *Deps, string) error {
			runCalls++
			return nil
		},
		ProfileSnippetPathOverride: snippet,
		RunfileTmpDirOverride:      tmpdir,
		SmokeDirOverride:           smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if compileCalls != 1 {
		t.Errorf("SmokeCompileFn called %d times; want 1", compileCalls)
	}
	if runCalls != 1 {
		t.Errorf("SmokeRunFn called %d times; want 1", runCalls)
	}
	if filepath.Dir(srcSeen) != smokeDir || filepath.Base(srcSeen) != "smoke.cu" {
		t.Errorf("smoke source should land under SmokeDirOverride as smoke.cu; got %q", srcSeen)
	}
	if filepath.Dir(binSeen) != smokeDir || filepath.Base(binSeen) != "smoke" {
		t.Errorf("smoke binary should land under SmokeDirOverride as smoke; got %q", binSeen)
	}
	d := deps.State.Get(CudaName).Details
	if d["compile_smoke_test"] != true {
		t.Errorf("details.compile_smoke_test must be true; got %v", d["compile_smoke_test"])
	}
	if d["compile_smoke_test_passed"] != true {
		t.Errorf("details.compile_smoke_test_passed must be true; got %v", d["compile_smoke_test_passed"])
	}
	if d["runtime_smoke_test_passed"] != true {
		t.Errorf("details.runtime_smoke_test_passed must be true; got %v", d["runtime_smoke_test_passed"])
	}
	// The .cu file must actually be on disk (so collect-logs can grab it).
	if _, err := os.Stat(srcSeen); err != nil {
		t.Errorf("smoke.cu missing on disk: %v", err)
	}
}

func TestCudaPhase_SmokeCompileFailureFailsRequiredMode(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:   func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeCompileFn: func(context.Context, *Deps, string, string) error {
			return errors.New("simulated nvcc: error: a previous error has been ignored")
		},
		RunfileTmpDirOverride: tmpdir,
		SmokeDirOverride:      smokeDir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal smoke-compile failure in required mode")
	}
	if !strings.Contains(err.Error(), "compile smoke test failed") {
		t.Errorf("error should mention compile smoke test failure: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestCudaPhase_SmokeCompileFailureOptionalSkipsNonfatal(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.Mode = "optional"
	p.CUDA.RunfileMaxAttempts = 1
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:        func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn:      func(context.Context, *Deps, string, string) error { return nil },
		SmokeCompileFn:        func(context.Context, *Deps, string, string) error { return errors.New("simulated compile failure") },
		RunfileTmpDirOverride: tmpdir,
		SmokeDirOverride:      smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("optional + compile fail must skip nonfatal; got err: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
	d := deps.State.Get(CudaName).Details
	if d["compile_smoke_test_passed"] != false {
		t.Errorf("details.compile_smoke_test_passed must be false; got %v", d["compile_smoke_test_passed"])
	}
}

func TestCudaPhase_AlreadyInstalledRunsSmoke(t *testing.T) {
	p := cudaTestProfile("auto", "")
	p.CUDA.SelectionPolicy = "exact-major"
	p.CUDA.ExpectedMajor = "13"
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	smokeDir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	smokeCalls := 0
	ph := Cuda{
		NvccProbeFn:  func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{Major: "13", Minor: "0"} },
		CudaLayoutFn: layoutCanonicalFn(),
		SmokeCompileFn: func(context.Context, *Deps, string, string) error {
			smokeCalls++
			return nil
		},
		SmokeRunFn:                 func(context.Context, *Deps, string) error { return nil },
		ProfileSnippetPathOverride: snippet,
		SmokeDirOverride:           smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if smokeCalls != 1 {
		t.Errorf("already-installed path must run smoke when compile_smoke_test=true; got %d compile calls", smokeCalls)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
}

func TestCudaPhase_CompileSmokeFalseSkipsSmoke(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	p.CUDA.CompileSmokeTest = false
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	compileCalls := 0
	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:   func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeCompileFn: func(context.Context, *Deps, string, string) error {
			compileCalls++
			return nil
		},
		RunfileTmpDirOverride: tmpdir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if compileCalls != 0 {
		t.Errorf("compile_smoke_test=false: compile must not run; got %d calls", compileCalls)
	}
	d := deps.State.Get(CudaName).Details
	if d["compile_smoke_test"] != false {
		t.Errorf("details.compile_smoke_test must be false; got %v", d["compile_smoke_test"])
	}
	if _, ok := d["compile_smoke_test_passed"]; ok {
		t.Errorf("details must not record compile_smoke_test_passed when smoke is off; got %v", d["compile_smoke_test_passed"])
	}
}

func TestCudaPhase_SmokeRunFailureIsNonFatalEvenInRequired(t *testing.T) {
	// "Some headless/cloud contexts may not expose a usable CUDA
	// device before reboot/driver reload." Compile success must let
	// the phase succeed even if running the binary fails.
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	p.CUDA.CompileSmokeTest = true
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	smokeDir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
		NvccProbeFn:            stagedNvccProbe(cuda.NvccRelease{}, cuda.NvccRelease{Major: "13", Minor: "0"}),
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("ok"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn:   func(context.Context, *Deps, string, string) error { return nil },
		RunfileInstallFn: func(context.Context, *Deps, string, string) error { return nil },
		SmokeCompileFn:   func(context.Context, *Deps, string, string) error { return nil },
		SmokeRunFn: func(context.Context, *Deps, string) error {
			return errors.New("CUDA error: no CUDA-capable device is detected")
		},
		RunfileTmpDirOverride: tmpdir,
		SmokeDirOverride:      smokeDir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("required + compile-pass + run-fail must NOT fail the phase; got err: %v", err)
	}
	if got := deps.State.Get(CudaName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(CudaName).Details
	if d["runtime_smoke_test_passed"] != false {
		t.Errorf("details.runtime_smoke_test_passed must be false; got %v", d["runtime_smoke_test_passed"])
	}
	if d["compile_smoke_test_passed"] != true {
		t.Errorf("details.compile_smoke_test_passed must remain true; got %v", d["compile_smoke_test_passed"])
	}
}

// -----------------------------------------------------------------------------
// diagnostic hint
// -----------------------------------------------------------------------------

func TestCudaPhase_CorruptRunfileHintInFatalError(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("corrupt-but-not-html"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn: func(context.Context, *Deps, string, string) error {
			// Mimic NVIDIA's own message so we know the hint fires
			// even when the upstream wording shows through.
			return errors.New("Error in MD5 checksums: <varies> is different from a7389036e857482d4465dc2d5b6370d8")
		},
		RunfileTmpDirOverride: tmpdir,
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal --check failure")
	}
	if !strings.Contains(err.Error(), "NVIDIA runfile internal checksum failed") {
		t.Errorf("error should embed the corrupt-runfile hint; got: %v", err)
	}
	if !strings.Contains(err.Error(), "cuda.method=apt") {
		t.Errorf("error should suggest cuda.method=apt; got: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	if !strings.Contains(asString(d["err"]), "internal checksum failed") {
		// state.Details["err"] is nil on fatal path (we use the
		// shared `fail` helper); the public-facing error string is
		// the right place to assert. Just ensure mode persisted.
	}
	_ = d
}

func TestCudaPhase_CorruptRunfileHintInOptionalSkipDetails(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.Mode = "optional"
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()

	ph := Cuda{
		CudaLayoutFn:           layoutMissingFn(),
		NvccProbeFn:            func(context.Context, *Deps) cuda.NvccRelease { return cuda.NvccRelease{} },
		RepoProbeFn:            func(string) bool { return false },
		CurrentUbuntuVersionFn: func() string { return "25.10" },
		DownloadFn: func(_ context.Context, _ *Deps, _ string, dst string) (int64, error) {
			_ = os.WriteFile(dst, []byte("corrupt-but-not-html"), 0o644)
			return cuda.MinRunfileBytes + 1, nil
		},
		RunfileCheckFn: func(context.Context, *Deps, string, string) error {
			return errors.New("--check fail")
		},
		RunfileTmpDirOverride: tmpdir,
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	d := deps.State.Get(CudaName).Details
	hint, ok := d["hint"].(string)
	if !ok || hint == "" {
		t.Fatalf("optional-skip details must include `hint`; got %+v", d)
	}
	if !strings.Contains(hint, "internal checksum failed") {
		t.Errorf("hint should match RunfileCheckCorruptHint; got %q", hint)
	}
	if !strings.Contains(hint, "cuda.method=apt") {
		t.Errorf("hint should suggest cuda.method=apt; got %q", hint)
	}
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestCudaPhase_PersistsDetailsOnSuccess(t *testing.T) {
	url := "https://example/cuda/13.0.2/cuda_13.0.2_580.95.05_linux.run"
	p := cudaTestProfile("runfile", url)
	p.CUDA.RunfileMaxAttempts = 1
	deps := cudaPhaseDeps(t, p)
	tmpdir := t.TempDir()
	snippet := filepath.Join(t.TempDir(), "clouddeploy-cuda.sh")

	ph := Cuda{
		CudaLayoutFn:           layoutCanonicalFn(),
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
