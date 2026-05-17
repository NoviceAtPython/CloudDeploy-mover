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
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// upgradeDeps clones newDeps for the ubuntu-upgrade phase. We want a
// state file path so PersistState doesn't no-op; everything else is
// the in-memory shape from phase_test.go.
func upgradeDeps(t *testing.T, p *config.Profile) *Deps {
	t.Helper()
	d := newDeps(t, p, nil)
	d.StatePath = filepath.Join(t.TempDir(), "state.json")
	d.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	return d
}

// profileForUpgrade returns a minimal profile with the deploy fields
// the ubuntu-upgrade phase consults.
func profileForUpgrade(target string, autoUpgrade, acceptNonLTS bool, directPolicy string) *config.Profile {
	return &config.Profile{
		Profile:       "hdr-4k120",
		UbuntuVersion: target,
		NVIDIA:        config.NVIDIAConfig{DriverMajor: "580"},
		CUDA:          config.CUDAConfig{Mode: "none"},
		Deploy: config.DeployConfig{
			AutoUpgradeUbuntu:        autoUpgrade,
			AcceptNonLTS:             acceptNonLTS,
			DirectAptCodenameUpgrade: directPolicy,
		},
	}
}

func TestUbuntuUpgrade_NoTarget_IsSkipped(t *testing.T) {
	deps := upgradeDeps(t, profileForUpgrade("", false, false, ""))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusSkipped {
		t.Errorf("status: got %q want skipped", got)
	}
}

func TestUbuntuUpgrade_AlreadyAtTarget_IsDone(t *testing.T) {
	deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, ""))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "25.10" },
	}
	if err := ph.Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusDone {
		t.Errorf("status: got %q want done", got)
	}
	d := deps.State.Get(UbuntuUpgradeName).Details
	if d["action"] != "noop" {
		t.Errorf("details.action: got %v want noop", d["action"])
	}
}

func TestUbuntuUpgrade_AutoUpgradeFalse_FailsFatal(t *testing.T) {
	deps := upgradeDeps(t, profileForUpgrade("25.10", false, false, ""))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ubuntu.ErrAutoUpgradeDisabled) {
		t.Errorf("err: got %v want ErrAutoUpgradeDisabled", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
	r := deps.State.Get(UbuntuUpgradeName).Reason
	if !strings.Contains(r, "auto_upgrade_ubuntu") {
		t.Errorf("reason should name the knob: %q", r)
	}
}

func TestUbuntuUpgrade_NonLTSGuard_FailsFatal(t *testing.T) {
	// auto_upgrade_ubuntu=true but accept_non_lts=false; target=25.10
	deps := upgradeDeps(t, profileForUpgrade("25.10", true, false, ""))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ubuntu.ErrNonLTSRefused) {
		t.Errorf("err: got %v want ErrNonLTSRefused", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
	r := deps.State.Get(UbuntuUpgradeName).Reason
	if !strings.Contains(r, "accept_non_lts") {
		t.Errorf("reason should name the knob: %q", r)
	}
}

func TestUbuntuUpgrade_UnknownHop_FailsFatal(t *testing.T) {
	deps := upgradeDeps(t, profileForUpgrade("99.99", true, true, ""))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, ubuntu.ErrUnknownHop) {
		t.Errorf("err: got %v want ErrUnknownHop", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestUbuntuUpgrade_DoReleaseUpgrade_NotImplementedYet(t *testing.T) {
	// 22.04 -> 24.04 is the "do-release-upgrade" plan. v3 doesn't
	// implement that path yet; it must surface a clear error.
	deps := upgradeDeps(t, profileForUpgrade("24.04", true, true, "off"))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "22.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "do-release-upgrade") {
		t.Errorf("error should name do-release-upgrade: %v", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestUbuntuUpgrade_AntiRebootLoop(t *testing.T) {
	// Simulate: previous run stashed pre_version=24.04 and we rebooted
	// but the host is still on 24.04. The phase must refuse to loop.
	deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, "force"))

	// Pre-seed state with pre_version=24.04 from a previous attempt.
	p := deps.State.Get(UbuntuUpgradeName)
	p.Details = map[string]any{"pre_version": "24.04"}

	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error from anti-loop guard")
	}
	if !strings.Contains(err.Error(), "did not make progress") {
		t.Errorf("error should explain the anti-loop guard: %v", err)
	}
	if got := deps.State.Get(UbuntuUpgradeName).Status; got != state.StatusFailedFatal {
		t.Errorf("status: got %q want failed_fatal", got)
	}
}

func TestUbuntuUpgrade_BadDirectPolicy_FailsFatal(t *testing.T) {
	deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, "maybe"))
	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
	}
	err := ph.Run(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected error for invalid direct_apt_codename_upgrade")
	}
	if !strings.Contains(err.Error(), "direct_apt_codename_upgrade") {
		t.Errorf("error should name the offending knob: %v", err)
	}
}

