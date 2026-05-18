package phase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// UbuntuUpgradeName is the canonical state-key for the ubuntu-upgrade
// phase. apply puts this BEFORE base-packages because dist-upgrade
// rewrites the apt world.
const UbuntuUpgradeName = "ubuntu_upgrade"

// UbuntuUpgradeStage names the discrete points the direct-codename
// rewrite passes through. They are persisted to state.Details["stage"]
// so a phase Run that resumes after a reboot / crash knows how far
// the previous attempt got AND whether forward progress is owed.
//
// The anti-reboot-loop guard fires only when the stage indicates the
// previous attempt actually finished its dist-upgrade. Earlier stages
// are re-entered idempotently:
//
//   - stage "started" / ""                       -> retry from the top
//   - stage "third-party-sources-disabled"       -> retry from the top
//     (the rename-aside step is itself idempotent: only third-party
//     sources are matched, and any prior rename has no effect on a
//     subsequent walk)
//   - stage "sources-rewritten"                  -> skip rewrite step
//   - stage "dist-upgrade-started"               -> retry dist-upgrade
//   - stage "dist-upgrade-complete" /
//     stage "reboot-required"                    -> if currentVersion
//     still equals pre_version, anti-loop fires (the reboot did NOT
//     advance the host). Otherwise we have actually advanced and the
//     phase marks done.
type UbuntuUpgradeStage string

const (
	UbuntuStageStarted             UbuntuUpgradeStage = "started"
	UbuntuStageThirdPartyDisabled  UbuntuUpgradeStage = "third-party-sources-disabled"
	UbuntuStageSourcesRewritten    UbuntuUpgradeStage = "sources-rewritten"
	UbuntuStageDistUpgradeStarted  UbuntuUpgradeStage = "dist-upgrade-started"
	UbuntuStageDistUpgradeComplete UbuntuUpgradeStage = "dist-upgrade-complete"
	UbuntuStageRebootRequired      UbuntuUpgradeStage = "reboot-required"
)

// stageRequiresAdvance reports whether the recorded stage represents
// "the dist-upgrade actually finished". When true AND
// currentVersion == pre_version, the anti-loop guard fires.
func stageRequiresAdvance(stage UbuntuUpgradeStage) bool {
	switch stage {
	case UbuntuStageDistUpgradeComplete, UbuntuStageRebootRequired:
		return true
	}
	return false
}

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

	// DirectRewriteHookFn lets tests replace the entire
	// runDirectCodenameRewrite body without spinning up a real
	// apt.Transaction. nil = run the real path. Tests use this to
	// exercise the stage-marker anti-loop guard cross-platform.
	DirectRewriteHookFn func() error
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

	// Anti-reboot-loop: fires ONLY when the previous attempt actually
	// finished its dist-upgrade (stage == dist-upgrade-complete or
	// reboot-required) AND the host VERSION_ID is still pre_version.
	// Earlier stages indicate the previous run was interrupted before
	// it could move the host forward; we re-run idempotently.
	details := deps.State.Get(UbuntuUpgradeName).Details
	pre, _ := details["pre_version"].(string)
	stageRaw, _ := details["stage"].(string)
	stage := UbuntuUpgradeStage(stageRaw)
	if pre != "" && pre == currentVer && plan.Action != ubuntu.UpgradeNoop && stageRequiresAdvance(stage) {
		err := fmt.Errorf("ubuntu-upgrade did not make progress: pre-reboot=%s current=%s stage=%s", pre, currentVer, stage)
		deps.State.MarkFailed(UbuntuUpgradeName,
			"reboot did not advance Ubuntu VERSION_ID; refusing to loop", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w", err)
	}
	if pre != "" && pre == currentVer && plan.Action != ubuntu.UpgradeNoop {
		log.Info("phase ubuntu-upgrade: resuming interrupted run",
			"pre_version", pre, "current_version", currentVer, "stage", stage)
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
		if p.DirectRewriteHookFn != nil {
			return p.DirectRewriteHookFn()
		}
		return p.runDirectCodenameRewrite(ctx, deps, plan, currentVer, log)
	}

	err := fmt.Errorf("unexpected upgrade action %s", plan.Action)
	deps.State.MarkFailed(UbuntuUpgradeName, "unexpected action", err, true)
	_ = deps.PersistState()
	return err
}

