package phase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// CudaName is the canonical state-key.
const CudaName = "cuda"

// RunfileCheckCorruptHint is emitted whenever the NVIDIA runfile's
// `--check` step fails. See the original commit (a58aa34) for the
// background on the corrupt CUDA 13.0.2 toolkit-only runfile.
const RunfileCheckCorruptHint = "NVIDIA runfile internal checksum failed; artifact/cache likely corrupt. " +
	"Re-downloading the same SHA cannot help. " +
	"Try cuda.method=apt (the official NVIDIA CUDA apt repo is the recommended path), " +
	"or pin a different cuda.runfile_url (a different CUDA 13.x version) AND set cuda.runfile_sha256 " +
	"to a value that passes `sh cuda_*_linux.run --check`."

// CudaSmokeProgram is the trivial .cu file the phase compiles after
// install to prove that nvcc actually works against the installed
// toolkit headers + libraries. Running the resulting binary is
// best-effort (cloud images may not expose a usable CUDA device pre-
// reboot); we only fail required mode on compile failure.
const CudaSmokeProgram = `// clouddeployctl cuda smoke test: prove nvcc + toolkit headers/libs work.
#include <cuda_runtime.h>
__global__ void k() {}
int main(void) {
    k<<<1, 1>>>();
    cudaError_t err = cudaDeviceSynchronize();
    // cudaSuccess: a GPU is present and the kernel ran.
    // cudaErrorNoDevice: toolkit OK; no CUDA device wired up in this
    //                    cloud image yet (driver not fully loaded, or
    //                    the VM is headless).
    return (err == cudaSuccess || err == cudaErrorNoDevice) ? 0 : 1;
}
`

// CudaSmokeDir is the default staging dir for the smoke test.
const CudaSmokeDir = "/var/tmp/clouddeploy-cuda-smoke"

// Cuda phase: real v2-equivalent CUDA toolkit install. Honors:
//
//   - cuda.Mode    (none / optional / required)
//   - cuda.Method  (auto / apt / runfile / none)
//   - cuda.SelectionPolicy (latest-compatible / exact-major /
//     min-major / any), cuda.ExpectedMajor, cuda.MinMajor
//   - cuda.AllowUbuntuArchiveFallback
//   - cuda.CompileSmokeTest
//
// Writes /etc/profile.d/clouddeploy-cuda.sh on success. Records source
// (apt / runfile / already-installed), package, nvcc version, smoke
// test outcome in state.Details.
type Cuda struct {
	ProbeFn                cuda.AvailabilityProbe
	RepoProbeFn            cuda.RepoAvailabilityProbe
	CurrentUbuntuVersionFn func() string
	NvccProbeFn            func(ctx context.Context, deps *Deps) cuda.NvccRelease
	CudaLayoutOKFn         func() bool
	HTTPHeadFn             func(ctx context.Context, deps *Deps, url string) bool
	DownloadFn             func(ctx context.Context, deps *Deps, url, dst string) (size int64, err error)
	RunfileCheckFn         func(ctx context.Context, deps *Deps, runfile, tmpdir string) error
	RunfileInstallFn       func(ctx context.Context, deps *Deps, runfile, tmpdir string) error

	// SmokeCompileFn lets tests inject the `nvcc -o bin src.cu` step.
	// Receives the path to the staged .cu and the desired binary path.
	// nil = run nvcc via the runner.
	SmokeCompileFn func(ctx context.Context, deps *Deps, src, bin string) error

	// SmokeRunFn lets tests inject the "execute the smoke binary"
	// step. nil = run the binary via the runner with a short timeout.
	SmokeRunFn func(ctx context.Context, deps *Deps, bin string) error

	ProfileSnippetPathOverride string
	RunfileTmpDirOverride      string

	// SmokeDirOverride lets tests redirect the smoke staging dir to a
	// t.TempDir(). Empty = CudaSmokeDir.
	SmokeDirOverride string
}

// Name implements Phase.
func (Cuda) Name() string { return CudaName }

