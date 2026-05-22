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
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// ubuntuCodenameFromTable is a tiny wrapper around the ubuntu package
// so callers in this file don't grow an `ubuntu.` import noise burden.
func ubuntuCodenameFromTable(version string) string {
	return ubuntu.CodenameForVersion(version)
}

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

const (
	cudaNvccVersionTimeout  = 15 * time.Second
	cudaNvidiaSmiTimeout    = 15 * time.Second
	cudaSmokeCompileTimeout = 120 * time.Second
	cudaSmokeRunTimeout     = 30 * time.Second
)

// CudaSmokeProgram is the trivial .cu file the phase compiles after
// install to prove that nvcc actually works against the installed
// toolkit headers + libraries. Running the resulting binary is also
// bounded: required mode fails cleanly on compile/run failures, while
// optional mode records a degraded terminal state and lets the gaming
// deployment continue to NVENC/Sunshine validation.
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

type cudaStageError struct {
	Stage      string
	Reason     string
	Limit      time.Duration
	Elapsed    time.Duration
	StdoutTail string
	StderrTail string
	Err        error
}

func (e *cudaStageError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Reason {
	case "smoke_compile_timeout":
		return fmt.Sprintf("CUDA smoke compile timed out after %s", secondsString(e.Limit))
	case "smoke_compile_failed":
		if e.Err != nil {
			return "CUDA smoke compile failed: " + e.Err.Error()
		}
		return "CUDA smoke compile failed"
	case "smoke_run_timeout":
		return fmt.Sprintf("CUDA smoke run timed out after %s", secondsString(e.Limit))
	case "smoke_run_failed":
		if e.Err != nil {
			return "CUDA smoke exited nonzero: " + e.Err.Error()
		}
		return "CUDA smoke exited nonzero"
	case "nvcc_version_timeout":
		return fmt.Sprintf("CUDA nvcc --version timed out after %s", secondsString(e.Limit))
	case "nvidia_smi_timeout":
		return fmt.Sprintf("nvidia-smi timed out after %s", secondsString(e.Limit))
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Reason
}

func (e *cudaStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type nvccProbeResult struct {
	Release cuda.NvccRelease
	Path    string
	Error   *cudaStageError
}

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

	// CudaLayoutFn lets tests inject the source-aware layout probe.
	// Production: walks both LayoutNvidiaCanonical and
	// LayoutUbuntuArchive paths via os.Stat. nil = real probe.
	CudaLayoutFn func() cuda.CudaLayout

	HTTPHeadFn       func(ctx context.Context, deps *Deps, url string) bool
	DownloadFn       func(ctx context.Context, deps *Deps, url, dst string) (size int64, err error)
	RunfileCheckFn   func(ctx context.Context, deps *Deps, runfile, tmpdir string) error
	RunfileInstallFn func(ctx context.Context, deps *Deps, runfile, tmpdir string) error

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

	// Timeout overrides are test hooks. Production uses the constants
	// above so live deploys get fixed, predictable ceilings.
	NvccVersionTimeoutOverride  time.Duration
	NvidiaSmiTimeoutOverride    time.Duration
	SmokeCompileTimeoutOverride time.Duration
	SmokeRunTimeoutOverride     time.Duration
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
	if rel, layout, ok := p.alreadyInstalledMatches(ctx, deps, selection); ok {
		baseDetails := map[string]any{
			"mode":                   string(plan.Mode),
			"method":                 string(method),
			"selection_policy":       string(selection.Policy),
			"source":                 "already-installed",
			"nvcc_version":           rel.Full(),
			"cuda_major":             rel.Major,
			"selected_major":         rel.Major,
			"driver_preferred_major": selection.DriverPreferredMajor,
			"required_major":         selection.ExpectedMajor,
			"min_major":              selection.MinMajor,
			"expected_major":         selection.EffectivePinnedMajor(),
			"layout_kind":            string(layout.Kind),
			"layout_nvcc":            layout.NvccPath,
			"layout_headers":         layout.Headers,
			"layout_libs":            layout.Libs,
			"verified_layout":        true,
		}
		p.probeNvidiaSmi(ctx, deps, baseDetails, log)
		if err := p.runSmokeIfRequested(ctx, deps, compileSmokeTest, baseDetails, log); err != nil {
			recordCudaDegraded(baseDetails, plan, err)
			if plan.FailsDeployOnError {
				return p.failWithDetails(deps, plan, cudaFailureSummary(err), err, baseDetails)
			}
			return p.skipNonfatal(deps, plan, cudaFailureSummary(err)+"; mode=optional -> degraded", baseDetails)
		}
		if err := p.writeProfileSnippet(); err != nil {
			log.Warn("phase cuda: failed to write profile.d snippet", "err", err)
		}
		baseDetails["cuda_status"] = "ok"
		baseDetails["cuda_required"] = plan.FailsDeployOnError
		baseDetails["profile_snippet"] = p.profileSnippetPath()
		deps.State.MarkDone(CudaName, baseDetails)
		_ = deps.PersistState()
		log.Info("phase cuda: nvcc already installed and policy-satisfied",
			"nvcc", rel.Full(), "policy", string(selection.Policy))
		return nil
	}

	// Resolve method=auto -> apt or runfile.
	effectiveMethod := method
	repoSel := cuda.RepoSelection{Tried: []string{}}
	hostVersion := p.currentUbuntuVersion()
	if effectiveMethod == cuda.MethodAuto || effectiveMethod == cuda.MethodApt {
		var candidates []string
		if deps.Profile != nil {
			candidates = deps.Profile.CUDA.CudaRepoDistroCandidates
		}
		allowCross := deps.Profile != nil && deps.Profile.CUDA.AllowCrossDistroCudaRepo
		resolved := cuda.ResolveCudaRepoCandidates(hostVersion, candidates, allowCross)
		probe := p.RepoProbeFn
		if probe == nil {
			probe = func(distro string) bool {
				return p.httpHead(ctx, deps, cuda.KeyringURL(distro))
			}
		}
		repoSel = cuda.PickReachableRepoDistro(hostVersion, resolved, probe)
		log.Info("phase cuda: NVIDIA CUDA repo probe",
			"tried", repoSel.Tried,
			"selected", repoSel.Selected,
			"cross_distro", repoSel.CrossDistro,
			"host_native", repoSel.HostNative)
	}
	if effectiveMethod == cuda.MethodAuto {
		if repoSel.Selected != "" {
			effectiveMethod = cuda.MethodApt
		} else {
			effectiveMethod = cuda.MethodRunfile
		}
	}

	switch effectiveMethod {
	case cuda.MethodApt:
		return p.runAptPath(ctx, deps, plan, repoSel, hostVersion, driverMajor, explicitName, selection, compileSmokeTest, log)
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
	repoSel cuda.RepoSelection,
	hostVersion string,
	driverMajor string,
	explicitName string,
	selection cuda.Selection,
	compileSmokeTest bool,
	log *slog.Logger,
) error {
	repoDistro := repoSel.Selected
	hostCodename := ubuntuCodename(hostVersion)
	fallbackReason := ""

	// archiveOnly: no official NVIDIA CUDA apt repo for this Ubuntu
	// version among the configured candidates, but the profile allows
	// Ubuntu's archive fallback. We skip cuda-keyring bootstrap
	// entirely and rely on whatever nvidia-cuda-toolkit is already in
	// the host's apt sources.
	archiveOnly := repoDistro == "" && selection.AllowUbuntuArchiveFallback
	if archiveOnly {
		fallbackReason = "no reachable NVIDIA CUDA apt repo among candidates; allow_ubuntu_archive_fallback=true"
	}
	if repoDistro == "" && !archiveOnly {
		err := fmt.Errorf("no reachable NVIDIA CUDA apt repo among candidates %v, and cuda.allow_ubuntu_archive_fallback=false; try cuda.method=runfile, set cuda.allow_ubuntu_archive_fallback=true to accept Ubuntu's CUDA 12 archive, or pin cuda.cuda_repo_distro_candidates to a slug that exists (e.g. ubuntu2404)", repoSel.Tried)
		if plan.FailsDeployOnError {
			return p.fail(deps, plan, "no CUDA apt repo available", err)
		}
		return p.skipNonfatal(deps, plan, "no CUDA apt repo available; mode=optional -> skipped",
			map[string]any{
				"method":                 "apt",
				"selection_policy":       string(selection.Policy),
				"host_ubuntu_version":    hostVersion,
				"host_codename":          hostCodename,
				"host_native_repo":       repoSel.HostNative,
				"cuda_repo_distro_tried": repoSel.Tried,
				"selected_repo_distro":   "",
				"cross_distro_cuda_repo": false,
			})
	}
	if !archiveOnly {
		if err := p.ensureCudaAptRepo(ctx, deps, repoDistro); err != nil {
			if plan.FailsDeployOnError {
				return p.fail(deps, plan, "ensure CUDA apt repo failed", err)
			}
			return p.skipNonfatal(deps, plan, "ensure CUDA apt repo failed; mode=optional -> skipped",
				map[string]any{"method": "apt", "repo_distro": repoDistro, "err": err.Error()})
		}
		if repoSel.CrossDistro {
			log.Warn("phase cuda: using NVIDIA CUDA apt repo from a different Ubuntu distro than the host",
				"host_native", repoSel.HostNative,
				"selected_repo", repoDistro,
				"reason", "host-native NVIDIA CUDA repo unreachable for this Ubuntu version")
		}
	} else {
		log.Info("phase cuda: no NVIDIA CUDA apt repo reachable; using Ubuntu archive fallback (no cuda-keyring bootstrap)",
			"selection_policy", string(selection.Policy),
			"min_major", selection.MinMajor,
			"tried", repoSel.Tried)
	}

	probe := p.ProbeFn
	if probe == nil {
		probe = makeAptCacheProbe(ctx, deps)
	}
	// Apply prefer_major as the driver-preferred-major override:
	// the candidate ladder puts that major first in the ordering.
	// strict policies (exact-major / min-major) still authoritatively
	// filter on top.
	opts := selection.CandidateOptions(explicitName)
	if deps.Profile != nil && strings.TrimSpace(deps.Profile.CUDA.PreferMajor) != "" {
		opts.PreferredMajor = strings.TrimSpace(deps.Profile.CUDA.PreferMajor)
	}
	pkg := cuda.DiscoverCandidate(opts, probe)

	// Diagnostic value persisted to state.Details for both
	// success and failure branches.
	repoDisplay := repoDistro
	if archiveOnly {
		repoDisplay = "ubuntu-archive"
	}
	baseRepoDetails := func() map[string]any {
		m := map[string]any{
			"host_ubuntu_version":    hostVersion,
			"host_codename":          hostCodename,
			"host_native_repo":       repoSel.HostNative,
			"cuda_repo_distro_tried": repoSel.Tried,
			"selected_repo_distro":   repoDisplay,
			"cross_distro_cuda_repo": repoSel.CrossDistro,
		}
		if fallbackReason != "" {
			m["fallback_reason"] = fallbackReason
		}
		return m
	}

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
			err := fmt.Errorf("no installable CUDA toolkit candidate satisfies selection_policy=%q (expected_major=%q min_major=%q allow_ubuntu_archive_fallback=%v); ladder=%v; repo_tried=%v",
				selection.Policy, selection.ExpectedMajor, selection.MinMajor,
				selection.AllowUbuntuArchiveFallback, cuda.CandidateLadder(opts), repoSel.Tried)
			return p.fail(deps, plan, "no CUDA apt candidate matches selection policy", err)
		}
		skipDetails := baseRepoDetails()
		skipDetails["method"] = "apt"
		skipDetails["archive_fallback"] = archiveOnly
		skipDetails["selection_policy"] = string(selection.Policy)
		skipDetails["candidates"] = cuda.CandidateLadder(opts)
		return p.skipNonfatal(deps, plan, "no installable CUDA toolkit candidate; mode=optional -> skipped", skipDetails)
	}

	if isDriverMetaPackage(pkg) {
		err := fmt.Errorf("phase cuda refuses to install driver meta-package %q; the cuda phase is toolkit-only", pkg)
		return p.fail(deps, plan, "refused driver meta-package", err)
	}

	log.Info("phase cuda: apt install (toolkit-only)",
		"package", pkg,
		"repo", repoDisplay,
		"archive_fallback", archiveOnly,
		"cross_distro_cuda_repo", repoSel.CrossDistro,
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
		failDetails := baseRepoDetails()
		failDetails["method"] = "apt"
		failDetails["package"] = pkg
		failDetails["err"] = err.Error()
		return p.skipNonfatal(deps, plan, "cuda apt install failed; mode=optional", failDetails)
	}

	successDetails := baseRepoDetails()
	successDetails["method"] = "apt"
	successDetails["archive_fallback"] = archiveOnly
	successDetails["package"] = pkg
	successDetails["source"] = "apt"
	// repo_distro is the legacy alias; baseRepoDetails records the
	// clearer selected_repo_distro + cross_distro_cuda_repo pair.
	successDetails["repo_distro"] = repoDisplay
	return p.verifyAndFinish(ctx, deps, plan, selection, compileSmokeTest, successDetails, log)
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
	log.Info("phase cuda: probing nvcc version", "timeout", p.nvccVersionTimeout().String())
	nvccProbe := p.readNvccReleaseDetailed(ctx, deps)
	rel := nvccProbe.Release
	layout := p.detectCudaLayout()

	// Richer state-of-record fields. The pre-existing `expected_major`
	// key stays for backwards compat (set to the strict pin, the min
	// floor, or the driver-preferred fallback - whichever Selection
	// considers "operator-pinned"), but doctor / collect-logs should
	// prefer these clearer keys.
	details["mode"] = string(plan.Mode)
	details["selection_policy"] = string(selection.Policy)
	details["nvcc_version"] = rel.Full()
	details["nvcc_path"] = nvccProbe.Path
	details["nvcc_version_timeout"] = p.nvccVersionTimeout().String()
	if nvccProbe.Error != nil {
		details["nvcc_version_error"] = nvccProbe.Error.Error()
		details["nvcc_version_reason"] = nvccProbe.Error.Reason
		details["nvcc_version_elapsed"] = nvccProbe.Error.Elapsed.Round(time.Millisecond).String()
		details["nvcc_version_stdout_tail"] = nvccProbe.Error.StdoutTail
		details["nvcc_version_stderr_tail"] = nvccProbe.Error.StderrTail
	}
	details["cuda_major"] = rel.Major
	details["selected_major"] = rel.Major
	details["selected_package"] = stringOr(details, "package", "")
	details["driver_preferred_major"] = selection.DriverPreferredMajor
	details["required_major"] = selection.ExpectedMajor
	details["min_major"] = selection.MinMajor
	details["expected_major"] = selection.EffectivePinnedMajor()
	details["layout_kind"] = string(layout.Kind)
	details["layout_nvcc"] = layout.NvccPath
	details["layout_headers"] = layout.Headers
	details["layout_libs"] = layout.Libs
	details["verified_layout"] = layout.Kind != cuda.LayoutMissing
	details["cuda_required"] = plan.FailsDeployOnError
	p.probeNvidiaSmi(ctx, deps, details, log)

	// Source-aware layout requirement.
	expected := cuda.LayoutFromInstallSource(
		stringOr(details, "source", ""),
		stringOr(details, "package", ""),
		boolOr(details, "archive_fallback", false),
	)
	if rel.Major == "" {
		err := fmt.Errorf("cuda verification failed: nvcc_release=%q (could not parse `nvcc --version`)", rel.Full())
		if plan.FailsDeployOnError {
			return p.failWithDetails(deps, plan, "cuda post-install verification failed", err, details)
		}
		return p.skipNonfatal(deps, plan, "cuda verification failed; mode=optional", details)
	}
	if layout.Kind == cuda.LayoutMissing {
		err := fmt.Errorf("cuda verification failed: no usable toolkit layout on disk (looked for /usr/local/cuda/{bin/nvcc,include,lib64} AND /usr/bin/nvcc + /usr/include/cuda_runtime.h|/usr/lib/cuda/include + libcudart)")
		if plan.FailsDeployOnError {
			return p.failWithDetails(deps, plan, "cuda post-install verification failed", err, details)
		}
		return p.skipNonfatal(deps, plan, "cuda verification failed; mode=optional", details)
	}
	if expected != "" && layout.Kind != expected {
		// Strict path: a runfile install or NVIDIA-repo apt install
		// MUST land /usr/local/cuda; we refuse to declare success if
		// the host instead reports only the Ubuntu archive layout
		// (which would mean the canonical install silently no-op'd).
		err := fmt.Errorf("cuda verification failed: install source=%q expects %q layout but host shows %q",
			details["source"], expected, layout.Kind)
		if plan.FailsDeployOnError {
			return p.failWithDetails(deps, plan, "cuda layout does not match install source", err, details)
		}
		return p.skipNonfatal(deps, plan, "cuda layout mismatch; mode=optional", details)
	}
	if ok, why := selection.SatisfiesNvcc(rel); !ok {
		err := fmt.Errorf("cuda selection policy not satisfied: %s", why)
		if plan.FailsDeployOnError {
			return p.failWithDetails(deps, plan, "cuda selection policy not satisfied", err, details)
		}
		return p.skipNonfatal(deps, plan, "cuda selection policy not satisfied; mode=optional", details)
	}

	if err := p.runSmokeIfRequested(ctx, deps, compileSmokeTest, details, log); err != nil {
		recordCudaDegraded(details, plan, err)
		if plan.FailsDeployOnError {
			return p.failWithDetails(deps, plan, cudaFailureSummary(err), err, details)
		}
		return p.skipNonfatal(deps, plan, cudaFailureSummary(err)+"; mode=optional -> degraded", details)
	}

	if err := p.writeProfileSnippet(); err != nil {
		log.Warn("phase cuda: writing profile.d snippet failed", "err", err)
	}
	details["profile_snippet"] = p.profileSnippetPath()
	details["cuda_status"] = "ok"
	deps.State.MarkDone(CudaName, details)
	_ = deps.PersistState()
	log.Info("phase cuda: done", "nvcc", rel.Full(), "source", details["source"])
	return nil
}