func (p UbuntuUpgrade) runDirectCodenameRewrite(ctx context.Context, deps *Deps, plan ubuntu.UpgradePlan, currentVer string, log *slog.Logger) error {
	deps.State.MarkRunning(UbuntuUpgradeName)
	// Stash / extend Details. We preserve any stage marker from a
	// previous attempt so the per-stage idempotency logic below knows
	// what to skip.
	prev := deps.State.Get(UbuntuUpgradeName).Details
	prevStage := UbuntuUpgradeStage("")
	if s, ok := prev["stage"].(string); ok {
		prevStage = UbuntuUpgradeStage(s)
	}
	details := map[string]any{
		"pre_version":      stringOrFromDetails(prev, "pre_version", currentVer),
		"target_version":   plan.TargetVersion,
		"current_codename": plan.CurrentCodename,
		"target_codename":  plan.TargetCodename,
		"action":           plan.Action.String(),
		"stage":            string(UbuntuStageStarted),
	}
	deps.State.Get(UbuntuUpgradeName).Details = details
	_ = deps.PersistState()

	// Stop apt-daily / apt-daily-upgrade / unattended-upgrades for the
	// duration of the codename rewrite + dist-upgrade. Background
	// noble-keyed updates from cron-driven apt jobs were the cause of
	// the "rebooted into kernel 6.8 still on noble" failure on the
	// live VM. We don't restart them - the system will reboot at the
	// end of this phase and systemd-fresh-boot picks them up anyway.
	if !deps.DryRun {
		p.stopBackgroundAptServices(ctx, deps, log)
	}
	// Wait for any in-flight dpkg/apt holders to release the lock
	// before we mutate sources on disk.
	if !deps.DryRun {
		if err := p.waitForAptLocks(ctx, deps, log); err != nil {
			log.Warn("phase ubuntu-upgrade: dpkg/apt lock wait timed out; proceeding (apt.Transaction will retry)", "err", err)
		}
	}

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

	advance := func(s UbuntuUpgradeStage) {
		details["stage"] = string(s)
		deps.State.Get(UbuntuUpgradeName).Details = details
		_ = deps.PersistState()
	}

	// 1. Move third-party NVIDIA / CUDA / graphics-drivers apt
	//    sources aside. Skip when already past this stage.
	if !deps.DryRun && !stageAtLeast(prevStage, UbuntuStageThirdPartyDisabled) {
		if err := disableThirdPartyAptSources(sourcesDir, disabledDir, log); err != nil {
			deps.State.MarkFailed(UbuntuUpgradeName, "disable third-party apt sources", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase ubuntu-upgrade: %w", err)
		}
	}
	advance(UbuntuStageThirdPartyDisabled)

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

		// 4. Rewrite codename in apt sources. Idempotent: succeeds
		//    when the source files are already at the target.
		if !deps.DryRun {
			if err := rewriteAptCodename(sourcesDir, sourcesList, plan.CurrentCodename, plan.TargetCodename, log); err != nil {
				return fmt.Errorf("rewrite codename: %w", err)
			}
		}
		advance(UbuntuStageSourcesRewritten)

		// 5. apt-get update + dist-upgrade against the new codename.
		if err := tc.Update(ctx); err != nil {
			return fmt.Errorf("apt-get update after codename rewrite: %w", err)
		}
		advance(UbuntuStageDistUpgradeStarted)
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
		advance(UbuntuStageDistUpgradeComplete)
		return nil
	})
	if err != nil {
		deps.State.MarkFailed(UbuntuUpgradeName, "direct-codename apt transaction failed", err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w", err)
	}

	// Reboot needed for the new kernel / userspace to land.
	advance(UbuntuStageRebootRequired)
	deps.State.SetRebootNeeded(true, UbuntuUpgradeName)
	_ = deps.PersistState()
	log.Warn("phase ubuntu-upgrade: direct codename rewrite completed; reboot required to load new kernel + userspace")
	return ErrRebootRequired
}

