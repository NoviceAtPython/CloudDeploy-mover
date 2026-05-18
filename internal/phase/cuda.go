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

// Cuda phase: real v2-equivalent CUDA toolkit install. Honors
// cuda.Mode (none/optional/required) and cuda.Method (auto/apt/runfile/none),
// drops a /etc/profile.d/clouddeploy-cuda.sh after a successful install,
// and verifies nvcc + /usr/local/cuda layout. Defensible against the
// known failure modes:
//
//   - same runfile sha256 + same --check failure -> refuses to retry
//     (cuda.RetryDecider, the v2 regression we are preventing).
//   - HTML error page disguised as a runfile -> caught by
//     cuda.MinRunfileBytes.
//   - Ubuntu's nvidia-cuda-toolkit installed when CUDA 13 is required
//     -> filtered out of the candidate ladder.
//   - cuda-drivers (the driver meta-package) ever installed -> apt
//     path only installs toolkit packages; runfile uses --toolkit
//     never --driver.
type Cuda struct {
	// ProbeFn lets tests inject an apt-cache availability probe.
	// nil = use real apt-cache via the runner.
	ProbeFn cuda.AvailabilityProbe

	// RepoProbeFn lets tests inject the "is the NVIDIA CUDA apt repo
	// for distro X reachable?" check. nil = use a real HEAD probe
	// via the runner.
	RepoProbeFn cuda.RepoAvailabilityProbe

	// CurrentUbuntuVersionFn lets tests inject the VERSION_ID instead
	// of reading /etc/os-release. nil = real read.
	CurrentUbuntuVersionFn func() string

	// NvccProbeFn lets tests inject the cached "nvcc release on this
	// host" lookup. Returns NvccRelease{} when nvcc is absent or
	// unparseable. nil = call /usr/local/cuda/bin/nvcc --version via
	// the runner.
	NvccProbeFn func(ctx context.Context, deps *Deps) cuda.NvccRelease

	// CudaLayoutOKFn lets tests fake the /usr/local/cuda {bin/nvcc,
	// include, lib64} layout check. nil = real os.Stat.
	CudaLayoutOKFn func() bool

	// HTTPHeadFn lets tests inject the keyring-reachable check
	// without curling. nil = curl --head via the runner.
	HTTPHeadFn func(ctx context.Context, deps *Deps, url string) bool

	// DownloadFn lets tests inject the runfile download step. nil =
	// curl --retry via the runner. Returns the on-disk path + size
	// or an error.
	DownloadFn func(ctx context.Context, deps *Deps, url, dst string) (size int64, err error)

	// RunfileCheckFn lets tests inject the `sh runfile --check` step.
	// nil = call via the runner.
	RunfileCheckFn func(ctx context.Context, deps *Deps, runfile, tmpdir string) error

	// RunfileInstallFn lets tests inject the `sh runfile --silent
	// --toolkit --override --tmpdir=...` step. nil = call via the
	// runner.
	RunfileInstallFn func(ctx context.Context, deps *Deps, runfile, tmpdir string) error

	// ProfileSnippetPathOverride lets tests redirect the on-disk
	// /etc/profile.d/clouddeploy-cuda.sh write to a temp file.
	// Empty = the real path.
	ProfileSnippetPathOverride string

	// RunfileTmpDirOverride lets tests redirect the staging dir to a
	// t.TempDir(). Empty = cuda.RunfileTmpDir (`/var/tmp/clouddeploy-cuda`).
	RunfileTmpDirOverride string
}

// Name implements Phase.
func (Cuda) Name() string { return CudaName }

