package phase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// UbuntuUpgradeName is the canonical state-key for the ubuntu-upgrade
// phase. apply puts this BEFORE base-packages because dist-upgrade
// rewrites the apt world.
const UbuntuUpgradeName = "ubuntu_upgrade"

// UbuntuUpgrade is the v3 port of v2's maybe_upgrade_ubuntu +
// direct_apt_codename_upgrade. The decision tree lives in
// internal/ubuntu.PlanUpgrade; this phase wires it to apt.Transaction
// + runner + state.
type UbuntuUpgrade struct {
	// CurrentVersionFn lets tests inject the current host version
	// instead of reading /etc/os-release. nil = read the real file.
	CurrentVersionFn func() string

	// SourcesDir overrides /etc/apt/sources.list.d for tests.
	SourcesDir string
	// SourcesListPath overrides /etc/apt/sources.list for tests.
	SourcesListPath string
	// DisabledDir overrides where third-party apt sources are moved
	// during the upgrade. Default sits next to sources.list.d.
	DisabledDir string
}

// Name implements Phase.
func (UbuntuUpgrade) Name() string { return UbuntuUpgradeName }

// Run implements Phase.
//
// Outcomes:
//
//   - Already at target -> mark done (and clear any progress
//     marker from a previous boot).
//   - No target / auto_upgrade_ubuntu=false / non-LTS guard / unknown
//     hop -> failed_fatal with a Reason that names the operator knob
//     to set.
//   - Direct apt codename rewrite plan -> stash the pre-upgrade
//     version in state.Details (anti-reboot-loop), disable third-
//     party apt sources, unhold packages, rewrite source codenames,
//     apt-get update + dist-upgrade + autoremove inside one
//     apt.Transaction, then set RebootNeeded and return
//     ErrRebootRequired.
//   - do-release-upgrade plan -> not implemented this commit; mark
//     failed_fatal with a clear "implement v3 do-release-upgrade
//     path or set deploy.direct_apt_codename_upgrade=force" message.
//
// Anti-reboot-loop guard: if the phase ran before and
// Details["pre_version"] equals the current version, we did not
// progress -> failed_fatal.
func (p UbuntuUpgrade) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, UbuntuUpgradeName) {
		log.Info("phase ubuntu-upgrade: already done; skipping")
		return nil
	}

	currentVer := p.currentVersion()
	if currentVer == "" {
		err := errors.New("could not determine current Ubuntu VERSION_ID")
		deps.State.MarkFailed(UbuntuUpgradeName, err.Error(), err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w", err)
	}

	targetVer := ""
	autoUpgrade := false
	acceptNonLTS := false
	policy := ubuntu.PolicyAuto
	if deps.Profile != nil {
		targetVer = deps.Profile.UbuntuVersion
		autoUpgrade = deps.Profile.Deploy.AutoUpgradeUbuntu
		acceptNonLTS = deps.Profile.Deploy.AcceptNonLTS
		if pol, err := ubuntu.ParseUpgradePolicy(deps.Profile.Deploy.DirectAptCodenameUpgrade); err == nil {
			policy = pol
		} else {
			deps.State.MarkFailed(UbuntuUpgradeName, "parse profile.deploy.direct_apt_codename_upgrade", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase ubuntu-upgrade: %w", err)
		}
	}

	plan := ubuntu.PlanUpgrade(ubuntu.PlanInputs{
		CurrentVersion:           currentVer,
		TargetVersion:            targetVer,
		AutoUpgrade:              autoUpgrade,
		AcceptNonLTS:             acceptNonLTS,
		DirectAptCodenameUpgrade: policy,
	})

	log.Info("phase ubuntu-upgrade: plan",
		"action", plan.Action.String(),
		"current", currentVer,
		"target", targetVer,
		"reason", plan.Reason)

	// Anti-reboot-loop: if the previous run stashed pre_version and
	// it equals the current version, the reboot didn't move us. Fail.
	if pre, ok := deps.State.Get(UbuntuUpgradeName).Details["pre_version"].(string); ok && pre == currentVer && plan.Action != ubuntu.UpgradeNoop {
		err := fmt.Errorf("ubuntu-upgrade did not make progress: pre-reboot=%s current=%s", pre, currentVer)
		deps.State.MarkFailed(UbuntuUpgradeName,
			"reboot did not advance Ubuntu VERSION_ID; refusing to loop", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w", err)
	}

	switch plan.Action {
	case ubuntu.UpgradeNoop:
		deps.State.MarkDone(UbuntuUpgradeName, map[string]any{
			"current_version": currentVer,
			"target_version":  targetVer,
			"action":          plan.Action.String(),
		})
		_ = deps.PersistState()
		return nil

	case ubuntu.UpgradeNotConfigured:
		// "Not configured" can be either "no target" (skipped) or
		// "target set but auto_upgrade=false" (failed). Distinguish
		// by the wrapped error.
		if errors.Is(plan.Err, ubuntu.ErrNoTargetVersion) {
			deps.State.MarkSkipped(UbuntuUpgradeName, plan.Reason)
			_ = deps.PersistState()
			log.Info("phase ubuntu-upgrade: skipped (no target)")
			return nil
		}
		deps.State.MarkFailed(UbuntuUpgradeName, plan.Reason, plan.Err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w (%s)", plan.Err, plan.Reason)

	case ubuntu.UpgradeRefusedNonLTS, ubuntu.UpgradeRefusedUnknownHop:
		deps.State.MarkFailed(UbuntuUpgradeName, plan.Reason, plan.Err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w (%s)", plan.Err, plan.Reason)

	case ubuntu.UpgradeDoReleaseUpgrade:
		err := errors.New("phase ubuntu-upgrade: do-release-upgrade path is NOT yet implemented in v3; set deploy.direct_apt_codename_upgrade=force or wait for Milestone 4.1")
		deps.State.MarkFailed(UbuntuUpgradeName,
			"do-release-upgrade path missing from v3", err, true)
		_ = deps.PersistState()
		return err

	case ubuntu.UpgradeDirectCodenameRewrite:
		return p.runDirectCodenameRewrite(ctx, deps, plan, currentVer, log)
	}

	err := fmt.Errorf("unexpected upgrade action %s", plan.Action)
	deps.State.MarkFailed(UbuntuUpgradeName, "unexpected action", err, true)
	_ = deps.PersistState()
	return err
}

func (p UbuntuUpgrade) runDirectCodenameRewrite(ctx context.Context, deps *Deps, plan ubuntu.UpgradePlan, currentVer string, log *slog.Logger) error {
	deps.State.MarkRunning(UbuntuUpgradeName)
	// Stash the pre-upgrade version for the anti-loop guard.
	deps.State.Get(UbuntuUpgradeName).Details = map[string]any{
		"pre_version":      currentVer,
		"target_version":   plan.TargetVersion,
		"current_codename": plan.CurrentCodename,
		"target_codename":  plan.TargetCodename,
		"action":           plan.Action.String(),
	}
	_ = deps.PersistState()

	sourcesDir := p.SourcesDir
	if sourcesDir == "" {
		sourcesDir = "/etc/apt/sources.list.d"
	}
	sourcesList := p.SourcesListPath
	if sourcesList == "" {
		sourcesList = "/etc/apt/sources.list"
	}
	disabledDir := p.DisabledDir
	if disabledDir == "" {
		disabledDir = filepath.Join(sourcesDir, "clouddeploy-disabled-during-upgrade")
	}

	// 1. Move third-party NVIDIA / CUDA / graphics-drivers apt
	//    sources aside so dist-upgrade doesn't try to pull a noble-
	//    keyed package out of the new questing tree.
	if !deps.DryRun {
		if err := disableThirdPartyAptSources(sourcesDir, disabledDir, log); err != nil {
			deps.State.MarkFailed(UbuntuUpgradeName, "disable third-party apt sources", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase ubuntu-upgrade: %w", err)
		}
	}

	// 2. + 3. + 4. + 5. inside one apt.Transaction so the policy-
	//    rc.d guard covers the whole hop.
	err := deps.APT.Run(ctx, func(tc *apt.TxContext) error {
		// 2. Unhold any held packages.
		if !deps.DryRun {
			_ = deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv: []string{"sh", "-c", "apt-mark showhold | xargs -r apt-mark unhold"},
				Sudo: true,
			})
		}

		// 3. Purge old NVIDIA/CUDA packages so we don't drag a stale
		//    closed-driver stack into the new codename.
		log.Info("phase ubuntu-upgrade: purging old NVIDIA/CUDA before codename rewrite")
		if err := tc.Purge(ctx, []string{
			"cuda-*", "nsight-*",
			"nvidia-*", "libnvidia-*",
			"xserver-xorg-video-nvidia-*",
		}); err != nil {
			// Purge of non-installed wildcards is non-fatal in apt;
			// log + continue.
			log.Warn("phase ubuntu-upgrade: pre-upgrade NVIDIA/CUDA purge non-fatal warning",
				"err", err)
		}

		// 4. Rewrite codename in apt sources.
		if !deps.DryRun {
			if err := rewriteAptCodename(sourcesDir, sourcesList, plan.CurrentCodename, plan.TargetCodename, log); err != nil {
				return fmt.Errorf("rewrite codename: %w", err)
			}
		}

		// 5. apt-get update + dist-upgrade against the new codename.
		if err := tc.Update(ctx); err != nil {
			return fmt.Errorf("apt-get update after codename rewrite: %w", err)
		}
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv: []string{
				"apt-get",
				"-y",
				"-o", "Dpkg::Options::=--force-confnew",
				"dist-upgrade",
			},
			Env: []string{
				"DEBIAN_FRONTEND=noninteractive",
				"NEEDRESTART_MODE=a",
				"NEEDRESTART_SUSPEND=1",
			},
			Sudo:    true,
			Timeout: deps.APT.AptTimeout,
			DryRun:  deps.DryRun,
		})
		if res.Err != nil {
			return fmt.Errorf("apt-get dist-upgrade: %w (stderr=%q)", res.Err, res.Stderr)
		}

		// autoremove + dpkg --configure -a are best-effort cleanup.
		_ = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:   []string{"apt-get", "-y", "autoremove"},
			Env:    []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a"},
			Sudo:   true,
			DryRun: deps.DryRun,
		})
		_ = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:   []string{"dpkg", "--configure", "-a"},
			Sudo:   true,
			DryRun: deps.DryRun,
		})
		return nil
	})
	if err != nil {
		deps.State.MarkFailed(UbuntuUpgradeName, "direct-codename apt transaction failed", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w", err)
	}

	// Reboot needed for the new kernel / userspace to land.
	deps.State.SetRebootNeeded(true, UbuntuUpgradeName)
	_ = deps.PersistState()
	log.Warn("phase ubuntu-upgrade: direct codename rewrite completed; reboot required to load new kernel + userspace")
	return ErrRebootRequired
}

