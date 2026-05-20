package phase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

func kwinPatchProfile(t *testing.T, patchPath string) *config.Profile {
	t.Helper()
	require := true
	p := desktopProfile()
	p.KWin = config.KWinConfig{
		PatchedHDR:    true,
		Patch:         patchPath,
		BuildDir:      filepath.Join(t.TempDir(), "kwin-build"),
		RequirePatch:  &require,
		ValidatePatch: true,
		HoldPackages:  true,
	}
	return p
}

func writeTestPatch(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kwin.patch")
	if err := os.WriteFile(path, []byte("diff --git a/a b/a\n"), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	return path
}

func TestKWinPatch_SkipsWhenPatchedHDRFalse(t *testing.T) {
	p := desktopProfile()
	p.KWin = config.KWinConfig{PatchedHDR: false}
	deps := newDeps(t, p, nil)

	ph := KWinPatch{}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := deps.State.Get(KWinPatchName)
	if got.Status != state.StatusSkipped {
		t.Fatalf("status: got %q want skipped", got.Status)
	}
	if got.Reason != "kwin.patched_hdr=false" {
		t.Fatalf("reason: got %q", got.Reason)
	}
}

func TestKWinPatch_MissingPatchFatalWhenRequired(t *testing.T) {
	deps := newDeps(t, kwinPatchProfile(t, filepath.Join(t.TempDir(), "missing.patch")), nil)

	err := (KWinPatch{
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			return "6.4.5-0ubuntu3", nil
		},
	}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error")
	}
	got := deps.State.Get(KWinPatchName)
	if got.Status != state.StatusFailedFatal {
		t.Fatalf("status: got %q want failed_fatal", got.Status)
	}
	if !strings.Contains(got.LastError, "kwin patch file missing") {
		t.Fatalf("last error should mention missing patch, got %q", got.LastError)
	}
}

func TestKWinPatch_DryRunDoesNotRequireAptSource(t *testing.T) {
	patchPath := writeTestPatch(t)
	deps := newDeps(t, kwinPatchProfile(t, patchPath), nil)
	called := false

	err := (KWinPatch{
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			called = true
			return "", errors.New("apt-cache should not run in dry-run")
		},
	}).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatalf("dry-run should not shell out to source-version detection")
	}
	got := deps.State.Get(KWinPatchName)
	if got.Status != state.StatusDone {
		t.Fatalf("status: got %q want done", got.Status)
	}
	if got.Details["dry_run"] != true {
		t.Fatalf("dry_run detail missing: %#v", got.Details)
	}
}