func (p Cuda) alreadyInstalledMatches(ctx context.Context, deps *Deps, selection cuda.Selection) (cuda.NvccRelease, cuda.CudaLayout, bool) {
	layout := p.detectCudaLayout()
	if layout.Kind == cuda.LayoutMissing {
		return cuda.NvccRelease{}, layout, false
	}
	rel := p.readNvccRelease(ctx, deps)
	if rel.Major == "" {
		return cuda.NvccRelease{}, layout, false
	}
	ok, _ := selection.SatisfiesNvcc(rel)
	return rel, layout, ok
}

func (p Cuda) readNvccRelease(ctx context.Context, deps *Deps) cuda.NvccRelease {
	return p.readNvccReleaseDetailed(ctx, deps).Release
}

func (p Cuda) readNvccReleaseDetailed(ctx context.Context, deps *Deps) nvccProbeResult {
	if p.NvccProbeFn != nil {
		return nvccProbeResult{
			Release: p.NvccProbeFn(ctx, deps),
			Path:    "injected",
		}
	}
	nvcc := filepath.Join(cuda.CudaRoot, "bin", "nvcc")
	if _, err := os.Stat(nvcc); err != nil {
		nvcc = "nvcc"
	}
	start := time.Now()
	limit := p.nvccVersionTimeout()
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{nvcc, "--version"},
		LogFile: "-",
		Timeout: limit,
	})
	if res.Err != nil {
		reason := "nvcc_version_failed"
		if res.TimedOut {
			reason = "nvcc_version_timeout"
		}
		return nvccProbeResult{
			Path: nvcc,
			Error: &cudaStageError{
				Stage:      "nvcc_version",
				Reason:     reason,
				Limit:      limit,
				Elapsed:    elapsedOr(res.Duration, start),
				StdoutTail: tailLines(res.Stdout, 8),
				StderrTail: tailLines(res.Stderr, 8),
				Err:        res.Err,
			},
		}
	}
	return nvccProbeResult{
		Release: cuda.ParseNvccRelease(res.Stdout),
		Path:    nvcc,
	}
}