// Run implements Phase.
func (p Cuda) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, CudaName) {
		log.Info("phase cuda: already terminal; skipping")
		return nil
	}

	deps.State.MarkRunning(CudaName)
	_ = deps.PersistState()

	mode := cuda.ModeNone
	method := cuda.MethodAuto
	explicitName := ""
	driverMajor := ""
	runfileURL := ""
	runfileSHA := ""
	runfileMaxAttempts := 0
	var selection cuda.Selection
	compileSmokeTest := false
	if deps.Profile != nil {
		m, err := cuda.ParseMode(deps.Profile.CUDA.Mode)
		if err != nil {
			deps.State.MarkFailed(CudaName, "parse cuda.mode", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda: %w", err)
		}
		mode = m
		mt, err := cuda.ParseMethod(deps.Profile.CUDA.Method)
		if err != nil {
			deps.State.MarkFailed(CudaName, "parse cuda.method", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda: %w", err)
		}
		method = mt
		pol, err := cuda.ParseSelectionPolicy(deps.Profile.CUDA.SelectionPolicy)
		if err != nil {
			deps.State.MarkFailed(CudaName, "parse cuda.selection_policy", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase cuda: %w", err)
		}
		explicitName = deps.Profile.CUDA.PackageName
		driverMajor = deps.Profile.NVIDIA.DriverMajor
		runfileURL = deps.Profile.CUDA.RunfileURL
		runfileSHA = deps.Profile.CUDA.RunfileSHA256
		runfileMaxAttempts = deps.Profile.CUDA.RunfileMaxAttempts
		selection = cuda.Selection{
			DriverPreferredMajor:       cuda.PreferredMajorForDriver(driverMajor),
			Policy:                     pol,
			ExpectedMajor:              strings.TrimSpace(deps.Profile.CUDA.ExpectedMajor),
			MinMajor:                   strings.TrimSpace(deps.Profile.CUDA.MinMajor),
			AllowUbuntuArchiveFallback: deps.Profile.CUDA.AllowUbuntuArchiveFallback,
		}
		// Default CompileSmokeTest:
		//   - true  when cuda.compile_smoke_test is explicitly set true.
		//   - false otherwise. (We default to false so existing
		//     mode=optional / mode=none profiles do not suddenly fail
		//     in environments without a C compiler.) The strict
		//     hdr-4k120-cuda profile sets it true explicitly.
		compileSmokeTest = deps.Profile.CUDA.CompileSmokeTest
	}
	plan := cuda.Plan(mode)

	// mode=none / method=none: skip cleanly.
	if !plan.WillAttemptInstall || method == cuda.MethodNone {
		deps.State.MarkSkipped(CudaName, plan.Rationale)
		deps.State.Get(CudaName).Details = map[string]any{
			"mode":      string(plan.Mode),
			"method":    string(method),
			"rationale": plan.Rationale,
		}
		_ = deps.PersistState()
		log.Info("phase cuda: skipped", "mode", string(plan.Mode), "method", string(method))
		return nil
	}

	// Short-circuit: working install already on disk that satisfies
	// the configured selection policy. Still run the smoke test (when
	// CompileSmokeTest=true) so a stale-headers regression on a
	// previously-passing host surfaces during apply.
	if rel, ok := p.alreadyInstalledMatches(ctx, deps, selection); ok {
		baseDetails := map[string]any{
			"mode":             string(plan.Mode),
			"method":           string(method),
			"selection_policy": string(selection.Policy),
			"source":           "already-installed",
			"nvcc_version":     rel.Full(),
			"cuda_major":       rel.Major,
			"expected_major":   selection.EffectivePinnedMajor(),
			"verified_layout":  true,
		}
		if err := p.runSmokeIfRequested(ctx, deps, compileSmokeTest, baseDetails, log); err != nil {
			if plan.FailsDeployOnError {
				return p.fail(deps, plan, "compile smoke test failed (already-installed path)", err)
			}
			return p.skipNonfatal(deps, plan, "compile smoke test failed (already-installed); mode=optional", baseDetails)
		}
		if err := p.writeProfileSnippet(); err != nil {
			log.Warn("phase cuda: failed to write profile.d snippet", "err", err)
		}
		baseDetails["profile_snippet"] = p.profileSnippetPath()
		deps.State.MarkDone(CudaName, baseDetails)
		_ = deps.PersistState()
		log.Info("phase cuda: nvcc already installed and policy-satisfied",
			"nvcc", rel.Full(), "policy", string(selection.Policy))
		return nil
	}

	// Resolve method=auto -> apt or runfile.
	effectiveMethod := method
	repoDistro := ""
	if effectiveMethod == cuda.MethodAuto || effectiveMethod == cuda.MethodApt {
		repoDistro = p.detectRepoDistro(ctx, deps)
	}
	if effectiveMethod == cuda.MethodAuto {
		if repoDistro != "" {
			effectiveMethod = cuda.MethodApt
		} else {
			effectiveMethod = cuda.MethodRunfile
		}
	}

	switch effectiveMethod {
	case cuda.MethodApt:
		return p.runAptPath(ctx, deps, plan, repoDistro, driverMajor, explicitName, selection, compileSmokeTest, log)
	case cuda.MethodRunfile:
		return p.runRunfilePath(ctx, deps, plan, runfileURL, runfileSHA, runfileMaxAttempts, selection, compileSmokeTest, log)
	}
	err := fmt.Errorf("unknown effective method %q", effectiveMethod)
	return p.fail(deps, plan, "method dispatch failed", err)
}

// -----------------------------------------------------------------------------
// apt path
// -----------------------------------------------------------------------------

func (p Cuda) runAptPath(
	ctx context.Context,
	deps *Deps,
	plan cuda.PlanResult,
	repoDistro string,
	driverMajor string,
	explicitName string,
	selection cuda.Selection,
	compileSmokeTest bool,
	log *slog.Logger,
) error {
	if repoDistro == "" {
		err := fmt.Errorf("no official NVIDIA CUDA apt repo detected for this Ubuntu version; try cuda.method=runfile or use a profile that targets a release with a CUDA apt repo (e.g. hdr-4k120-cuda-ubuntu2404)")
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "no CUDA apt repo available", err)
		}
		return p.skipNonfatal(deps, plan, "no CUDA apt repo available; mode=optional -> skipped",
			map[string]any{"method": "apt", "repo_distro": "", "selection_policy": string(selection.Policy)})
	}
	if err := p.ensureCudaAptRepo(ctx, deps, repoDistro); err != nil {
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "ensure CUDA apt repo failed", err)
		}
		return p.skipNonfatal(deps, plan, "ensure CUDA apt repo failed; mode=optional -> skipped",
			map[string]any{"method": "apt", "repo_distro": repoDistro, "err": err.Error()})
	}

	probe := p.ProbeFn
	if probe == nil {
		probe = makeAptCacheProbe(ctx, deps)
	}
	opts := selection.CandidateOptions(explicitName)
	pkg := cuda.DiscoverCandidate(opts, probe)

	if pkg == "" {
		// No apt candidate that matches the policy. Try the runfile
		// fallback when the profile pinned one AND required mode is
		// in play; otherwise fail / skip per mode.
		if plan.FailsDeployOnError && deps.Profile != nil && strings.TrimSpace(deps.Profile.CUDA.RunfileURL) != "" {
			log.Warn("phase cuda: no apt candidate satisfies selection policy; falling back to runfile",
				"policy", string(selection.Policy),
				"expected_major", selection.ExpectedMajor,
				"min_major", selection.MinMajor)
			return p.runRunfilePath(ctx, deps, plan,
				deps.Profile.CUDA.RunfileURL, deps.Profile.CUDA.RunfileSHA256,
				deps.Profile.CUDA.RunfileMaxAttempts, selection, compileSmokeTest, log)
		}
		if plan.FailsDeployOnError {
			err := fmt.Errorf("no installable CUDA toolkit candidate satisfies selection_policy=%q (expected_major=%q min_major=%q allow_ubuntu_archive_fallback=%v); ladder=%v",
				selection.Policy, selection.ExpectedMajor, selection.MinMajor,
				selection.AllowUbuntuArchiveFallback, cuda.CandidateLadder(opts))
			return p.fail(deps, plan, "no CUDA apt candidate matches selection policy", err)
		}
		return p.skipNonfatal(deps, plan, "no installable CUDA toolkit candidate; mode=optional -> skipped",
			map[string]any{
				"method":           "apt",
				"repo_distro":      repoDistro,
				"selection_policy": string(selection.Policy),
				"candidates":       cuda.CandidateLadder(opts),
			})
	}

	if isDriverMetaPackage(pkg) {
		err := fmt.Errorf("phase cuda refuses to install driver meta-package %q; the cuda phase is toolkit-only", pkg)
		return p.fail(deps, plan, "refused driver meta-package", err)
	}

	log.Info("phase cuda: apt install (toolkit-only)",
		"package", pkg,
		"repo", repoDistro,
		"selection_policy", string(selection.Policy),
		"expected_major", selection.ExpectedMajor,
		"min_major", selection.MinMajor)
	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Install(ctx, []string{pkg})
	})
	if err != nil {
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "cuda apt install failed; mode=required", err)
		}
		return p.skipNonfatal(deps, plan, "cuda apt install failed; mode=optional",
			map[string]any{"method": "apt", "package": pkg, "err": err.Error()})
	}

	return p.verifyAndFinish(ctx, deps, plan, selection, compileSmokeTest, map[string]any{
		"method":      "apt",
		"repo_distro": repoDistro,
		"package":     pkg,
		"source":      "apt",
	}, log)
}

