package phase

import (
	"context"
	"errors"
	"fmt"
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
	// Simulate: previous run reached dist-upgrade-complete /
	// reboot-required AND we rebooted, but the host is still on
	// 24.04. The phase must refuse to loop.
	deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, "force"))

	// Pre-seed state with pre_version=24.04 AND stage that indicates
	// the dist-upgrade actually finished.
	p := deps.State.Get(UbuntuUpgradeName)
	p.Details = map[string]any{
		"pre_version": "24.04",
		"stage":       string(UbuntuStageRebootRequired),
	}

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

// TestUbuntuUpgrade_StageEarly_RetriesNotFatal exercises the
// interrupted-before-rewrite case: pre_version is set but stage is
// "started" / "third-party-sources-disabled". The phase should NOT
// fire the anti-loop guard; it should re-run idempotently.
//
// This test does NOT exercise the full rewrite path (that needs an
// apt.Transaction with a real env). Instead it asserts that the Run
// dispatch does NOT short-circuit to failed_fatal on the anti-loop
// guard when stage is early. We make a custom UbuntuUpgrade with a
// no-op direct-codename hook to keep the test cross-platform.
func TestUbuntuUpgrade_StageEarly_RetriesNotFatal(t *testing.T) {
	for _, stage := range []UbuntuUpgradeStage{
		"", UbuntuStageStarted, UbuntuStageThirdPartyDisabled,
	} {
		t.Run("stage="+string(stage), func(t *testing.T) {
			deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, "force"))
			ph := deps.State.Get(UbuntuUpgradeName)
			ph.Details = map[string]any{
				"pre_version": "24.04",
				"stage":       string(stage),
			}

			ran := false
			phase := UbuntuUpgrade{
				CurrentVersionFn: func() string { return "24.04" },
				DirectRewriteHookFn: func() error {
					ran = true
					return nil // simulate successful continuation
				},
			}
			err := phase.Run(context.Background(), deps)
			// Hook returns nil but the real path requires an apt
			// transaction. We just assert the anti-loop guard didn't
			// fire (i.e. the phase status is not failed_fatal with
			// the "did not make progress" reason).
			if err != nil && strings.Contains(err.Error(), "did not make progress") {
				t.Errorf("anti-loop fired on stage %q (should retry): %v", stage, err)
			}
			if got := deps.State.Get(UbuntuUpgradeName).Status; got == state.StatusFailedFatal {
				if reason := deps.State.Get(UbuntuUpgradeName).Reason; strings.Contains(reason, "did not make progress") {
					t.Errorf("anti-loop status set on stage %q (should retry): %q", stage, reason)
				}
			}
			if !ran {
				t.Errorf("DirectRewriteHookFn should have been invoked for stage %q (retry path)", stage)
			}
		})
	}
}

// TestUbuntuUpgrade_StageMid_ContinuesIdempotent: stage indicates
// "sources-rewritten" or "dist-upgrade-started" — the phase must
// NOT anti-loop-fail; it must continue idempotently.
func TestUbuntuUpgrade_StageMid_ContinuesIdempotent(t *testing.T) {
	for _, stage := range []UbuntuUpgradeStage{
		UbuntuStageSourcesRewritten, UbuntuStageDistUpgradeStarted,
	} {
		t.Run("stage="+string(stage), func(t *testing.T) {
			deps := upgradeDeps(t, profileForUpgrade("25.10", true, true, "force"))
			ph := deps.State.Get(UbuntuUpgradeName)
			ph.Details = map[string]any{
				"pre_version": "24.04",
				"stage":       string(stage),
			}

			ran := false
			phase := UbuntuUpgrade{
				CurrentVersionFn:    func() string { return "24.04" },
				DirectRewriteHookFn: func() error { ran = true; return nil },
			}
			_ = phase.Run(context.Background(), deps)
			if got := deps.State.Get(UbuntuUpgradeName).Status; got == state.StatusFailedFatal {
				if reason := deps.State.Get(UbuntuUpgradeName).Reason; strings.Contains(reason, "did not make progress") {
					t.Errorf("anti-loop status set on mid-stage %q: %q", stage, reason)
				}
			}
			if !ran {
				t.Errorf("DirectRewriteHookFn must run on mid-stage %q", stage)
			}
		})
	}
}