// detectCudaLayout returns the on-disk evidence of an installed CUDA
// toolkit. It accepts EITHER:
//
//   - LayoutNvidiaCanonical (/usr/local/cuda/{bin/nvcc,include,lib64}),
//     produced by NVIDIA's runfile and the official CUDA apt repo, OR
//   - LayoutUbuntuArchive (nvcc on PATH + /usr/include/cuda_runtime.h
//     OR /usr/lib/cuda/include + libcudart somewhere reasonable),
//     produced by Ubuntu's `nvidia-cuda-toolkit` package.
//
// On non-Linux developer hosts the probe defaults to "missing" so
// tests that exercise the verifier must wire CudaLayoutFn explicitly.
func (p Cuda) detectCudaLayout() cuda.CudaLayout {
	if p.CudaLayoutFn != nil {
		return p.CudaLayoutFn()
	}
	if runtime.GOOS != "linux" {
		return cuda.CudaLayout{Kind: cuda.LayoutMissing, Notes: []string{"non-linux developer host"}}
	}
	canonical := true
	canonicalNvcc := filepath.Join(cuda.CudaRoot, "bin", "nvcc")
	canonicalInc := filepath.Join(cuda.CudaRoot, "include")
	canonicalLib := filepath.Join(cuda.CudaRoot, "lib64")
	for _, sub := range []string{canonicalNvcc, canonicalInc, canonicalLib} {
		if _, err := os.Stat(sub); err != nil {
			canonical = false
			break
		}
	}
	if canonical {
		return cuda.CudaLayout{
			Kind: cuda.LayoutNvidiaCanonical, NvccPath: canonicalNvcc,
			Headers: canonicalInc, Libs: canonicalLib,
		}
	}
	// Ubuntu archive layout: nvcc on PATH, headers + libs in either
	// of the two locations the package ships them.
	out := cuda.CudaLayout{Kind: cuda.LayoutMissing}
	if _, err := os.Stat("/usr/bin/nvcc"); err == nil {
		out.NvccPath = "/usr/bin/nvcc"
	}
	for _, h := range []string{"/usr/include/cuda_runtime.h", "/usr/lib/cuda/include"} {
		if _, err := os.Stat(h); err == nil {
			out.Headers = h
			break
		}
	}
	for _, l := range []string{"/usr/lib/cuda/lib64", "/usr/lib/x86_64-linux-gnu"} {
		// We accept either an explicit /usr/lib/cuda/lib64 dir OR a
		// matching libcudart.so* under multiarch lib. Stat the dir
		// + check the libcudart pattern lazily.
		info, err := os.Stat(l)
		if err != nil || !info.IsDir() {
			continue
		}
		if l == "/usr/lib/cuda/lib64" {
			out.Libs = l
			break
		}
		// Look for libcudart.so* under multiarch.
		entries, derr := os.ReadDir(l)
		if derr != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "libcudart.so") {
				out.Libs = filepath.Join(l, name)
				break
			}
		}
		if out.Libs != "" {
			break
		}
	}
	if out.NvccPath != "" && out.Headers != "" && out.Libs != "" {
		out.Kind = cuda.LayoutUbuntuArchive
		out.Notes = append(out.Notes, "Ubuntu archive nvidia-cuda-toolkit layout")
	}
	return out
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