func (p Cuda) ensureCudaAptRepo(ctx context.Context, deps *Deps, distro string) error {
	if deps.DryRun {
		return nil
	}
	url := cuda.KeyringURL(distro)
	tmpDeb := filepath.Join(os.TempDir(), fmt.Sprintf("cuda-keyring-%d.deb", time.Now().UnixNano()))
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"curl", "-fL",
			"--retry", "5", "--retry-all-errors", "--retry-delay", "5",
			"--connect-timeout", "30",
			"-o", tmpDeb, url,
		},
		Sudo: true,
	})
	if res.Err != nil {
		return fmt.Errorf("download keyring %s: %w", url, res.Err)
	}
	defer func() { _ = os.Remove(tmpDeb) }()
	dres := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{"dpkg", "-i", tmpDeb},
		Sudo: true,
	})
	if dres.Err != nil {
		return fmt.Errorf("dpkg -i %s: %w", tmpDeb, dres.Err)
	}
	return deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		return tc.Update(ctx)
	})
}

// isDriverMetaPackage returns true for the package names that would
// install the NVIDIA driver out from under the nvidia-driver phase.
func isDriverMetaPackage(name string) bool {
	switch name {
	case "cuda", "cuda-drivers":
		return true
	}
	if strings.HasPrefix(name, "cuda-drivers-") {
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// runfile path
// -----------------------------------------------------------------------------

func (p Cuda) runRunfilePath(
	ctx context.Context,
	deps *Deps,
	plan cuda.PlanResult,
	url, expectedSHA string,
	maxAttempts int,
	selection cuda.Selection,
	compileSmokeTest bool,
	log *slog.Logger,
) error {
	if strings.TrimSpace(url) == "" {
		err := errors.New("cuda.runfile_url is empty; cannot install via runfile")
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "runfile path requires cuda.runfile_url", err)
		}
		return p.skipNonfatal(deps, plan, "runfile method but no runfile_url; mode=optional -> skipped",
			map[string]any{"method": "runfile"})
	}
	maxAttempts = cuda.ResolveRunfileMaxAttempts(maxAttempts, os.Getenv("CLOUDDEPLOY_CUDA_RUNFILE_MAX_ATTEMPTS"))

	tmpdir := cuda.RunfileTmpDir
	if p.RunfileTmpDirOverride != "" {
		tmpdir = p.RunfileTmpDirOverride
	}
	if err := os.MkdirAll(tmpdir, 0o755); err != nil {
		return p.fail(deps, plan, fmt.Sprintf("mkdir %s", tmpdir), err)
	}

	var decider cuda.RetryDecider
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Info("phase cuda: runfile attempt",
			"attempt", attempt, "max_attempts", maxAttempts, "url", url, "tmpdir", tmpdir)

		runfilePath := filepath.Join(tmpdir, fmt.Sprintf("cuda-toolkit-%d.run", time.Now().UnixNano()))
		size, dlErr := p.downloadRunfile(ctx, deps, url, runfilePath)
		if dlErr != nil {
			lastErr = dlErr
			log.Warn("phase cuda: runfile download failed", "attempt", attempt, "err", dlErr)
			_ = os.Remove(runfilePath)
			continue
		}
		if size < cuda.MinRunfileBytes && !deps.DryRun {
			lastErr = fmt.Errorf("runfile size %d < %d (HTML error page disguise?)", size, cuda.MinRunfileBytes)
			log.Warn("phase cuda: rejected too-small runfile", "size", size)
			_ = os.Remove(runfilePath)
			continue
		}

		actualSHA := ""
		if s, hErr := sha256OfFile(runfilePath); hErr == nil {
			actualSHA = s
		} else if deps.DryRun && expectedSHA != "" {
			actualSHA = expectedSHA
		} else {
			log.Warn("phase cuda: sha256 failed; continuing without it", "err", hErr)
		}
		log.Info("phase cuda: runfile downloaded",
			"size", size, "sha256", actualSHA, "expected_sha256", expectedSHA)

		if ok, why := decider.ShouldRetry(actualSHA, size); !ok {
			lastErr = fmt.Errorf("retry refused: %s", why)
			log.Error("phase cuda: refusing to retry corrupt runfile",
				"sha256", actualSHA, "size", size, "reason", why)
			_ = os.Remove(runfilePath)
			break
		}

		if expectedSHA != "" && actualSHA != "" && !strings.EqualFold(actualSHA, expectedSHA) {
			decider.Record(cuda.Attempt{URL: url, Size: size, SHA256: actualSHA, CheckOK: false, ErrorTail: "sha256 mismatch"})
			lastErr = fmt.Errorf("runfile sha256 mismatch: got %s want %s", actualSHA, expectedSHA)
			log.Warn("phase cuda: runfile sha256 mismatch", "got", actualSHA, "want", expectedSHA)
			_ = os.Remove(runfilePath)
			continue
		}

		checkErr := p.checkRunfile(ctx, deps, runfilePath, tmpdir)
		decider.Record(cuda.Attempt{
			URL: url, Size: size, SHA256: actualSHA, CheckOK: checkErr == nil,
		})
		if checkErr != nil {
			lastErr = fmt.Errorf("runfile --check failed (attempt %d): %w", attempt, checkErr)
			log.Warn("phase cuda: runfile --check failed",
				"attempt", attempt, "sha256", actualSHA, "size", size, "err", checkErr)
			log.Error("phase cuda: hint", "hint", RunfileCheckCorruptHint)
			_ = os.Remove(runfilePath)
			continue
		}

		log.Info("phase cuda: runfile --check OK; installing toolkit-only",
			"sha256", actualSHA, "size", size)
		if err := p.installRunfile(ctx, deps, runfilePath, tmpdir); err != nil {
			if plan.FailsDeployOnError {
				_ = os.Remove(runfilePath)
				return p.fail(deps, plan, "runfile install failed", err)
			}
			_ = os.Remove(runfilePath)
			return p.skipNonfatal(deps, plan, "runfile install failed; mode=optional",
				map[string]any{"method": "runfile", "runfile_url": url, "sha256": actualSHA, "err": err.Error()})
		}
		_ = os.Remove(runfilePath)

		return p.verifyAndFinish(ctx, deps, plan, selection, compileSmokeTest, map[string]any{
			"method":      "runfile",
			"runfile_url": url,
			"sha256":      actualSHA,
			"size":        size,
			"source":      "runfile",
		}, log)
	}

	log.Error("phase cuda: runfile path exhausted attempts; hint follows",
		"attempts", maxAttempts, "last_err", lastErr)
	log.Error("phase cuda: hint", "hint", RunfileCheckCorruptHint)
	finalErr := fmt.Errorf("runfile install failed after %d attempts: %w. %s",
		maxAttempts, lastErr, RunfileCheckCorruptHint)
	if plan.FailsDeployOnError {
		return p.fail(deps, plan, "runfile install exhausted attempts", finalErr)
	}
	return p.skipNonfatal(deps, plan, "runfile install exhausted attempts; mode=optional",
		map[string]any{
			"method":      "runfile",
			"runfile_url": url,
			"attempts":    maxAttempts,
			"err":         finalErr.Error(),
			"hint":        RunfileCheckCorruptHint,
		})
}