// stageAtLeast reports whether `recorded` is the same as or after
// `target` in the linear stage ordering.
func stageAtLeast(recorded, target UbuntuUpgradeStage) bool {
	order := map[UbuntuUpgradeStage]int{
		"":                             0,
		UbuntuStageStarted:             1,
		UbuntuStageThirdPartyDisabled:  2,
		UbuntuStageSourcesRewritten:    3,
		UbuntuStageDistUpgradeStarted:  4,
		UbuntuStageDistUpgradeComplete: 5,
		UbuntuStageRebootRequired:      6,
	}
	return order[recorded] >= order[target]
}

func stringOrFromDetails(d map[string]any, key, def string) string {
	if d == nil {
		return def
	}
	if v, ok := d[key].(string); ok && v != "" {
		return v
	}
	return def
}

// stopBackgroundAptServices stops and (best-effort) masks the
// auto-update timers + the unattended-upgrades unit so background apt
// jobs do not fight the codename rewrite. Failures are warnings — on
// hosts without those units installed, the systemctl call exits
// non-zero and we just continue.
func (p UbuntuUpgrade) stopBackgroundAptServices(ctx context.Context, deps *Deps, log *slog.Logger) {
	for _, unit := range []string{
		"apt-daily.timer",
		"apt-daily-upgrade.timer",
		"unattended-upgrades.service",
	} {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"systemctl", "stop", unit},
			Sudo:    true,
			LogFile: "-",
			Timeout: 30 * time.Second,
		})
		if res.Err != nil {
			log.Info("phase ubuntu-upgrade: systemctl stop reported non-zero (unit may be missing)",
				"unit", unit, "err", res.Err)
			continue
		}
		log.Info("phase ubuntu-upgrade: stopped background apt unit", "unit", unit)
	}
}

// waitForAptLocks polls for any process holding the dpkg/apt locks
// and waits up to ~3 minutes for them to release. apt.Transaction
// already does this for transactional steps, but the source-rewrite
// happens outside that wrapper, so we wait once here too.
func (p UbuntuUpgrade) waitForAptLocks(ctx context.Context, deps *Deps, log *slog.Logger) error {
	if deps.APT == nil {
		return nil
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		holders := deps.APT.LockHolders(ctx)
		if len(holders) == 0 {
			return nil
		}
		log.Info("phase ubuntu-upgrade: waiting for dpkg/apt locks",
			"holders", holders)
		if time.Now().After(deadline) {
			return fmt.Errorf("dpkg/apt locks held by %v after 3m", holders)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
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
// Idempotent:
//
//   - some-file contains `from`            -> rewrite that file.
//   - no file contains `from` BUT at least one already contains `to`
//     -> success (we're past the
//     rewrite stage; this is
//     an interrupted-run resume).
//   - no file contains either              -> fatal: refusing to
//     dist-upgrade with a
//     source set that
//     mentions neither codename.
func rewriteAptCodename(sourcesDir, sourcesListPath, from, to string, log *slog.Logger) error {
	rewrote := false
	alreadyAtTarget := false

	candidates := []string{
		filepath.Join(sourcesDir, "ubuntu.sources"),
		sourcesListPath,
	}
	for _, path := range candidates {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := string(b)
		switch {
		case strings.Contains(body, from):
			log.Info("phase ubuntu-upgrade: rewriting apt source",
				"path", path, "from", from, "to", to)
			newBody := strings.ReplaceAll(body, from, to)
			if err := os.WriteFile(path, []byte(newBody), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			rewrote = true
		case strings.Contains(body, to):
			log.Info("phase ubuntu-upgrade: apt source already at target codename; skipping rewrite",
				"path", path, "target", to)
			alreadyAtTarget = true
		}
	}

	if !rewrote && !alreadyAtTarget {
		return fmt.Errorf("no apt source file in %s or %s mentioned codename %q or %q; refusing to dist-upgrade", sourcesDir, sourcesListPath, from, to)
	}
	return nil
}