// runSmokeIfRequested writes the smoke .cu, compiles it with nvcc, and
// runs the compiled binary under a short timeout. Mutates `details`
// with compile_smoke_test* and runtime_smoke_test* keys. The caller
// decides whether a returned error is fatal (cuda.mode=required) or a
// degraded terminal skip (cuda.mode=optional / auto profiles).
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
	compileTimeout := p.smokeCompileTimeout()
	runTimeout := p.smokeRunTimeout()
	details["smoke_compile_timeout"] = compileTimeout.String()
	details["smoke_run_timeout"] = runTimeout.String()
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
	details["smoke_source"] = src
	details["smoke_binary"] = bin
	details["smoke_source_generated"] = true

	log.Info("phase cuda: smoke compile start", "src", src, "bin", bin, "timeout", compileTimeout.String())
	details["smoke_compile_started"] = true
	if err := p.smokeCompile(ctx, deps, src, bin); err != nil {
		err = normalizeCudaStageError("smoke_compile", "smoke_compile_failed", compileTimeout, err)
		recordCudaStageError(details, err, "smoke_compile")
		details["compile_smoke_test_passed"] = false
		details["compile_smoke_test_error"] = err.Error()
		return fmt.Errorf("nvcc compile %s: %w", src, err)
	}
	details["compile_smoke_test_passed"] = true
	log.Info("phase cuda: smoke compile success", "bin", bin)

	log.Info("phase cuda: smoke run start", "bin", bin, "timeout", runTimeout.String())
	details["smoke_run_started"] = true
	if runErr := p.smokeRun(ctx, deps, bin); runErr != nil {
		runErr = normalizeCudaStageError("smoke_run", "smoke_run_failed", runTimeout, runErr)
		recordCudaStageError(details, runErr, "smoke_run")
		log.Warn("phase cuda: smoke binary run failed", "err", runErr)
		details["runtime_smoke_test_passed"] = false
		details["runtime_smoke_test_error"] = runErr.Error()
		return fmt.Errorf("run CUDA smoke binary %s: %w", bin, runErr)
	}
	details["runtime_smoke_test_passed"] = true
	log.Info("phase cuda: smoke run success", "bin", bin)
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
	start := time.Now()
	limit := p.smokeCompileTimeout()
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{nvcc, "-o", bin, src},
		Sudo:    true,
		Timeout: limit,
	})
	if res.Err != nil {
		reason := "smoke_compile_failed"
		if res.TimedOut {
			reason = "smoke_compile_timeout"
		}
		return &cudaStageError{
			Stage:      "smoke_compile",
			Reason:     reason,
			Limit:      limit,
			Elapsed:    elapsedOr(res.Duration, start),
			StdoutTail: tailLines(res.Stdout, 12),
			StderrTail: tailLines(res.Stderr, 12),
			Err:        fmt.Errorf("%s -o %s %s: %w", nvcc, bin, src, res.Err),
		}
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
	start := time.Now()
	limit := p.smokeRunTimeout()
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{bin},
		Sudo:    true,
		Timeout: limit,
	})
	if res.Err != nil {
		reason := "smoke_run_failed"
		if res.TimedOut {
			reason = "smoke_run_timeout"
		}
		return &cudaStageError{
			Stage:      "smoke_run",
			Reason:     reason,
			Limit:      limit,
			Elapsed:    elapsedOr(res.Duration, start),
			StdoutTail: tailLines(res.Stdout, 8),
			StderrTail: tailLines(res.Stderr, 8),
			Err:        fmt.Errorf("run %s: %w", bin, res.Err),
		}
	}
	return nil
}