func (p Cuda) downloadRunfile(ctx context.Context, deps *Deps, url, dst string) (int64, error) {
	if p.DownloadFn != nil {
		return p.DownloadFn(ctx, deps, url, dst)
	}
	if deps.DryRun {
		return cuda.MinRunfileBytes + 1, nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"curl", "-fL",
			"--retry", "10", "--retry-all-errors", "--retry-delay", "10",
			"--connect-timeout", "30", "--silent", "--show-error",
			"-o", dst, url,
		},
		Sudo:    true,
		Timeout: 60 * time.Minute,
	})
	if res.Err != nil {
		return 0, fmt.Errorf("curl: %w; stderr=%q", res.Err, tailLines(res.Stderr, 5))
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, fmt.Errorf("stat downloaded runfile: %w", err)
	}
	return info.Size(), nil
}

func (p Cuda) checkRunfile(ctx context.Context, deps *Deps, runfile, tmpdir string) error {
	if p.RunfileCheckFn != nil {
		return p.RunfileCheckFn(ctx, deps, runfile, tmpdir)
	}
	if deps.DryRun {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"sh", runfile, "--check", "--tmpdir=" + tmpdir},
		Sudo:    true,
		Timeout: 15 * time.Minute,
	})
	if res.Err != nil {
		return fmt.Errorf("runfile --check: %w; stderr=%q", res.Err, tailLines(res.Stderr, 8))
	}
	return nil
}

