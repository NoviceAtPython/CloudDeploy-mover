package phase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

const KWinPatchName = "kwin_patch"

type KWinPatch struct {
	MarkerPath string
	NowFn      func() time.Time

	SourceVersionFn func(context.Context, *Deps, config.KWinConfig) (string, error)
	ValidateFn      func(context.Context, *Deps, config.KWinConfig, string) error
	BuildInstallFn  func(context.Context, *Deps, config.KWinConfig, string, string) ([]string, error)
}

func (KWinPatch) Name() string { return KWinPatchName }

func (p KWinPatch) Run(ctx context.Context, deps *Deps) error {
	if shouldSkip(deps.State, KWinPatchName) {
		return nil
	}
	cfg := deps.Profile.EffectiveKWin()
	details := map[string]any{
		"patched_hdr":             cfg.PatchedHDR,
		"source_mode":             cfg.SourceMode,
		"install_mode":            cfg.InstallMode,
		"build_dir":               cfg.BuildDir,
		"runtime_bin":             cfg.RuntimeBin,
		"require_patch":           cfg.RequirePatchEnabled(),
		"allow_packaged_fallback": cfg.AllowPackagedFallback,
		"validate_patch":          cfg.ValidatePatch,
		"hold_packages":           cfg.HoldPackages,
	}
	if !cfg.PatchedHDR {
		deps.State.MarkSkipped(KWinPatchName, "kwin.patched_hdr=false")
		deps.State.Get(KWinPatchName).Details = details
		_ = deps.PersistState()
		return nil
	}

	deps.State.MarkRunning(KWinPatchName)
	_ = deps.PersistState()

	patchPath := resolveKWinPatchPath(cfg.Patch)
	details["patch_path"] = patchPath
	if _, err := os.Stat(patchPath); err != nil {
		return p.patchFailure(deps, details, fmt.Errorf("kwin patch file missing: %s: %w", patchPath, err), "patch file missing", cfg)
	}
	patchSHA, err := kwinpkg.HashFile(patchPath)
	if err != nil {
		return p.fail(deps, details, "hash patch", err)
	}
	details["patch_sha256"] = patchSHA

	if deps.DryRun {
		details["dry_run"] = true
		details["kwin_source_version"] = "not probed in dry-run"
		deps.State.MarkDone(KWinPatchName, details)
		_ = deps.PersistState()
		return nil
	}

	sourceVersion, err := p.sourceVersion(ctx, deps, cfg)
	if err != nil {
		return p.fail(deps, details, "detect kwin source version", err)
	}
	details["kwin_source_version"] = sourceVersion

	markerPath := p.markerPath()
	details["marker_path"] = markerPath
	if marker, err := kwinpkg.ReadMarker(markerPath); err == nil && marker.Matches(patchSHA, sourceVersion, cfg.InstallMode, cfg.RuntimeBin) {
		details["marker_matched"] = true
		details["installed_packages"] = marker.InstalledPackages
		deps.State.MarkDone(KWinPatchName, details)
		_ = deps.PersistState()
		return nil
	}

	if strings.EqualFold(cfg.SourceMode, "packaged") {
		return p.patchFailure(deps, details, fmt.Errorf("kwin.source_mode=packaged cannot apply %s", patchPath), "packaged KWin fallback requested", cfg)
	}
	if cfg.InstallMode != "packages" {
		return p.patchFailure(deps, details, fmt.Errorf("kwin.install_mode=%s is not implemented yet; supported now: packages", cfg.InstallMode), "unsupported kwin install mode", cfg)
	}

	if cfg.ValidatePatch {
		if err := p.validate(ctx, deps, cfg, patchPath); err != nil {
			return p.patchFailure(deps, details, err, "patch validation failed", cfg)
		}
		details["patch_validated"] = true
	}

	pkgs, err := p.buildInstall(ctx, deps, cfg, patchPath, patchSHA)
	if err != nil {
		return p.patchFailure(deps, details, err, "build/install patched kwin", cfg)
	}
	sort.Strings(pkgs)
	details["installed_packages"] = pkgs
	marker := kwinpkg.Marker{
		PatchPath:          patchPath,
		PatchSHA256:        patchSHA,
		KWinSourceVersion:  sourceVersion,
		BuildDir:           cfg.BuildDir,
		InstalledPackages:  pkgs,
		InstallMode:        cfg.InstallMode,
		RuntimeBin:         cfg.RuntimeBin,
		Profile:            deps.Profile.Profile,
		Timestamp:          p.now(),
		CloudDeployVersion: os.Getenv("CLOUDDEPLOY_VERSION"),
	}
	if err := kwinpkg.WriteMarker(markerPath, marker); err != nil {
		return p.fail(deps, details, "write kwin patch marker", err)
	}

	deps.State.MarkDone(KWinPatchName, details)
	_ = deps.PersistState()
	return nil
}

func (p KWinPatch) patchFailure(deps *Deps, details map[string]any, err error, reason string, cfg config.KWinConfig) error {
	details["fallback_reason"] = err.Error()
	if cfg.AllowPackagedFallback && !cfg.RequirePatchEnabled() {
		deps.State.MarkSkipped(KWinPatchName, reason+": packaged fallback allowed")
		deps.State.Get(KWinPatchName).Details = details
		_ = deps.PersistState()
		return nil
	}
	return p.fail(deps, details, reason, err)
}

func (p KWinPatch) fail(deps *Deps, details map[string]any, reason string, err error) error {
	details["err"] = err.Error()
	deps.State.MarkFailed(KWinPatchName, reason, err, true)
	deps.State.Get(KWinPatchName).Details = details
	_ = deps.PersistState()
	return fmt.Errorf("phase kwin-patch: %w", err)
}