func normalizeCudaStageError(stage, fallbackReason string, limit time.Duration, err error) error {
	if err == nil {
		return nil
	}
	var se *cudaStageError
	if errors.As(err, &se) {
		return se
	}
	return &cudaStageError{
		Stage:   stage,
		Reason:  fallbackReason,
		Limit:   limit,
		Elapsed: 0,
		Err:     err,
	}
}

func recordCudaStageError(details map[string]any, err error, prefix string) {
	if details == nil || err == nil {
		return
	}
	var se *cudaStageError
	if !errors.As(err, &se) || se == nil {
		details[prefix+"_error"] = err.Error()
		return
	}
	details[prefix+"_reason"] = se.Reason
	details[prefix+"_error"] = se.Error()
	if se.Elapsed > 0 {
		details[prefix+"_elapsed"] = se.Elapsed.Round(time.Millisecond).String()
	}
	if se.Limit > 0 {
		details[prefix+"_limit"] = se.Limit.String()
	}
	if se.StdoutTail != "" {
		details[prefix+"_stdout_tail"] = se.StdoutTail
	}
	if se.StderrTail != "" {
		details[prefix+"_stderr_tail"] = se.StderrTail
	}
	// Compatibility aliases for the live-run debug fields the operator
	// asked to see in state.json after a CUDA smoke timeout.
	switch prefix {
	case "smoke_compile":
		details["smoke_compile_elapsed"] = details[prefix+"_elapsed"]
		details["smoke_compile_stdout_tail"] = se.StdoutTail
		details["smoke_compile_stderr_tail"] = se.StderrTail
	case "smoke_run":
		details["smoke_run_elapsed"] = details[prefix+"_elapsed"]
		details["smoke_stdout_tail"] = se.StdoutTail
		details["smoke_stderr_tail"] = se.StderrTail
	}
}