func TestEnsureDebSrcEnabled_Deb822AddsDebSrc(t *testing.T) {
	dir := t.TempDir()
	sources := filepath.Join(dir, "ubuntu.sources")
	legacy := filepath.Join(dir, "missing-sources.list")
	backupDir := filepath.Join(dir, "backups")
	body := strings.Join([]string{
		"# kept comment",
		"Types: deb",
		"URIs: http://archive.ubuntu.com/ubuntu",
		"Suites: questing questing-updates",
		"Components: main restricted universe multiverse",
		"",
	}, "\n")
	if err := os.WriteFile(sources, []byte(body), 0o644); err != nil {
		t.Fatalf("write sources: %v", err)
	}
	deps := newDeps(t, kwinPatchProfile(t, writeTestPatch(t)), nil)
	deps.DryRun = true

	res, err := ensureDebSrcEnabled(context.Background(), deps, debSrcOptions{
		Deb822SourcesPath:     sources,
		LegacySourcesListPath: legacy,
		BackupDir:             backupDir,
		Now:                   func() time.Time { return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("ensureDebSrcEnabled: %v", err)
	}
	if res.EnabledBefore {
		t.Fatalf("EnabledBefore got true want false")
	}
	if !res.Modified || !res.AptUpdateAfterDebSrc {
		t.Fatalf("modified/update flags: %#v", res)
	}
	if res.BackupPath == "" {
		t.Fatalf("backup path missing: %#v", res)
	}
	got, err := os.ReadFile(sources)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	if !strings.Contains(string(got), "Types: deb deb-src") {
		t.Fatalf("deb-src not added:\n%s", string(got))
	}
	if !strings.Contains(string(got), "# kept comment") {
		t.Fatalf("comment not preserved:\n%s", string(got))
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Fatalf("backup was not written at %s: %v", res.BackupPath, err)
	}
}

func TestEnsureDebSrcEnabled_Deb822AlreadyEnabledUnchanged(t *testing.T) {
	dir := t.TempDir()
	sources := filepath.Join(dir, "ubuntu.sources")
	legacy := filepath.Join(dir, "missing-sources.list")
	backupDir := filepath.Join(dir, "backups")
	body := "Types: deb deb-src\nURIs: http://archive.ubuntu.com/ubuntu\n"
	if err := os.WriteFile(sources, []byte(body), 0o644); err != nil {
		t.Fatalf("write sources: %v", err)
	}
	deps := newDeps(t, kwinPatchProfile(t, writeTestPatch(t)), nil)
	deps.DryRun = true

	res, err := ensureDebSrcEnabled(context.Background(), deps, debSrcOptions{
		Deb822SourcesPath:     sources,
		LegacySourcesListPath: legacy,
		BackupDir:             backupDir,
	})
	if err != nil {
		t.Fatalf("ensureDebSrcEnabled: %v", err)
	}
	if !res.EnabledBefore {
		t.Fatalf("EnabledBefore got false want true")
	}
	if res.Modified || res.AptUpdateAfterDebSrc || res.BackupPath != "" {
		t.Fatalf("already-enabled source should be unchanged: %#v", res)
	}
	got, err := os.ReadFile(sources)
	if err != nil {
		t.Fatalf("read sources: %v", err)
	}
	if string(got) != body {
		t.Fatalf("sources changed unexpectedly:\n%s", string(got))
	}
}

func TestEnsureDebSrcEnabled_LegacySourcesAddsMissingDebSrc(t *testing.T) {
	dir := t.TempDir()
	deb822 := filepath.Join(dir, "missing-ubuntu.sources")
	legacy := filepath.Join(dir, "sources.list")
	body := strings.Join([]string{
		"# local mirror",
		"deb http://archive.ubuntu.com/ubuntu questing main restricted",
		"deb http://security.ubuntu.com/ubuntu questing-security main",
		"",
	}, "\n")
	if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
		t.Fatalf("write sources.list: %v", err)
	}
	deps := newDeps(t, kwinPatchProfile(t, writeTestPatch(t)), nil)
	deps.DryRun = true

	res, err := ensureDebSrcEnabled(context.Background(), deps, debSrcOptions{
		Deb822SourcesPath:     deb822,
		LegacySourcesListPath: legacy,
		BackupDir:             filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatalf("ensureDebSrcEnabled: %v", err)
	}
	if res.EnabledBefore {
		t.Fatalf("EnabledBefore got true want false")
	}
	if !res.Modified || !res.AptUpdateAfterDebSrc {
		t.Fatalf("modified/update flags: %#v", res)
	}
	got, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("read sources.list: %v", err)
	}
	for _, want := range []string{
		"deb-src http://archive.ubuntu.com/ubuntu questing main restricted",
		"deb-src http://security.ubuntu.com/ubuntu questing-security main",
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in:\n%s", want, string(got))
		}
	}
}

func TestKWinPatch_ValidationFailureFatalWhenRequired(t *testing.T) {
	patchPath := writeTestPatch(t)
	deps := newDeps(t, kwinPatchProfile(t, patchPath), nil)
	deps.DryRun = false
	errValidation := errors.New("patch no longer applies")

	err := (KWinPatch{
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			return "6.4.5-0ubuntu3", nil
		},
		ValidateFn: func(context.Context, *Deps, config.KWinConfig, string) error {
			return errValidation
		},
	}).Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fatal error")
	}
	got := deps.State.Get(KWinPatchName)
	if got.Status != state.StatusFailedFatal {
		t.Fatalf("status: got %q want failed_fatal", got.Status)
	}
	if got.Reason != "patch validation failed" {
		t.Fatalf("reason: got %q", got.Reason)
	}
}

func TestKWinPatch_ValidationFailureAllowsPackagedFallback(t *testing.T) {
	patchPath := writeTestPatch(t)
	require := false
	p := kwinPatchProfile(t, patchPath)
	p.KWin.RequirePatch = &require
	p.KWin.AllowPackagedFallback = true
	deps := newDeps(t, p, nil)
	deps.DryRun = false

	err := (KWinPatch{
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			return "6.4.5-0ubuntu3", nil
		},
		ValidateFn: func(context.Context, *Deps, config.KWinConfig, string) error {
			return errors.New("dry-run rejected hunk")
		},
	}).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := deps.State.Get(KWinPatchName)
	if got.Status != state.StatusSkipped {
		t.Fatalf("status: got %q want skipped", got.Status)
	}
	if got.Details["fallback_reason"] == "" {
		t.Fatalf("fallback_reason missing from details: %#v", got.Details)
	}
}