func (p KWinPatch) markerPath() string {
	if p.MarkerPath != "" {
		return p.MarkerPath
	}
	return kwinpkg.DefaultMarkerPath
}

func (p KWinPatch) now() time.Time {
	if p.NowFn != nil {
		return p.NowFn().UTC()
	}
	return time.Now().UTC()
}

func (p KWinPatch) sourceVersion(ctx context.Context, deps *Deps, cfg config.KWinConfig) (string, error) {
	if p.SourceVersionFn != nil {
		return p.SourceVersionFn(ctx, deps, cfg)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"apt-cache", "showsrc", "kwin"},
		Timeout: 30 * time.Second,
		LogFile: "-",
	})
	if res.Err != nil {
		return "", fmt.Errorf("apt-cache showsrc kwin failed; deb-src may be disabled: %w", res.Err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Version:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("could not find Version in apt-cache showsrc kwin output")
}

func (p KWinPatch) validate(ctx context.Context, deps *Deps, cfg config.KWinConfig, patchPath string) error {
	if p.ValidateFn != nil {
		return p.ValidateFn(ctx, deps, cfg, patchPath)
	}
	work := filepath.Join(cfg.BuildDir, "validate")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	if err := run(ctx, deps, "", []string{"apt-get", "update"}, 20*time.Minute, true); err != nil {
		return err
	}
	if err := run(ctx, deps, work, []string{"apt-get", "source", "kwin"}, 20*time.Minute, false); err != nil {
		return fmt.Errorf("apt source kwin failed; enable deb-src for the active Ubuntu release: %w", err)
	}
	src, err := firstDir(work, "kwin-*")
	if err != nil {
		return err
	}
	patchBytes, err := os.ReadFile(patchPath)
	if err != nil {
		return err
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"patch", "-p1", "--dry-run"},
		Cwd:     src,
		Stdin:   string(patchBytes),
		Timeout: 2 * time.Minute,
		LogFile: "-",
	})
	if res.Err != nil {
		return fmt.Errorf("patch dry-run failed: %w stderr=%q", res.Err, tailLines(res.Stderr, 10))
	}
	return nil
}

func (p KWinPatch) buildInstall(ctx context.Context, deps *Deps, cfg config.KWinConfig, patchPath, patchSHA string) ([]string, error) {
	if p.BuildInstallFn != nil {
		return p.BuildInstallFn(ctx, deps, cfg, patchPath, patchSHA)
	}
	work := filepath.Join(cfg.BuildDir, "build")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	if err := run(ctx, deps, "", []string{"apt-get", "-y", "build-dep", "kwin"}, 45*time.Minute, true); err != nil {
		return nil, fmt.Errorf("apt-get build-dep kwin failed; deb-src may be disabled: %w", err)
	}
	if err := run(ctx, deps, work, []string{"apt-get", "source", "kwin"}, 20*time.Minute, false); err != nil {
		return nil, fmt.Errorf("apt source kwin failed: %w", err)
	}
	src, err := firstDir(work, "kwin-*")
	if err != nil {
		return nil, err
	}
	patchBytes, err := os.ReadFile(patchPath)
	if err != nil {
		return nil, err
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"patch", "-p1"},
		Cwd:     src,
		Stdin:   string(patchBytes),
		Timeout: 2 * time.Minute,
		LogFile: "-",
	})
	if res.Err != nil {
		return nil, fmt.Errorf("patch apply failed: %w stderr=%q", res.Err, tailLines(res.Stderr, 10))
	}
	if err := run(ctx, deps, src, []string{"dpkg-buildpackage", "-b", "-uc", "-us"}, 2*time.Hour, false); err != nil {
		return nil, err
	}
	debs, err := filepath.Glob(filepath.Join(work, "*.deb"))
	if err != nil || len(debs) == 0 {
		return nil, fmt.Errorf("no generated .deb packages found under %s", work)
	}
	args := append([]string{"apt-get", "-y", "install"}, debs...)
	if err := run(ctx, deps, "", args, 45*time.Minute, true); err != nil {
		return nil, err
	}
	pkgs := packageNamesFromDebs(debs)
	if cfg.HoldPackages && len(pkgs) > 0 {
		if err := run(ctx, deps, "", append([]string{"apt-mark", "hold"}, pkgs...), 2*time.Minute, true); err != nil {
			return nil, err
		}
	}
	return pkgs, nil
}

func run(ctx context.Context, deps *Deps, cwd string, argv []string, timeout time.Duration, sudo bool) error {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    argv,
		Cwd:     cwd,
		Sudo:    sudo,
		Timeout: timeout,
		Env: []string{
			"DEBIAN_FRONTEND=noninteractive",
			"APT_LISTCHANGES_FRONTEND=none",
		},
	})
	if res.Err != nil {
		return fmt.Errorf("%s failed: %w stderr=%q", strings.Join(argv, " "), res.Err, tailLines(res.Stderr, 10))
	}
	return nil
}

func resolveKWinPatchPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, path))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), path))
	}
	candidates = append(candidates, filepath.Join("/opt/clouddeploy-mover", path))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return path
}

func firstDir(root, pattern string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		return "", err
	}
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			return m, nil
		}
	}
	return "", fmt.Errorf("no %s directory under %s", pattern, root)
}

func packageNamesFromDebs(debs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, deb := range debs {
		name := filepath.Base(deb)
		if i := strings.IndexByte(name, '_'); i > 0 {
			name = name[:i]
		} else {
			name = strings.TrimSuffix(name, ".deb")
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