func recordCudaDegraded(details map[string]any, plan cuda.PlanResult, err error) {
	if details == nil {
		return
	}
	details["cuda_status"] = "degraded"
	details["cuda_required"] = plan.FailsDeployOnError
	details["cuda_degraded_reason"] = cudaFailureReason(err)
}

func cudaFailureReason(err error) string {
	var se *cudaStageError
	if errors.As(err, &se) && se != nil && se.Reason != "" {
		return se.Reason
	}
	return "cuda_validation_failed"
}

func cudaFailureSummary(err error) string {
	var se *cudaStageError
	if errors.As(err, &se) && se != nil {
		return se.Error()
	}
	return "CUDA validation failed"
}

func secondsString(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

func elapsedOr(d time.Duration, started time.Time) time.Duration {
	if d > 0 {
		return d
	}
	if !started.IsZero() {
		return time.Since(started)
	}
	return 0
}

func (p Cuda) nvccVersionTimeout() time.Duration {
	if p.NvccVersionTimeoutOverride > 0 {
		return p.NvccVersionTimeoutOverride
	}
	return cudaNvccVersionTimeout
}

func (p Cuda) nvidiaSmiTimeout() time.Duration {
	if p.NvidiaSmiTimeoutOverride > 0 {
		return p.NvidiaSmiTimeoutOverride
	}
	return cudaNvidiaSmiTimeout
}

func (p Cuda) smokeCompileTimeout() time.Duration {
	if p.SmokeCompileTimeoutOverride > 0 {
		return p.SmokeCompileTimeoutOverride
	}
	return cudaSmokeCompileTimeout
}

func (p Cuda) smokeRunTimeout() time.Duration {
	if p.SmokeRunTimeoutOverride > 0 {
		return p.SmokeRunTimeoutOverride
	}
	return cudaSmokeRunTimeout
}

func (p Cuda) probeNvidiaSmi(ctx context.Context, deps *Deps, details map[string]any, log *slog.Logger) {
	if details == nil || deps == nil || deps.Runner == nil {
		return
	}
	if runtime.GOOS != "linux" {
		details["nvidia_smi_works"] = false
		details["nvidia_smi_reason"] = "skipped_non_linux"
		return
	}
	limit := p.nvidiaSmiTimeout()
	if log != nil {
		log.Info("phase cuda: probing nvidia-smi", "timeout", limit.String())
	}
	start := time.Now()
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"},
		LogFile: "-",
		Timeout: limit,
	})
	details["nvidia_smi_timeout"] = limit.String()
	details["nvidia_smi_elapsed"] = elapsedOr(res.Duration, start).Round(time.Millisecond).String()
	details["nvidia_smi_stdout_tail"] = tailLines(res.Stdout, 8)
	details["nvidia_smi_stderr_tail"] = tailLines(res.Stderr, 8)
	if res.Err == nil {
		details["nvidia_smi_works"] = true
		details["nvidia_smi"] = strings.TrimSpace(res.Stdout)
		return
	}
	details["nvidia_smi_works"] = false
	reason := "nvidia_smi_failed"
	if res.TimedOut {
		reason = "nvidia_smi_timeout"
	}
	details["nvidia_smi_reason"] = reason
	details["nvidia_smi_error"] = (&cudaStageError{
		Stage:      "nvidia_smi",
		Reason:     reason,
		Limit:      limit,
		Elapsed:    elapsedOr(res.Duration, start),
		StdoutTail: tailLines(res.Stdout, 8),
		StderrTail: tailLines(res.Stderr, 8),
		Err:        res.Err,
	}).Error()
}