// Run implements Phase. See type-level docstring for the algorithm.
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
		explicitName = deps.Profile.CUDA.PackageName
		driverMajor = deps.Profile.NVIDIA.DriverMajor
		runfileURL = deps.Profile.CUDA.RunfileURL
		runfileSHA = deps.Profile.CUDA.RunfileSHA256
		runfileMaxAttempts = deps.Profile.CUDA.RunfileMaxAttempts
	}
	plan := cuda.Plan(mode)
	expectedMajor := cuda.ExpectedMajor(explicitName, runfileURL)

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

	// Short-circuit: working install already on disk with a
	// satisfying nvcc release.
	if rel, ok := p.alreadyInstalledMatches(ctx, deps, expectedMajor); ok {
		if err := p.writeProfileSnippet(); err != nil {
			log.Warn("phase cuda: failed to write profile.d snippet", "err", err)
		}
		deps.State.MarkDone(CudaName, map[string]any{
			"mode":            string(plan.Mode),
			"method":          string(method),
			"source":          "already-installed",
			"nvcc_version":    rel.Full(),
			"cuda_major":      rel.Major,
			"expected_major":  expectedMajor,
			"verified_layout": true,
		})
		_ = deps.PersistState()
		log.Info("phase cuda: nvcc already installed and matches expected",
			"nvcc", rel.Full(), "expected_major", expectedMajor)
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
		return p.runAptPath(ctx, deps, plan, repoDistro, driverMajor, explicitName, expectedMajor, log)
	case cuda.MethodRunfile:
		return p.runRunfilePath(ctx, deps, plan, runfileURL, runfileSHA, runfileMaxAttempts, expectedMajor, log)
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
	expectedMajor string,
	log *slog.Logger,
) error {
	if repoDistro == "" {
		// method=apt with no reachable repo. Fail or skip per mode.
		err := fmt.Errorf("no official NVIDIA CUDA apt repo detected for this Ubuntu version; try method=runfile or set deploy fallback")
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "no CUDA apt repo available", err)
		}
		return p.skipNonfatal(deps, plan, "no CUDA apt repo available; mode=optional -> skipped",
			map[string]any{"method": "apt", "repo_distro": ""})
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
	opts := cuda.CandidateOptions{
		PreferredMajor: driverMajor,
		ExplicitName:   explicitName,
		RequiredMajor:  expectedMajor,
	}
	pkg := cuda.DiscoverCandidate(opts, probe)

	// If no apt candidate matches AND we're in required mode, fall
	// back to the runfile path before declaring defeat (only if the
	// profile pinned a runfile URL).
	if pkg == "" {
		if plan.FailsDeployOnError && deps.Profile != nil && strings.TrimSpace(deps.Profile.CUDA.RunfileURL) != "" {
			log.Warn("phase cuda: no apt candidate matches required-major; falling back to runfile",
				"required_major", expectedMajor)
			return p.runRunfilePath(ctx, deps, plan,
				deps.Profile.CUDA.RunfileURL, deps.Profile.CUDA.RunfileSHA256,
				deps.Profile.CUDA.RunfileMaxAttempts, expectedMajor, log)
		}
		if plan.FailsDeployOnError {
			err := fmt.Errorf("no installable CUDA toolkit candidate (required major=%q) in ladder %v", expectedMajor, cuda.CandidateLadder(opts))
			return p.fail(deps, plan, "no CUDA apt candidate matches required major", err)
		}
		return p.skipNonfatal(deps, plan, "no installable CUDA toolkit candidate; mode=optional -> skipped",
			map[string]any{"method": "apt", "repo_distro": repoDistro, "candidates": cuda.CandidateLadder(opts)})
	}

	// Refuse known driver meta-packages explicitly. (DiscoverCandidate
	// today only ever returns toolkit names, but a future operator
	// override via package_name should not be able to push us into
	// installing cuda-drivers and clobbering the driver phase.)
	if isDriverMetaPackage(pkg) {
		err := fmt.Errorf("phase cuda refuses to install driver meta-package %q; the cuda phase is toolkit-only", pkg)
		return p.fail(deps, plan, "refused driver meta-package", err)
	}

	log.Info("phase cuda: apt install (toolkit-only)",
		"package", pkg, "repo", repoDistro, "expected_major", expectedMajor)
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

	return p.verifyAndFinish(ctx, deps, plan, map[string]any{
		"method":      "apt",
		"repo_distro": repoDistro,
		"package":     pkg,
		"source":      "apt",
	}, expectedMajor, log)
}