func (p Cuda) installRunfile(ctx context.Context, deps *Deps, runfile, tmpdir string) error {
	if p.RunfileInstallFn != nil {
		return p.RunfileInstallFn(ctx, deps, runfile, tmpdir)
	}
	if deps.DryRun {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"sh", runfile,
			"--silent",
			"--toolkit",
			"--override",
			"--tmpdir=" + tmpdir,
		},
		Env:     []string{"TMPDIR=" + tmpdir},
		Sudo:    true,
		Timeout: 30 * time.Minute,
	})
	if res.Err != nil {
		return fmt.Errorf("runfile install: %w; stderr=%q", res.Err, tailLines(res.Stderr, 12))
	}
	return nil
}

func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// -----------------------------------------------------------------------------
// verify + smoke + finish
// -----------------------------------------------------------------------------

func (p Cuda) verifyAndFinish(
	ctx context.Context,
	deps *Deps,
	plan cuda.PlanResult,
	selection cuda.Selection,
	compileSmokeTest bool,
	details map[string]any,
	log *slog.Logger,
) error {
	rel := p.readNvccRelease(ctx, deps)
	layoutOK := p.cudaLayoutOK()

	details["mode"] = string(plan.Mode)
	details["selection_policy"] = string(selection.Policy)
	details["nvcc_version"] = rel.Full()
	details["cuda_major"] = rel.Major
	details["expected_major"] = selection.EffectivePinnedMajor()
	details["verified_layout"] = layoutOK

	if rel.Major == "" || !layoutOK {
		err := fmt.Errorf("cuda verification failed: nvcc_release=%q layout_ok=%v (need /usr/local/cuda/bin/nvcc + include + lib64)",
			rel.Full(), layoutOK)
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "cuda post-install verification failed", err)
		}
		return p.skipNonfatal(deps, plan, "cuda verification failed; mode=optional", details)
	}
	if ok, why := selection.SatisfiesNvcc(rel); !ok {
		err := fmt.Errorf("cuda selection policy not satisfied: %s", why)
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "cuda selection policy not satisfied", err)
		}
		return p.skipNonfatal(deps, plan, "cuda selection policy not satisfied; mode=optional", details)
	}

	if err := p.runSmokeIfRequested(ctx, deps, compileSmokeTest, details, log); err != nil {
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "compile smoke test failed", err)
		}
		return p.skipNonfatal(deps, plan, "compile smoke test failed; mode=optional", details)
	}

	if err := p.writeProfileSnippet(); err != nil {
		log.Warn("phase cuda: writing profile.d snippet failed", "err", err)
	}
	details["profile_snippet"] = p.profileSnippetPath()
	deps.State.MarkDone(CudaName, details)
	_ = deps.PersistState()
	log.Info("phase cuda: done", "nvcc", rel.Full(), "source", details["source"])
	return nil
}

