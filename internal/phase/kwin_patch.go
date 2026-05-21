package phase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

const KWinPatchName = "kwin_patch"

const (
	defaultDeb822SourcesPath     = "/etc/apt/sources.list.d/ubuntu.sources"
	defaultLegacySourcesListPath = "/etc/apt/sources.list"
	defaultDebSrcBackupDir       = "/var/lib/clouddeploy/backups"
)

type DebSrcResult struct {
	EnabledBefore        bool
	Modified             bool
	BackupPath           string
	AptUpdateAfterDebSrc bool
}

type debSrcOptions struct {
	Deb822SourcesPath     string
	LegacySourcesListPath string
	BackupDir             string
	Now                   func() time.Time
}

type KWinPatch struct {
	MarkerPath string
	NowFn      func() time.Time

	Deb822SourcesPath     string
	LegacySourcesListPath string
	DebSrcBackupDir       string

	EnsureDebSrcFn   func(context.Context, *Deps) (DebSrcResult, error)
	VerifyPackagesFn func(context.Context, *Deps, []string, bool) error
	SourceVersionFn  func(context.Context, *Deps, config.KWinConfig) (string, error)
	ValidateFn       func(context.Context, *Deps, config.KWinConfig, string) error
	BuildInstallFn   func(context.Context, *Deps, config.KWinConfig, string, string) ([]string, error)
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

	// Packaged-mode short-circuit BEFORE any patch-file checks.
	//
	// Live-VM regression: a profile with `kwin.source_mode=packaged`
	// + `allow_packaged_fallback=true` + `require_patch=false` (i.e.
	// "use stock KWin, don't bother with the HDR patch on this box")
	// was failing on patch-file-missing before ever reaching this
	// gate, which is the opposite of what the operator asked for.
	//
	// Order is now:
	//   1. source_mode=packaged -> patchFailure() (the helper converts
	//      to MarkSkipped("packaged fallback allowed") when both
	//      `allow_packaged_fallback=true` AND `require_patch=false`,
	//      so the phase succeeds-by-skip in the safe case).
	//   2. Otherwise, stat + hash the patch file as before.
	if strings.EqualFold(cfg.SourceMode, "packaged") {
		err := fmt.Errorf("kwin.source_mode=packaged cannot satisfy kwin.patched_hdr=true with require_patch=%v", cfg.RequirePatchEnabled())
		return p.patchFailure(deps, details, err, "packaged KWin requested", cfg)
	}

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

	debSrc, err := p.ensureDebSrcEnabled(ctx, deps)
	if err != nil {
		return p.fail(deps, details, "enable deb-src repositories", err)
	}
	details["deb_src_enabled_before"] = debSrc.EnabledBefore
	details["deb_src_modified"] = debSrc.Modified
	details["deb_src_backup_path"] = debSrc.BackupPath
	details["apt_update_after_deb_src"] = debSrc.AptUpdateAfterDebSrc

	sourceVersion, err := p.sourceVersion(ctx, deps, cfg)
	if err != nil {
		return p.fail(deps, details, "detect kwin source version", err)
	}
	details["kwin_source_version"] = sourceVersion

	markerPath := p.markerPath()
	details["marker_path"] = markerPath
	if marker, err := kwinpkg.ReadMarker(markerPath); err == nil && marker.Matches(patchSHA, sourceVersion, cfg.InstallMode, cfg.RuntimeBin) {
		if deps.DryRun {
			details["marker_matched"] = true
			details["installed_packages"] = marker.InstalledPackages
			deps.State.MarkDone(KWinPatchName, details)
			_ = deps.PersistState()
			return nil
		}
		verifyErr := p.verifyPackages(ctx, deps, marker.InstalledPackages, cfg.HoldPackages)
		if verifyErr == nil {
			details["marker_matched"] = true
			details["installed_packages"] = marker.InstalledPackages
			deps.State.MarkDone(KWinPatchName, details)
			_ = deps.PersistState()
			return nil
		}
		details["marker_matched"] = true
		details["marker_verify_failed"] = verifyErr.Error()
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

func (p KWinPatch) ensureDebSrcEnabled(ctx context.Context, deps *Deps) (DebSrcResult, error) {
	if p.EnsureDebSrcFn != nil {
		return p.EnsureDebSrcFn(ctx, deps)
	}
	opts := debSrcOptions{
		Deb822SourcesPath:     p.Deb822SourcesPath,
		LegacySourcesListPath: p.LegacySourcesListPath,
		BackupDir:             p.DebSrcBackupDir,
		Now:                   p.now,
	}
	return ensureDebSrcEnabled(ctx, deps, opts)
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
	env := []string{fmt.Sprintf("DEB_BUILD_OPTIONS=nocheck parallel=%d", runtime.NumCPU()), "DEB_BUILD_PROFILES=nocheck"}
	if err := run(ctx, deps, src, []string{"dpkg-buildpackage", "-b", "-uc", "-us"}, 2*time.Hour, false, env...); err != nil {
		return nil, err
	}
	debs, err := filepath.Glob(filepath.Join(work, "*.deb"))
	if err != nil || len(debs) == 0 {
		return nil, fmt.Errorf("no generated .deb packages found under %s", work)
	}
	// Live VM (2026-05-21): the rebuilt patched .debs carry the
	// archive's source version (e.g. 6.4.5-0ubuntu3) which apt-get
	// install treats as a downgrade vs. whatever the distro is
	// currently advertising. Without --allow-downgrades the install
	// aborts with "Packages were downgraded and -y was used without
	// --allow-downgrades", and the patched stack never lands.
	//
	// --allow-change-held-packages covers the re-run case: if the
	// previous attempt already apt-mark hold'd the same packages,
	// the next `apt-get install` would otherwise refuse to touch
	// them. Both flags are safe because:
	//   - the only -y install in this codepath is THIS file's
	//     locally-built KWin .debs (operator-explicit intent);
	//   - we re-hold immediately after via the HoldPackages block.
	args := append(
		[]string{"apt-get", "-y", "--allow-downgrades", "--allow-change-held-packages", "install"},
		debs...,
	)
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

func run(ctx context.Context, deps *Deps, cwd string, argv []string, timeout time.Duration, sudo bool, extraEnv ...string) error {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    argv,
		Cwd:     cwd,
		Sudo:    sudo,
		Timeout: timeout,
		DryRun:  deps.DryRun,
		Env:     append([]string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}, extraEnv...),
	})
	if res.Err != nil {
		return fmt.Errorf("%s failed: %w stderr=%q", strings.Join(argv, " "), res.Err, tailLines(res.Stderr, 10))
	}
	return nil
}