// TestUbuntuUpgrade_Resolver_PicksFirstSupported is the brief's
// central OS-resolver case: candidates list newest-first, the
// resolver rejects 26.04 (not in v3 supported list) and selects
// 25.10. State.Details should record the full trail.
func TestUbuntuUpgrade_Resolver_PicksFirstSupported(t *testing.T) {
	p := profileForUpgrade("", true, true, "auto")
	p.Deploy.UbuntuSelectionPolicy = "latest-compatible"
	p.Deploy.UbuntuCandidates = []string{"26.04", "25.10", "24.04"}
	deps := upgradeDeps(t, p)

	// Host is already 25.10 -> after the resolver picks 25.10, the
	// PlanUpgrade returns UpgradeNoop (current == target).
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
	if d["selected_ubuntu_version"] != "25.10" {
		t.Errorf("selected_ubuntu_version: got %v want 25.10", d["selected_ubuntu_version"])
	}
	if d["selected_ubuntu_codename"] != "questing" {
		t.Errorf("selected_ubuntu_codename: got %v want questing", d["selected_ubuntu_codename"])
	}
	tried, _ := d["ubuntu_candidates_tried"].([]string)
	if len(tried) == 0 || tried[0] != "26.04" {
		t.Errorf("ubuntu_candidates_tried: got %v want [26.04, ...]", tried)
	}
	rejected, _ := d["rejected_ubuntu_candidates"].(map[string]string)
	if rejected["26.04"] == "" {
		t.Errorf("rejected_ubuntu_candidates should record 26.04; got %v", rejected)
	}
	if d["ubuntu_selection_policy"] != "latest-compatible" {
		t.Errorf("ubuntu_selection_policy: got %v want latest-compatible", d["ubuntu_selection_policy"])
	}
}