func (p Cuda) alreadyInstalledMatches(ctx context.Context, deps *Deps, selection cuda.Selection) (cuda.NvccRelease, bool) {
	if !p.cudaLayoutOK() {
		return cuda.NvccRelease{}, false
	}
	rel := p.readNvccRelease(ctx, deps)
	if rel.Major == "" {
		return cuda.NvccRelease{}, false
	}
	ok, _ := selection.SatisfiesNvcc(rel)
	return rel, ok
}

func (p Cuda) readNvccRelease(ctx context.Context, deps *Deps) cuda.NvccRelease {
	if p.NvccProbeFn != nil {
		return p.NvccProbeFn(ctx, deps)
	}
	nvcc := filepath.Join(cuda.CudaRoot, "bin", "nvcc")
	if _, err := os.Stat(nvcc); err != nil {
		nvcc = "nvcc"
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{nvcc, "--version"},
		LogFile: "-",
		Timeout: 30 * time.Second,
	})
	if res.Err != nil {
		return cuda.NvccRelease{}
	}
	return cuda.ParseNvccRelease(res.Stdout)
}

func (p Cuda) cudaLayoutOK() bool {
	if p.CudaLayoutOKFn != nil {
		return p.CudaLayoutOKFn()
	}
	if runtime.GOOS != "linux" {
		return true
	}
	for _, sub := range []string{
		filepath.Join(cuda.CudaRoot, "bin", "nvcc"),
		filepath.Join(cuda.CudaRoot, "include"),
		filepath.Join(cuda.CudaRoot, "lib64"),
	} {
		if _, err := os.Stat(sub); err != nil {
			return false
		}
	}
	return true
}