func ensureDebSrcEnabled(ctx context.Context, deps *Deps, opts debSrcOptions) (DebSrcResult, error) {
	deb822Path := opts.Deb822SourcesPath
	if strings.TrimSpace(deb822Path) == "" {
		deb822Path = defaultDeb822SourcesPath
	}
	legacyPath := opts.LegacySourcesListPath
	if strings.TrimSpace(legacyPath) == "" {
		legacyPath = defaultLegacySourcesListPath
	}
	backupDir := opts.BackupDir
	if strings.TrimSpace(backupDir) == "" {
		backupDir = defaultDebSrcBackupDir
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	result := DebSrcResult{}
	var backups []string

	if text, ok, err := readOptionalTextFile(deb822Path); err != nil {
		return result, err
	} else if ok {
		rewritten, modified, enabled := rewriteDeb822DebSrc(text)
		result.EnabledBefore = result.EnabledBefore || enabled
		if modified {
			backup, err := backupFile(deb822Path, backupDir, nowFn)
			if err != nil {
				return result, err
			}
			backups = append(backups, backup)
			if err := os.WriteFile(deb822Path, []byte(rewritten), 0o644); err != nil {
				return result, fmt.Errorf("write %s: %w", deb822Path, err)
			}
			result.Modified = true
		}
	}

	if text, ok, err := readOptionalTextFile(legacyPath); err != nil {
		return result, err
	} else if ok {
		rewritten, modified, enabled := rewriteLegacySourcesDebSrc(text)
		result.EnabledBefore = result.EnabledBefore || enabled
		if modified {
			backup, err := backupFile(legacyPath, backupDir, nowFn)
			if err != nil {
				return result, err
			}
			backups = append(backups, backup)
			if err := os.WriteFile(legacyPath, []byte(rewritten), 0o644); err != nil {
				return result, fmt.Errorf("write %s: %w", legacyPath, err)
			}
			result.Modified = true
		}
	}

	result.BackupPath = strings.Join(backups, ";")
	if result.Modified {
		if err := run(ctx, deps, "", []string{"apt-get", "update"}, 20*time.Minute, true); err != nil {
			return result, err
		}
		result.AptUpdateAfterDebSrc = true
	}
	return result, nil
}

func readOptionalTextFile(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), true, nil
}