// -----------------------------------------------------------------------------
// helper-level tests using real temp dirs.
// -----------------------------------------------------------------------------

func TestDisableThirdPartyAptSources_MovesNvidiaAndCuda(t *testing.T) {
	dir := t.TempDir()
	disabled := filepath.Join(dir, "disabled")

	files := map[string]string{
		"ubuntu.sources":           "URIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble\n",
		"cuda-keyring.list":        "deb https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64 /\n",
		"graphics-drivers.list":    "deb http://ppa.launchpadcontent.net/graphics-drivers/ppa/ubuntu noble main\n",
		"nvidia-container.sources": "URIs: https://nvidia.github.io/libnvidia-container/stable/ubuntu24.04/$(ARCH)\n",
		"unrelated.list":           "deb http://example.com/repo noble main\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := disableThirdPartyAptSources(dir, disabled, log); err != nil {
		t.Fatalf("disableThirdPartyAptSources: %v", err)
	}

	// ubuntu.sources MUST remain.
	if _, err := os.Stat(filepath.Join(dir, "ubuntu.sources")); err != nil {
		t.Errorf("ubuntu.sources should still be in place: %v", err)
	}
	// unrelated.list MUST remain.
	if _, err := os.Stat(filepath.Join(dir, "unrelated.list")); err != nil {
		t.Errorf("unrelated.list should still be in place: %v", err)
	}
	// NVIDIA / CUDA / graphics-drivers MUST have moved to disabled/.
	for _, name := range []string{"cuda-keyring.list", "graphics-drivers.list", "nvidia-container.sources"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been moved; stat err=%v", name, err)
		}
		if _, err := os.Stat(filepath.Join(disabled, name)); err != nil {
			t.Errorf("%s should be in disabled/: %v", name, err)
		}
	}
}

func TestRewriteAptCodename_RewritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	ubuntuSources := filepath.Join(dir, "ubuntu.sources")
	sourcesList := filepath.Join(dir, "..", "sources.list")
	// Ensure the parent exists.
	if err := os.MkdirAll(filepath.Dir(sourcesList), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(ubuntuSources, []byte("Suites: noble noble-updates noble-backports\n"), 0o644); err != nil {
		t.Fatalf("seed ubuntu.sources: %v", err)
	}
	if err := os.WriteFile(sourcesList, []byte("deb http://archive.ubuntu.com/ubuntu/ noble main\n"), 0o644); err != nil {
		t.Fatalf("seed sources.list: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := rewriteAptCodename(dir, sourcesList, "noble", "questing", log); err != nil {
		t.Fatalf("rewriteAptCodename: %v", err)
	}

	b1, _ := os.ReadFile(ubuntuSources)
	if !strings.Contains(string(b1), "questing") || strings.Contains(string(b1), "noble") {
		t.Errorf("ubuntu.sources not rewritten: %q", string(b1))
	}
	b2, _ := os.ReadFile(sourcesList)
	if !strings.Contains(string(b2), "questing") || strings.Contains(string(b2), "noble") {
		t.Errorf("sources.list not rewritten: %q", string(b2))
	}
}

func TestRewriteAptCodename_NoMatch_Errors(t *testing.T) {
	dir := t.TempDir()
	ubuntuSources := filepath.Join(dir, "ubuntu.sources")
	if err := os.WriteFile(ubuntuSources, []byte("Suites: jammy\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sourcesList := filepath.Join(dir, "sources.list")
	if err := os.WriteFile(sourcesList, []byte("# empty\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := rewriteAptCodename(dir, sourcesList, "noble", "questing", log)
	if err == nil {
		t.Fatalf("expected error when no file mentions the from-codename")
	}
	if !strings.Contains(err.Error(), "noble") {
		t.Errorf("error should name the missing codename: %v", err)
	}
}