func TestKWinPatch_MatchingMarkerSkipsRebuild(t *testing.T) {
	patchPath := writeTestPatch(t)
	deps := newDeps(t, kwinPatchProfile(t, patchPath), nil)
	deps.DryRun = false
	sum, err := kwinpkg.HashFile(patchPath)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	markerPath := filepath.Join(t.TempDir(), "kwin-patch.json")
	if err := kwinpkg.WriteMarker(markerPath, kwinpkg.Marker{
		PatchPath:         patchPath,
		PatchSHA256:       sum,
		KWinSourceVersion: "6.4.5-0ubuntu3",
		BuildDir:          "/opt/clouddeploy-kwin-src",
		InstalledPackages: []string{"kwin-wayland"},
		InstallMode:       "packages",
		Timestamp:         time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	buildCalled := false

	err = (KWinPatch{
		MarkerPath: markerPath,
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			return "6.4.5-0ubuntu3", nil
		},
		ValidateFn: func(context.Context, *Deps, config.KWinConfig, string) error {
			t.Fatalf("validate should not run when marker matches")
			return nil
		},
		BuildInstallFn: func(context.Context, *Deps, config.KWinConfig, string, string) ([]string, error) {
			buildCalled = true
			return nil, nil
		},
	}).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buildCalled {
		t.Fatalf("build/install should not run when marker matches")
	}
	if got := deps.State.Get(KWinPatchName).Status; got != state.StatusDone {
		t.Fatalf("status: got %q want done", got)
	}
}

func TestKWinPatch_MarkerMismatchRebuilds(t *testing.T) {
	patchPath := writeTestPatch(t)
	deps := newDeps(t, kwinPatchProfile(t, patchPath), nil)
	deps.DryRun = false
	markerPath := filepath.Join(t.TempDir(), "kwin-patch.json")
	if err := kwinpkg.WriteMarker(markerPath, kwinpkg.Marker{
		PatchPath:         patchPath,
		PatchSHA256:       "old",
		KWinSourceVersion: "6.4.5-0ubuntu3",
		InstallMode:       "packages",
		Timestamp:         time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	buildCalled := false

	err := (KWinPatch{
		MarkerPath: markerPath,
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			return "6.4.5-0ubuntu3", nil
		},
		ValidateFn: func(context.Context, *Deps, config.KWinConfig, string) error {
			return nil
		},
		BuildInstallFn: func(context.Context, *Deps, config.KWinConfig, string, string) ([]string, error) {
			buildCalled = true
			return []string{"kwin-wayland", "kwin-common"}, nil
		},
		NowFn: func() time.Time { return time.Unix(2, 0) },
	}).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !buildCalled {
		t.Fatalf("build/install should run when marker mismatches")
	}
	got, err := kwinpkg.ReadMarker(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got.PatchSHA256 == "old" {
		t.Fatalf("marker was not updated: %#v", got)
	}
	if got.Profile != "test" {
		t.Fatalf("marker profile: got %q want test", got.Profile)
	}
}

func TestKWinPatch_EnsuresDebSrcBeforeSourceValidateBuild(t *testing.T) {
	patchPath := writeTestPatch(t)
	deps := newDeps(t, kwinPatchProfile(t, patchPath), nil)
	deps.DryRun = false
	var events []string

	err := (KWinPatch{
		MarkerPath: filepath.Join(t.TempDir(), "kwin-patch.json"),
		EnsureDebSrcFn: func(context.Context, *Deps) (DebSrcResult, error) {
			events = append(events, "ensure-deb-src")
			return DebSrcResult{EnabledBefore: false, Modified: true, BackupPath: "/tmp/ubuntu.sources.bak", AptUpdateAfterDebSrc: true}, nil
		},
		SourceVersionFn: func(context.Context, *Deps, config.KWinConfig) (string, error) {
			events = append(events, "source-version")
			return "6.4.5-0ubuntu3", nil
		},
		ValidateFn: func(context.Context, *Deps, config.KWinConfig, string) error {
			events = append(events, "validate")
			return nil
		},
		BuildInstallFn: func(context.Context, *Deps, config.KWinConfig, string, string) ([]string, error) {
			events = append(events, "build-install")
			return []string{"kwin-wayland"}, nil
		},
	}).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(events, ",")
	want := "ensure-deb-src,source-version,validate,build-install"
	if got != want {
		t.Fatalf("event order got %q want %q", got, want)
	}
	details := deps.State.Get(KWinPatchName).Details
	if details["deb_src_enabled_before"] != false ||
		details["deb_src_modified"] != true ||
		details["deb_src_backup_path"] != "/tmp/ubuntu.sources.bak" ||
		details["apt_update_after_deb_src"] != true {
		t.Fatalf("deb-src details not recorded correctly: %#v", details)
	}
}

func TestPackageNamesFromDebs(t *testing.T) {
	got := packageNamesFromDebs([]string{
		"/tmp/kwin-wayland_6.4.5-1_amd64.deb",
		"/tmp/kwin-common_6.4.5-1_all.deb",
		"/tmp/kwin-wayland_6.4.5-1_amd64.deb",
	})
	want := []string{"kwin-common", "kwin-wayland"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("packageNamesFromDebs: got %v want %v", got, want)
	}
}