// currentVersion reads VERSION_ID from /etc/os-release.
func (p UbuntuUpgrade) currentVersion() string {
	if p.CurrentVersionFn != nil {
		return p.CurrentVersionFn()
	}
	r, err := ubuntu.Read("")
	if err != nil {
		return ""
	}
	return r.VersionID
}

// -----------------------------------------------------------------------------
// helpers: side-effects guarded behind DryRun in Run().
// -----------------------------------------------------------------------------

// thirdPartySourcePatterns are the apt-sources we always want out of
// the way during a codename rewrite. Matches v2's
// direct_apt_codename_upgrade selection.
var thirdPartySourcePatterns = []string{
	"developer.download.nvidia.com",
	"ppa.launchpadcontent.net/graphics-drivers",
	"cuda-keyring",
	"nvidia-drivers",
	"nvidia-cuda",
	"graphics-drivers",
}

// thirdPartyFilenameKeywords are filename hints that match the same
// pattern when the file's body is not searchable (binary list).
var thirdPartyFilenameKeywords = []string{
	"cuda", "nvidia", "graphics-drivers",
}

// disableThirdPartyAptSources walks sourcesDir and moves files
// matching the third-party patterns into disabledDir.
func disableThirdPartyAptSources(sourcesDir, disabledDir string, log *slog.Logger) error {
	if err := os.MkdirAll(disabledDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", disabledDir, err)
	}
	entries, err := os.ReadDir(sourcesDir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", sourcesDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Never disable the Ubuntu archive's own sources.
		if name == "ubuntu.sources" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".list" && ext != ".sources" {
			continue
		}
		full := filepath.Join(sourcesDir, name)
		match := false
		// Filename hint first (cheap, no I/O).
		for _, kw := range thirdPartyFilenameKeywords {
			if strings.Contains(strings.ToLower(name), kw) {
				match = true
				break
			}
		}
		if !match {
			// Body scan.
			if b, err := os.ReadFile(full); err == nil {
				body := string(b)
				for _, pat := range thirdPartySourcePatterns {
					if strings.Contains(body, pat) {
						match = true
						break
					}
				}
			}
		}
		if !match {
			continue
		}
		dst := filepath.Join(disabledDir, name)
		log.Info("phase ubuntu-upgrade: disabling third-party apt source", "src", full, "dst", dst)
		if err := os.Rename(full, dst); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", full, dst, err)
		}
	}
	return nil
}