func (p Cuda) ensureCudaAptRepo(ctx context.Context, deps *Deps, distro string) error {
	if deps.DryRun {
		return nil
	}
	url := cuda.KeyringURL(distro)
	tmpDeb := filepath.Join(os.TempDir(), fmt.Sprintf("cuda-keyring-%d.deb", time.Now().UnixNano()))
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"curl",
			"-fL",
			"--retry", "5",
			"--retry-all-errors",
			"--retry-delay", "5",
			"--connect-timeout", "30",
			"-o", tmpDeb,
			url,
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
	// apt-get update so the new repo's Release file is loaded.
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
	expectedMajor string,
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
	// MkdirAll is cheap and safe under DryRun too: it never installs
	// anything, just guarantees the staging dir exists so the
	// download/check hooks (or the runner) have somewhere to write.
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

		// Always try to hash whatever's on disk. The download hook
		// (real curl or a test stub) is expected to have produced a
		// file at runfilePath; if it didn't, we just log empty and
		// continue (the SHA mismatch / RetryDecider checks behave
		// correctly with an empty hash).
		actualSHA := ""
		if s, hErr := sha256OfFile(runfilePath); hErr == nil {
			actualSHA = s
		} else if deps.DryRun && expectedSHA != "" {
			// True dry-run with no on-disk file and no download hook:
			// assume the pin will match so we exercise the rest of the
			// happy path without faking I/O.
			actualSHA = expectedSHA
		} else {
			log.Warn("phase cuda: sha256 failed; continuing without it", "err", hErr)
		}
		log.Info("phase cuda: runfile downloaded",
			"size", size, "sha256", actualSHA, "expected_sha256", expectedSHA)

		// Anti-corruption guard: if a previous attempt with the same
		// (sha256,size) already failed --check, do not retry.
		if ok, why := decider.ShouldRetry(actualSHA, size); !ok {
			lastErr = fmt.Errorf("retry refused: %s", why)
			log.Error("phase cuda: refusing to retry corrupt runfile",
				"sha256", actualSHA, "size", size, "reason", why)
			_ = os.Remove(runfilePath)
			break
		}

		// Refuse SHA mismatch when the operator pinned one.
		if expectedSHA != "" && actualSHA != "" && !strings.EqualFold(actualSHA, expectedSHA) {
			decider.Record(cuda.Attempt{URL: url, Size: size, SHA256: actualSHA, CheckOK: false, ErrorTail: "sha256 mismatch"})
			lastErr = fmt.Errorf("runfile sha256 mismatch: got %s want %s", actualSHA, expectedSHA)
			log.Warn("phase cuda: runfile sha256 mismatch", "got", actualSHA, "want", expectedSHA)
			_ = os.Remove(runfilePath)
			continue
		}

		// --check the runfile before running the installer.
		checkErr := p.checkRunfile(ctx, deps, runfilePath, tmpdir)
		decider.Record(cuda.Attempt{
			URL:     url,
			Size:    size,
			SHA256:  actualSHA,
			CheckOK: checkErr == nil,
		})
		if checkErr != nil {
			lastErr = fmt.Errorf("runfile --check failed (attempt %d): %w", attempt, checkErr)
			log.Warn("phase cuda: runfile --check failed",
				"attempt", attempt, "sha256", actualSHA, "err", checkErr)
			_ = os.Remove(runfilePath)
			continue
		}

		// --check passed: install toolkit-only.
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

		return p.verifyAndFinish(ctx, deps, plan, map[string]any{
			"method":      "runfile",
			"runfile_url": url,
			"sha256":      actualSHA,
			"size":        size,
			"source":      "runfile",
		}, expectedMajor, log)
	}

	// All attempts exhausted.
	finalErr := fmt.Errorf("runfile install failed after %d attempts: %w", maxAttempts, lastErr)
	if plan.FailsDeployOnError {
		return p.fail(deps, plan, "runfile install exhausted attempts", finalErr)
	}
	return p.skipNonfatal(deps, plan, "runfile install exhausted attempts; mode=optional",
		map[string]any{"method": "runfile", "runfile_url": url, "attempts": maxAttempts, "err": finalErr.Error()})
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
			"curl",
			"-fL",
			"--retry", "10",
			"--retry-all-errors",
			"--retry-delay", "10",
			"--connect-timeout", "30",
			"--silent", "--show-error",
			"-o", dst,
			url,
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
			"--toolkit", // toolkit-only; NEVER --driver
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
// verify + finish + state details helpers
// -----------------------------------------------------------------------------

func (p Cuda) verifyAndFinish(
	ctx context.Context,
	deps *Deps,
	plan cuda.PlanResult,
	details map[string]any,
	expectedMajor string,
	log *slog.Logger,
) error {
	rel := p.readNvccRelease(ctx, deps)
	layoutOK := p.cudaLayoutOK()

	details["mode"] = string(plan.Mode)
	details["nvcc_version"] = rel.Full()
	details["cuda_major"] = rel.Major
	details["expected_major"] = expectedMajor
	details["verified_layout"] = layoutOK

	if rel.Major == "" || !layoutOK {
		err := fmt.Errorf("cuda verification failed: nvcc_release=%q layout_ok=%v (need /usr/local/cuda/bin/nvcc + include + lib64)",
			rel.Full(), layoutOK)
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "cuda post-install verification failed", err)
		}
		return p.skipNonfatal(deps, plan, "cuda verification failed; mode=optional", details)
	}
	if !cuda.MajorMatches(rel, expectedMajor) {
		err := fmt.Errorf("cuda version mismatch: nvcc=%s expected major=%s", rel.Full(), expectedMajor)
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "cuda major mismatch", err)
		}
		return p.skipNonfatal(deps, plan, "cuda major mismatch; mode=optional", details)
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

func (p Cuda) alreadyInstalledMatches(ctx context.Context, deps *Deps, expectedMajor string) (cuda.NvccRelease, bool) {
	if !p.cudaLayoutOK() {
		return cuda.NvccRelease{}, false
	}
	rel := p.readNvccRelease(ctx, deps)
	if rel.Major == "" {
		return cuda.NvccRelease{}, false
	}
	return rel, cuda.MajorMatches(rel, expectedMajor)
}

func (p Cuda) readNvccRelease(ctx context.Context, deps *Deps) cuda.NvccRelease {
	if p.NvccProbeFn != nil {
		return p.NvccProbeFn(ctx, deps)
	}
	nvcc := filepath.Join(cuda.CudaRoot, "bin", "nvcc")
	if _, err := os.Stat(nvcc); err != nil {
		// Fall back to PATH lookup.
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
		// Tests on Windows: there is no /usr/local/cuda; trust the
		// nvcc probe to gate verification.
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
	// Best-effort read; if /etc/os-release is missing (developer
	// host) we just return "" and the caller treats apt as
	// unavailable.
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "VERSION_ID=") {
			v := strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"'`)
			return v
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

// makeAptCacheProbe returns an AvailabilityProbe that shells out to
// `apt-cache policy <pkg>` via the deps runner.
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