// -----------------------------------------------------------------------------
// repo probing helpers
// -----------------------------------------------------------------------------

// ubuntuCodename maps a VERSION_ID to its codename via the existing
// internal/ubuntu table. Returns "" for unknown versions so the
// state-details field becomes empty rather than misleading.
func ubuntuCodename(version string) string {
	return ubuntuCodenameFromTable(version)
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
	return p.failWithDetails(deps, plan, reason, err, nil)
}

func (p Cuda) failWithDetails(deps *Deps, plan cuda.PlanResult, reason string, err error, extra map[string]any) error {
	deps.State.MarkFailed(CudaName, reason, err, true)
	d := map[string]any{
		"mode":     string(plan.Mode),
		"fatal":    true,
		"plan_msg": plan.Rationale,
	}
	for k, v := range extra {
		d[k] = v
	}
	deps.State.Get(CudaName).Details = d
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

// stringOr returns details[key] if it's a non-empty string, else def.
func stringOr(details map[string]any, key, def string) string {
	if v, ok := details[key].(string); ok && v != "" {
		return v
	}
	return def
}

// boolOr returns details[key] if it's a bool, else def.
func boolOr(details map[string]any, key string, def bool) bool {
	if v, ok := details[key].(bool); ok {
		return v
	}
	return def
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