func (p Cuda) profileSnippetPath() string {
	if p.ProfileSnippetPathOverride != "" {
		return p.ProfileSnippetPathOverride
	}
	return cuda.ProfileSnippetPath
}

func (p Cuda) writeProfileSnippet() error {
	dst := p.profileSnippetPath()
	if dst == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(cuda.ProfileSnippetBody()), 0o644)
}

// -----------------------------------------------------------------------------
// smoke test
// -----------------------------------------------------------------------------

// runSmokeIfRequested writes the smoke .cu, compiles it with nvcc,
// and (best-effort) runs the compiled binary. Mutates `details` with
// compile_smoke_test* keys. Returns a non-nil error only on compile
// failure when compileSmokeTest=true; run failures are recorded but
// never fatal.
func (p Cuda) runSmokeIfRequested(
	ctx context.Context,
	deps *Deps,
	compileSmokeTest bool,
	details map[string]any,
	log *slog.Logger,
) error {
	details["compile_smoke_test"] = compileSmokeTest
	if !compileSmokeTest {
		return nil
	}
	dir := p.SmokeDirOverride
	if dir == "" {
		dir = CudaSmokeDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		details["compile_smoke_test_passed"] = false
		details["compile_smoke_test_error"] = fmt.Sprintf("mkdir %s: %v", dir, err)
		return fmt.Errorf("smoke test mkdir %s: %w", dir, err)
	}
	src := filepath.Join(dir, "smoke.cu")
	bin := filepath.Join(dir, "smoke")
	if err := os.WriteFile(src, []byte(CudaSmokeProgram), 0o644); err != nil {
		details["compile_smoke_test_passed"] = false
		details["compile_smoke_test_error"] = fmt.Sprintf("write %s: %v", src, err)
		return fmt.Errorf("smoke test write %s: %w", src, err)
	}
	details["compile_smoke_test_binary"] = bin

	log.Info("phase cuda: compiling smoke test", "src", src, "bin", bin)
	if err := p.smokeCompile(ctx, deps, src, bin); err != nil {
		details["compile_smoke_test_passed"] = false
		details["compile_smoke_test_error"] = err.Error()
		return fmt.Errorf("nvcc compile %s: %w", src, err)
	}
	details["compile_smoke_test_passed"] = true

	// Best-effort run: record outcome, never fatal.
	if runErr := p.smokeRun(ctx, deps, bin); runErr != nil {
		log.Warn("phase cuda: smoke binary run failed (non-fatal; no CUDA device on this image?)", "err", runErr)
		details["runtime_smoke_test_passed"] = false
		details["runtime_smoke_test_error"] = runErr.Error()
	} else {
		details["runtime_smoke_test_passed"] = true
	}
	return nil
}