// TestUbuntuUpgrade_Resolver_PackageProbeOverridesPick: a profile-
// supplied probe rejects 25.10 even though it would otherwise pass;
// the resolver falls through to 24.04.
func TestUbuntuUpgrade_Resolver_PackageProbeOverridesPick(t *testing.T) {
	p := profileForUpgrade("", true, true, "auto")
	p.Deploy.UbuntuSelectionPolicy = "latest-compatible"
	p.Deploy.UbuntuCandidates = []string{"25.10", "24.04"}
	deps := upgradeDeps(t, p)

	ph := UbuntuUpgrade{
		CurrentVersionFn: func() string { return "24.04" },
		OSPackageProbeFn: func(v string) (bool, string) {
			if v == "25.10" {
				return false, "kde-plasma not yet built for questing"
			}
			return true, ""
		},
	}
	_ = ph.Run(context.Background(), deps)
	d := deps.State.Get(UbuntuUpgradeName).Details
	if d["selected_ubuntu_version"] != "24.04" {
		t.Errorf("probe should push selection to 24.04; got %v", d["selected_ubuntu_version"])
	}
	rejected, _ := d["rejected_ubuntu_candidates"].(map[string]string)
	if r := rejected["25.10"]; !strings.Contains(r, "package probe rejected") {
		t.Errorf("rejected_ubuntu_candidates[25.10] should explain probe rejection; got %q", r)
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

// TestQuarantineNonOfficialAptSources_MovesAllThirdParty exercises
// the broader allowlist sweep that landed after the live VM 2026-05-22
// jammy -> noble hop left docker.list + tailscale.list active. Every
// non-Ubuntu-archive file should move aside; ubuntu.sources +
// ubuntu-archive-only files stay put.
func TestQuarantineNonOfficialAptSources_MovesAllThirdParty(t *testing.T) {
	dir := t.TempDir()
	disabled := filepath.Join(dir, "disabled")

	files := map[string]string{
		"ubuntu.sources":           "URIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble\nComponents: main universe\n",
		"cuda-keyring.list":        "deb https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64 /\n",
		"graphics-drivers.list":    "deb http://ppa.launchpadcontent.net/graphics-drivers/ppa/ubuntu jammy main\n",
		"nvidia-container.sources": "URIs: https://nvidia.github.io/libnvidia-container/stable/ubuntu24.04/$(ARCH)\nSuites: /\n",
		// Live VM regression: docker.list + tailscale.list were left
		// active after the codename rewrite. New sweep must move them.
		"docker.list":           "deb [arch=amd64] https://download.docker.com/linux/ubuntu jammy stable\n",
		"tailscale.list":        "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu jammy main\n",
		"my-ubuntu-mirror.list": "# operator's local mirror still hits archive.ubuntu.com\ndeb http://us.archive.ubuntu.com/ubuntu noble main universe\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	moved, kept, err := quarantineNonOfficialAptSources(dir, disabled, log)
	if err != nil {
		t.Fatalf("quarantineNonOfficialAptSources: %v", err)
	}

	// ubuntu.sources + the ubuntu-archive mirror list MUST remain.
	for _, name := range []string{"ubuntu.sources", "my-ubuntu-mirror.list"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should still be in place (ubuntu archive); err=%v", name, err)
		}
	}
	if len(kept) != 2 {
		t.Errorf("expected 2 kept (ubuntu.sources + my-ubuntu-mirror.list); got %v", kept)
	}
	// All third-party sources MUST have moved to disabled/.
	wantMoved := []string{
		"cuda-keyring.list",
		"graphics-drivers.list",
		"nvidia-container.sources",
		"docker.list",
		"tailscale.list",
	}
	for _, name := range wantMoved {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have moved out of sources.list.d; stat err=%v", name, err)
		}
		if _, err := os.Stat(filepath.Join(disabled, name)); err != nil {
			t.Errorf("%s should be in disabled/; err=%v", name, err)
		}
	}
	if len(moved) != len(wantMoved) {
		t.Errorf("moved count: got %d (%v) want %d (%v)", len(moved), moved, len(wantMoved), wantMoved)
	}
}

func TestQuarantineNonOfficialAptSources_LeavesDeb822UbuntuMirrorAlone(t *testing.T) {
	dir := t.TempDir()
	disabled := filepath.Join(dir, "disabled")
	body := "Types: deb deb-src\nURIs: http://us.archive.ubuntu.com/ubuntu\nSuites: noble noble-updates noble-security\nComponents: main universe\n"
	if err := os.WriteFile(filepath.Join(dir, "operator-mirror.sources"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, kept, err := quarantineNonOfficialAptSources(dir, disabled, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("quarantineNonOfficialAptSources: %v", err)
	}
	if len(moved) != 0 {
		t.Fatalf("operator's ubuntu-archive deb822 mirror should have stayed in place; got moved=%v", moved)
	}
	if len(kept) != 1 {
		t.Fatalf("kept: got %v want 1 entry", kept)
	}
}

func TestQuarantineNonOfficialAptSources_PostHopGrepFindsNoJammy(t *testing.T) {
	// Live VM acceptance: after jammy -> noble, `grep -R jammy
	// /etc/apt/sources.list.d` should return nothing active.
	dir := t.TempDir()
	disabled := filepath.Join(dir, "disabled")
	files := map[string]string{
		"ubuntu.sources": "URIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble\n",
		"docker.list":    "deb https://download.docker.com/linux/ubuntu jammy stable\n",
		"tailscale.list": "deb https://pkgs.tailscale.com/stable/ubuntu jammy main\n",
		"graphics.list":  "deb http://ppa.launchpadcontent.net/graphics-drivers/ppa/ubuntu jammy main\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := quarantineNonOfficialAptSources(dir, disabled, slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(b), "jammy") {
			t.Fatalf("post-quarantine file %s still contains \"jammy\":\n%s", e.Name(), b)
		}
	}
}

// -----------------------------------------------------------------------------
// grub-pc preseed + apt term.log parser + dpkg recovery
// -----------------------------------------------------------------------------

func TestParseAptFailedPackages_GrubPCPostinstFailure(t *testing.T) {
	// Live VM 2026-05-22 apt term.log excerpt: grub-pc died first,
	// then grub-gfxpayload-lists, grub-efi-amd64-signed, shim-signed
	// followed because they depend on grub-pc being configured.
	log := `Setting up grub-pc (2.12-1ubuntu7.3) ...
Replacing config file /etc/default/grub with new version
grub-pc: Running grub-install ...
dpkg: error processing package grub-pc (--configure):
 installed grub-pc package post-installation script subprocess returned error exit status 1
dpkg: dependency problems prevent configuration of grub-gfxpayload-lists:
 grub-gfxpayload-lists depends on grub-pc; however:
  Package grub-pc is not configured yet.
dpkg: error processing package grub-gfxpayload-lists (--configure):
 dependency problems - leaving unconfigured
Errors were encountered while processing:
 grub-pc
 grub-gfxpayload-lists
 grub-efi-amd64-signed
 shim-signed
E: Sub-process /usr/bin/dpkg returned an error code (1)
`
	got := parseAptFailedPackages(log)
	want := []string{"grub-pc", "grub-gfxpayload-lists", "grub-efi-amd64-signed", "shim-signed"}
	if len(got) != len(want) {
		t.Fatalf("parseAptFailedPackages: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseAptFailedPackages[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestParseAptFailedPackages_EmptyOnSuccess(t *testing.T) {
	for _, in := range []string{
		"",
		"Setting up grub-pc (2.12) ...\nReading package lists... Done\n",
	} {
		if got := parseAptFailedPackages(in); len(got) != 0 {
			t.Errorf("parseAptFailedPackages(%q): got %v want empty", in, got)
		}
	}
}

func TestMentionsGrubOrShim(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want bool
	}{
		{[]string{"grub-pc"}, true},
		{[]string{"grub-gfxpayload-lists"}, true},
		{[]string{"shim-signed"}, true},
		{[]string{"grub2-common"}, true},
		{[]string{"unrelated", "linux-image-generic"}, false},
		{[]string{}, false},
	} {
		if got := mentionsGrubOrShim(c.in); got != c.want {
			t.Errorf("mentionsGrubOrShim(%v): got %v want %v", c.in, got, c.want)
		}
	}
}

func TestResolveBootDiskFromOutputs_ParentNameIsPrimaryPath(t *testing.T) {
	// findmnt says / is on /dev/vda2; lsblk -no PKNAME says vda.
	got := resolveBootDiskFromOutputs("/dev/vda2", "vda", "/dev/sda")
	if got != "/dev/vda" {
		t.Fatalf("got %q want /dev/vda (parent should win)", got)
	}
}

func TestResolveBootDiskFromOutputs_AcceptsAbsolutePKName(t *testing.T) {
	// Some lsblk builds prefix PKNAME with /dev/. Defensive.
	got := resolveBootDiskFromOutputs("/dev/nvme0n1p1", "/dev/nvme0n1", "/dev/sda")
	if got != "/dev/nvme0n1" {
		t.Fatalf("got %q want /dev/nvme0n1", got)
	}
}

func TestResolveBootDiskFromOutputs_FallsBackToFirstDiskWhenPKNameEmpty(t *testing.T) {
	// dm-mapper / LVM root: PKNAME is empty; fallback wins.
	got := resolveBootDiskFromOutputs("/dev/dm-0", "", "/dev/nvme0n1")
	if got != "/dev/nvme0n1" {
		t.Fatalf("got %q want /dev/nvme0n1 (fallback to first disk)", got)
	}
}

func TestResolveBootDiskFromOutputs_AcceptsRootSourceWhenItIsADisk(t *testing.T) {
	// Some cloud VMs mount / straight off /dev/vda with no
	// partition table.
	got := resolveBootDiskFromOutputs("/dev/vda", "", "")
	if got != "/dev/vda" {
		t.Fatalf("got %q want /dev/vda (root source itself is the disk)", got)
	}
}

func TestResolveBootDiskFromOutputs_ReturnsEmptyOnTotalFailure(t *testing.T) {
	got := resolveBootDiskFromOutputs("", "", "")
	if got != "" {
		t.Fatalf("got %q want empty string (nothing resolvable)", got)
	}
}

func TestParseDpkgAuditOutput_LiveVMShape(t *testing.T) {
	out := `The following packages are in a mess due to serious problems during
installation.  They must be reinstalled for them (and any packages
that depend on them) to function properly:
  grub-efi-amd64-signed
  grub-gfxpayload-lists
  shim-signed
The following packages have been unpacked but not yet configured.
They must be configured using dpkg --configure or the configure
menu option in dselect for them to work:
  grub-pc
`
	got := parseDpkgAuditOutput(out)
	want := []string{"grub-efi-amd64-signed", "grub-gfxpayload-lists", "shim-signed", "grub-pc"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestParseDpkgAuditOutput_EmptyOnClean(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"No installed packages have problems. Everything OK.\n",
	} {
		if got := parseDpkgAuditOutput(in); len(got) != 0 {
			t.Errorf("parseDpkgAuditOutput(%q): got %v want empty", in, got)
		}
	}
}

func TestBIOSEFIPurgeCandidateIsConservative(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		want bool
	}{
		{name: "efi only", pkgs: []string{"grub-efi-amd64-signed", "shim-signed"}, want: true},
		{name: "grub pc still broken", pkgs: []string{"grub-pc", "grub-efi-amd64-signed", "shim-signed"}, want: false},
		{name: "gfx payload still broken", pkgs: []string{"grub-gfxpayload-lists", "shim-signed"}, want: false},
		{name: "unrelated", pkgs: []string{"linux-image-generic"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := biosEFIPurgeCandidate(tc.pkgs); got != tc.want {
				t.Fatalf("biosEFIPurgeCandidate(%v): got %v want %v", tc.pkgs, got, tc.want)
			}
		})
	}
}
func TestReadLastLines_TailsLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "term.log")
	var body string
	for i := 1; i <= 500; i++ {
		body += fmt.Sprintf("line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readLastLines(path, 5)
	want := "line 496\nline 497\nline 498\nline 499\nline 500"
	if got != want {
		t.Fatalf("readLastLines: got %q want %q", got, want)
	}
}

func TestReadLastLines_MissingFileReturnsEmpty(t *testing.T) {
	if got := readLastLines("/no/such/file/here.log", 10); got != "" {
		t.Errorf("expected empty for missing file; got %q", got)
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
	mirrorList := filepath.Join(dir, "operator-mirror.list")
	if err := os.WriteFile(mirrorList, []byte("deb http://us.archive.ubuntu.com/ubuntu noble main\n"), 0o644); err != nil {
		t.Fatalf("seed operator mirror: %v", err)
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
	b3, _ := os.ReadFile(mirrorList)
	if !strings.Contains(string(b3), "questing") || strings.Contains(string(b3), "noble") {
		t.Errorf("operator mirror list not rewritten: %q", string(b3))
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
		t.Fatalf("expected error when no file mentions either codename")
	}
	if !strings.Contains(err.Error(), "noble") {
		t.Errorf("error should name the missing codename: %v", err)
	}
}

// TestRewriteAptCodename_IdempotentWhenAlreadyAtTarget is the
// resume-after-interrupted-upgrade case: the previous run already
// rewrote sources but the dist-upgrade didn't complete. Resuming
// should NOT fail just because the source files no longer mention
// the from-codename.
func TestRewriteAptCodename_IdempotentWhenAlreadyAtTarget(t *testing.T) {
	dir := t.TempDir()
	ubuntuSources := filepath.Join(dir, "ubuntu.sources")
	if err := os.WriteFile(ubuntuSources, []byte("Suites: questing questing-updates questing-backports\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sourcesList := filepath.Join(dir, "sources.list")
	if err := os.WriteFile(sourcesList, []byte("deb http://archive.ubuntu.com/ubuntu/ questing main\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := rewriteAptCodename(dir, sourcesList, "noble", "questing", log); err != nil {
		t.Errorf("rewriteAptCodename must be idempotent when sources are already at target; got: %v", err)
	}
	// Files must be untouched (still target codename only).
	b, _ := os.ReadFile(ubuntuSources)
	if !strings.Contains(string(b), "questing") {
		t.Errorf("ubuntu.sources lost its codename: %q", string(b))
	}
}

// TestRewriteAptCodename_MixedRewriteAndAlreadyTarget exercises the
// case where one file still has the old codename (we rewrite it) and
// the other already has the new one (we leave it alone).
func TestRewriteAptCodename_MixedRewriteAndAlreadyTarget(t *testing.T) {
	dir := t.TempDir()
	ubuntuSources := filepath.Join(dir, "ubuntu.sources")
	if err := os.WriteFile(ubuntuSources, []byte("Suites: noble\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sourcesList := filepath.Join(dir, "sources.list")
	if err := os.WriteFile(sourcesList, []byte("deb http://archive.ubuntu.com/ubuntu/ questing main\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := rewriteAptCodename(dir, sourcesList, "noble", "questing", log); err != nil {
		t.Fatalf("mixed rewrite must succeed; got: %v", err)
	}
	b, _ := os.ReadFile(ubuntuSources)
	if !strings.Contains(string(b), "questing") || strings.Contains(string(b), "noble") {
		t.Errorf("ubuntu.sources not rewritten: %q", string(b))
	}
	b2, _ := os.ReadFile(sourcesList)
	if !strings.Contains(string(b2), "questing") {
		t.Errorf("sources.list lost target codename: %q", string(b2))
	}
}