func rewriteDeb822DebSrc(text string) (string, bool, bool) {
	lines := splitLinesNoTrailingEmpty(text)
	modified := false
	enabledBefore := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Types") {
			continue
		}
		fields := strings.Fields(rest)
		hasDeb := false
		hasDebSrc := false
		for _, f := range fields {
			switch f {
			case "deb":
				hasDeb = true
			case "deb-src":
				hasDebSrc = true
			}
		}
		if hasDebSrc {
			enabledBefore = true
		}
		if hasDeb && !hasDebSrc {
			fields = append(fields, "deb-src")
			lines[i] = key + ": " + strings.Join(fields, " ")
			modified = true
		}
	}
	return strings.Join(lines, "\n"), modified, enabledBefore
}

func rewriteLegacySourcesDebSrc(text string) (string, bool, bool) {
	lines := splitLinesNoTrailingEmpty(text)
	existing := map[string]bool{}
	var toAdd []string
	enabledBefore := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "deb-src" {
			enabledBefore = true
			existing[strings.TrimSpace(strings.TrimPrefix(trimmed, "deb-src"))] = true
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 || fields[0] != "deb" || !looksLikeUbuntuArchiveLine(trimmed) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "deb"))
		if existing[rest] {
			continue
		}
		toAdd = append(toAdd, "deb-src "+rest)
		existing[rest] = true
	}
	if len(toAdd) == 0 {
		return strings.Join(lines, "\n"), false, enabledBefore
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") && out != "" {
		out += "\n"
	}
	out += strings.Join(toAdd, "\n") + "\n"
	return out, true, enabledBefore
}

func looksLikeUbuntuArchiveLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "ubuntu")
}

func splitLinesNoTrailingEmpty(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func backupFile(path, backupDir string, nowFn func() time.Time) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", backupDir, err)
	}
	stamp := nowFn().UTC().Format("20060102T150405Z")
	dst := filepath.Join(backupDir, filepath.Base(path)+"."+stamp)
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(backupDir, fmt.Sprintf("%s.%s.%d", filepath.Base(path), stamp, i))
	}
	src, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open backup source %s: %w", path, err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("create backup %s: %w", dst, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("copy backup %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close backup %s: %w", dst, err)
	}
	return dst, nil
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

func (p KWinPatch) verifyPackages(ctx context.Context, deps *Deps, pkgs []string, checkHold bool) error {
	if p.VerifyPackagesFn != nil {
		return p.VerifyPackagesFn(ctx, deps, pkgs, checkHold)
	}
	if len(pkgs) == 0 {
		return nil
	}
	args := append([]string{"dpkg-query", "-W", "--showformat=${db:Status-Status}\n"}, pkgs...)
	res := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: args, Timeout: 10 * time.Second})
	if res.Err != nil {
		return res.Err
	}
	for _, status := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if status != "installed" {
			return errors.New("package missing or not fully installed")
		}
	}
	if checkHold {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: []string{"apt-mark", "showhold"}, Timeout: 10 * time.Second})
		if res.Err != nil {
			return res.Err
		}
		held := make(map[string]bool)
		for _, line := range strings.Split(res.Stdout, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				held[line] = true
			}
		}
		for _, pkg := range pkgs {
			if !held[pkg] {
				return fmt.Errorf("package %s not on hold", pkg)
			}
		}
	}
	return nil
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
