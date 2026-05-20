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