func (p Cuda) smokeCompile(ctx context.Context, deps *Deps, src, bin string) error {
	if p.SmokeCompileFn != nil {
		return p.SmokeCompileFn(ctx, deps, src, bin)
	}
	if deps.DryRun {
		// Pretend to compile under DryRun so the happy path still
		// reaches MarkDone in apply --dry-run.
		return nil
	}
	nvcc := filepath.Join(cuda.CudaRoot, "bin", "nvcc")
	if _, err := os.Stat(nvcc); err != nil {
		nvcc = "nvcc"
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{nvcc, "-o", bin, src},
		Sudo:    true,
		Timeout: 5 * time.Minute,
	})
	if res.Err != nil {
		return fmt.Errorf("%s -o %s %s: %w; stderr=%q",
			nvcc, bin, src, res.Err, tailLines(res.Stderr, 12))
	}
	return nil
}

func (p Cuda) smokeRun(ctx context.Context, deps *Deps, bin string) error {
	if p.SmokeRunFn != nil {
		return p.SmokeRunFn(ctx, deps, bin)
	}
	if deps.DryRun {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{bin},
		Sudo:    true,
		Timeout: 30 * time.Second,
	})
	if res.Err != nil {
		return fmt.Errorf("run %s: %w; stderr=%q", bin, res.Err, tailLines(res.Stderr, 5))
	}
	return nil
}

// -----------------------------------------------------------------------------
// repo probing helpers
// -----------------------------------------------------------------------------

func (p Cuda) detectRepoDistro(ctx context.Context, deps *Deps) string {
	v := p.currentUbuntuVersion()
	if v == "" {
		return ""
	}
	probe := p.RepoProbeFn
	if probe == nil {
		probe = func(distro string) bool {
			return p.httpHead(ctx, deps, cuda.KeyringURL(distro))
		}
	}
	return cuda.DetectRepoDistro(v, probe)
}

func (p Cuda) currentUbuntuVersion() string {
	if p.CurrentUbuntuVersionFn != nil {
		return p.CurrentUbuntuVersionFn()
	}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "VERSION_ID=") {
			return strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"'`)
		}
	}
	return ""
}

func (p Cuda) httpHead(ctx context.Context, deps *Deps, url string) bool {
	if p.HTTPHeadFn != nil {
		return p.HTTPHeadFn(ctx, deps, url)
	}
	if url == "" {
		return false
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"curl", "--head", "-fsSL", "--connect-timeout", "10", "--max-time", "20", url},
		LogFile: "-",
		Timeout: 25 * time.Second,
	})
	return res.Err == nil
}

// -----------------------------------------------------------------------------
// terminal-state helpers
// -----------------------------------------------------------------------------

func (p Cuda) fail(deps *Deps, plan cuda.PlanResult, reason string, err error) error {
	deps.State.MarkFailed(CudaName, reason, err, true)
	deps.State.Get(CudaName).Details = map[string]any{
		"mode":     string(plan.Mode),
		"fatal":    true,
		"plan_msg": plan.Rationale,
	}
	_ = deps.PersistState()
	return fmt.Errorf("phase cuda (mode=%s): %s: %w", plan.Mode, reason, err)
}

func (p Cuda) skipNonfatal(deps *Deps, plan cuda.PlanResult, reason string, extra map[string]any) error {
	d := map[string]any{
		"mode":     string(plan.Mode),
		"plan_msg": plan.Rationale,
		"fatal":    false,
	}
	for k, v := range extra {
		d[k] = v
	}
	deps.State.MarkSkipped(CudaName, reason)
	deps.State.Get(CudaName).Details = d
	_ = deps.PersistState()
	return nil
}

func makeAptCacheProbe(ctx context.Context, deps *Deps) cuda.AvailabilityProbe {
	return func(pkg string) bool {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"apt-cache", "policy", pkg},
			LogFile: "-",
			DryRun:  deps.DryRun,
		})
		if res.Err != nil {
			return false
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Candidate:") {
				cand := strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
				return cand != "" && cand != "(none)"
			}
		}
		return false
	}
}

func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