// rewriteAptCodename replaces every occurrence of `from` with `to`
// in /etc/apt/sources.list.d/ubuntu.sources and /etc/apt/sources.list.
// Returns an error if neither file mentioned `from` (we refuse to
// dist-upgrade without confirming the codename is actually moving).
func rewriteAptCodename(sourcesDir, sourcesListPath, from, to string, log *slog.Logger) error {
	rewrote := false

	ubuntuSources := filepath.Join(sourcesDir, "ubuntu.sources")
	if b, err := os.ReadFile(ubuntuSources); err == nil {
		body := string(b)
		if strings.Contains(body, from) {
			log.Info("phase ubuntu-upgrade: rewriting ubuntu.sources",
				"from", from, "to", to)
			newBody := strings.ReplaceAll(body, from, to)
			if err := os.WriteFile(ubuntuSources, []byte(newBody), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", ubuntuSources, err)
			}
			rewrote = true
		}
	}

	if b, err := os.ReadFile(sourcesListPath); err == nil {
		body := string(b)
		if strings.Contains(body, from) {
			log.Info("phase ubuntu-upgrade: rewriting sources.list",
				"from", from, "to", to)
			newBody := strings.ReplaceAll(body, from, to)
			if err := os.WriteFile(sourcesListPath, []byte(newBody), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", sourcesListPath, err)
			}
			rewrote = true
		}
	}

	if !rewrote {
		return fmt.Errorf("no apt source file in %s or %s mentioned codename %q; refusing to dist-upgrade", sourcesDir, sourcesListPath, from)
	}
	return nil
}
