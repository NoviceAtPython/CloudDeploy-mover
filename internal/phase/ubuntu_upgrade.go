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
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
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

// buildOSResolverDetails turns a profile + resolver result into the
// state.Details fields the brief asks for. Always returns a non-nil
// map; entries are only populated when the input has meaningful data.
func buildOSResolverDetails(profile *config.Profile, policy ubuntu.OSSelectionPolicy, res ubuntu.OSResolverResult) map[string]any {
	out := map[string]any{}
	if profile == nil {
		return out
	}
	if profile.Deploy.UbuntuSelectionPolicy != "" || len(profile.Deploy.UbuntuCandidates) > 0 {
		out["ubuntu_selection_policy"] = string(policy)
	}
	if len(profile.Deploy.UbuntuCandidates) > 0 {
		out["ubuntu_candidates_configured"] = profile.Deploy.UbuntuCandidates
	}
	if len(res.Tried) > 0 {
		tried := make([]string, 0, len(res.Tried))
		for _, c := range res.Tried {
			tried = append(tried, c.Version)
		}
		out["ubuntu_candidates_tried"] = tried
	}
	if res.Selected != "" {
		out["selected_ubuntu_version"] = res.Selected
		out["selected_ubuntu_codename"] = res.SelectedCodename
	}
	if reasons := res.RejectedReasons(); len(reasons) > 0 {
		out["rejected_ubuntu_candidates"] = reasons
	}
	if profile.Deploy.PreferLTS {
		out["prefer_lts"] = true
	}
	return out
}

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

	// OSPackageProbeFn is the optional per-candidate hook the OS
	// resolver calls to ask "would the profile's required packages
	// install on this Ubuntu version?". nil = no probe (trust the
	// codename map + v3-supported list). Tests use this to simulate
	// "package X not yet available on 26.04".
	OSPackageProbeFn ubuntu.PackageProbeFn
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
	var resolved ubuntu.OSResolverResult
	osPolicy := ubuntu.OSPolicyLatestCompatible
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
		if osPol, err := ubuntu.ParseOSSelectionPolicy(deps.Profile.Deploy.UbuntuSelectionPolicy); err == nil {
			osPolicy = osPol
		} else {
			deps.State.MarkFailed(UbuntuUpgradeName, "parse profile.deploy.ubuntu_selection_policy", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase ubuntu-upgrade: %w", err)
		}
		// Resolver: if the operator gave us a candidate list, walk it
		// newest-first. The first candidate that passes the gate set
		// (codename-known + v3-supported + non-LTS gate + optional
		// package probe) wins. Falls back to Profile.UbuntuVersion
		// (the pre-resolver behavior) when the list is empty.
		if len(deps.Profile.Deploy.UbuntuCandidates) > 0 {
			in := ubuntu.OSResolverInputs{
				CurrentVersion: currentVer,
				Candidates:     deps.Profile.Deploy.UbuntuCandidates,
				MinVersion:     deps.Profile.Deploy.UbuntuMinVersion,
				Policy:         osPolicy,
				AcceptNonLTS:   acceptNonLTS,
				PreferLTS:      deps.Profile.Deploy.PreferLTS,
				Probe:          p.OSPackageProbeFn,
			}
			resolved = ubuntu.ResolveOSTarget(in)
			log.Info("phase ubuntu-upgrade: OS resolver",
				"policy", osPolicy,
				"candidates", deps.Profile.Deploy.UbuntuCandidates,
				"selected", resolved.Selected,
				"selected_codename", resolved.SelectedCodename,
				"rejected", resolved.RejectedReasons())
			if resolved.Selected != "" {
				targetVer = resolved.Selected
			}
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

	resolverDetails := buildOSResolverDetails(deps.Profile, osPolicy, resolved)

	switch plan.Action {
	case ubuntu.UpgradeNoop:
		doneDetails := map[string]any{
			"current_version": currentVer,
			"target_version":  targetVer,
			"action":          plan.Action.String(),
		}
		for k, v := range resolverDetails {
			doneDetails[k] = v
		}
		deps.State.MarkDone(UbuntuUpgradeName, doneDetails)
		_ = deps.PersistState()
		return nil

	case ubuntu.UpgradeNotConfigured:
		// "Not configured" can be either "no target" (skipped) or
		// "target set but auto_upgrade=false" (failed). Distinguish
		// by the wrapped error.
		if errors.Is(plan.Err, ubuntu.ErrNoTargetVersion) {
			deps.State.MarkSkipped(UbuntuUpgradeName, plan.Reason)
			for k, v := range resolverDetails {
				deps.State.Get(UbuntuUpgradeName).Details[k] = v
			}
			_ = deps.PersistState()
			log.Info("phase ubuntu-upgrade: skipped (no target)")
			return nil
		}
		deps.State.MarkFailed(UbuntuUpgradeName, plan.Reason, plan.Err, true)
		for k, v := range resolverDetails {
			deps.State.Get(UbuntuUpgradeName).Details[k] = v
		}
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w (%s)", plan.Err, plan.Reason)

	case ubuntu.UpgradeRefusedNonLTS, ubuntu.UpgradeRefusedUnknownHop:
		deps.State.MarkFailed(UbuntuUpgradeName, plan.Reason, plan.Err, true)
		for k, v := range resolverDetails {
			deps.State.Get(UbuntuUpgradeName).Details[k] = v
		}
		_ = deps.PersistState()
		return fmt.Errorf("phase ubuntu-upgrade: %w (%s)", plan.Err, plan.Reason)

	case ubuntu.UpgradeDoReleaseUpgrade:
		err := errors.New("phase ubuntu-upgrade: do-release-upgrade path is NOT yet implemented in v3; set deploy.direct_apt_codename_upgrade=force or wait for Milestone 4.1")
		deps.State.MarkFailed(UbuntuUpgradeName,
			"do-release-upgrade path missing from v3", err, true)
		for k, v := range resolverDetails {
			deps.State.Get(UbuntuUpgradeName).Details[k] = v
		}
		_ = deps.PersistState()
		return err

	case ubuntu.UpgradeDirectCodenameRewrite:
		if p.DirectRewriteHookFn != nil {
			return p.DirectRewriteHookFn()
		}
		return p.runDirectCodenameRewrite(ctx, deps, plan, currentVer, resolverDetails, log)
	}

	err := fmt.Errorf("unexpected upgrade action %s", plan.Action)
	deps.State.MarkFailed(UbuntuUpgradeName, "unexpected action", err, true)
	_ = deps.PersistState()
	return err
}

func (p UbuntuUpgrade) runDirectCodenameRewrite(ctx context.Context, deps *Deps, plan ubuntu.UpgradePlan, currentVer string, resolverDetails map[string]any, log *slog.Logger) error {
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
		"pre_version":          stringOrFromDetails(prev, "pre_version", currentVer),
		"target_version":       plan.TargetVersion,
		"final_target_version": plan.FinalTargetVersion,
		"current_codename":     plan.CurrentCodename,
		"target_codename":      plan.TargetCodename,
		"action":               plan.Action.String(),
		"stage":                string(UbuntuStageStarted),
		"intermediate_hop":     plan.FinalTargetVersion != "" && plan.FinalTargetVersion != plan.TargetVersion,
	}
	for k, v := range resolverDetails {
		details[k] = v
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

	// 1. Quarantine ALL non-official apt sources. Live VM evidence
	//    (2026-05-22): the previous "disable specific NVIDIA/CUDA
	//    patterns" approach missed docker.list and tailscale.list
	//    that were still pinned to jammy after Noble landed, which
	//    poisoned `apt-get update` post-reboot. The new sweep is
	//    allowlist-based: keep ubuntu.sources + ubuntu-archive
	//    URLs, move everything else aside. The cuda / tailscale /
	//    docker apt phases that come AFTER the upgrade re-create
	//    their lists against the new codename.
	if !deps.DryRun && !stageAtLeast(prevStage, UbuntuStageThirdPartyDisabled) {
		moved, kept, err := quarantineNonOfficialAptSources(sourcesDir, disabledDir, log)
		if err != nil {
			deps.State.MarkFailed(UbuntuUpgradeName, "quarantine non-official apt sources", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase ubuntu-upgrade: %w", err)
		}
		details["quarantined_sources"] = moved
		details["kept_sources"] = kept
		deps.State.Get(UbuntuUpgradeName).Details = details
		_ = deps.PersistState()
	}
	advance(UbuntuStageThirdPartyDisabled)

	// 1b. GRUB preseed. Live VM 2026-05-22: jammy -> noble reached
	//     /etc/os-release=24.04 but `apt-get dist-upgrade` died with
	//     dpkg: error processing package grub-pc (--configure)
	//     because grub-pc's debconf install-device prompt was
	//     interactive and the runner had no controlling tty. Preseed
	//     grub-pc/install_devices to the host's actual boot disk
	//     BEFORE dist-upgrade so the postinst doesn't ask.
	if !deps.DryRun {
		gd, err := preseedGrubInstallDevice(ctx, deps, log)
		if err != nil {
			// Non-fatal: many cloud VMs are pure-UEFI, in which case
			// grub-pc isn't installed and the preseed simply doesn't
			// apply. Record the attempt so the operator can see what
			// we believed the boot disk to be.
			log.Warn("phase ubuntu-upgrade: grub-pc preseed best-effort failure (likely UEFI-only host)", "err", err)
			details["grub_preseed_warning"] = err.Error()
		} else {
			details["grub_preseed_disk"] = gd.Disk
			details["grub_preseed_root_source"] = gd.RootSource
			details["grub_preseed_efi_present"] = gd.EFIPresent
		}
		deps.State.Get(UbuntuUpgradeName).Details = details
		_ = deps.PersistState()
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
		//    closed-driver stack into the new codename. Also purge
		//    snapd: on live VMs its postinst has half-failed during
		//    25.10 dist-upgrade, leaving the system in a broken
		//    state including a clobbered /etc/sudoers (which is why
		//    apt-health writes /etc/sudoers.d/90-clouddeploy-<user>
		//    earlier). CloudDeploy does not need snapd; uninstalling
		//    it pre-emptively removes the failure mode entirely.
		log.Info("phase ubuntu-upgrade: purging old NVIDIA/CUDA + snapd before codename rewrite")
		if err := tc.Purge(ctx, []string{
			"cuda-*", "nsight-*",
			"nvidia-*", "libnvidia-*",
			"xserver-xorg-video-nvidia-*",
			"snapd",
		}); err != nil {
			// Purge of non-installed wildcards is non-fatal in apt;
			// log + continue.
			log.Warn("phase ubuntu-upgrade: pre-upgrade NVIDIA/CUDA/snapd purge non-fatal warning",
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
		res := runHardenedDistUpgrade(ctx, deps)
		if res.Err != nil {
			// Live VM 2026-05-22: jammy -> noble apt-get dist-upgrade
			// died configuring grub-pc with a generic exit-1 error.
			// Try ONE targeted recovery: parse the failed package
			// names from apt's term.log, re-preseed grub-pc + run
			// dpkg --configure -a + apt-get -f install, then rerun
			// dist-upgrade. If recovery still fails, return an error
			// that names the actual failed packages.
			recovery := runDpkgRecovery(ctx, deps, log, res, details)
			details["dist_upgrade_first_error"] = res.Err.Error()
			details["dist_upgrade_first_apt_term_tail"] = recovery.AptTermTail
			details["dist_upgrade_failed_packages_initial"] = recovery.FailedPackages
			details["dist_upgrade_recovery_attempted"] = recovery.Attempted
			details["dist_upgrade_recovery_succeeded"] = recovery.RecoverySucceeded
			deps.State.Get(UbuntuUpgradeName).Details = details
			_ = deps.PersistState()
			if !recovery.Attempted || !recovery.RecoverySucceeded {
				if len(recovery.FailedPackages) > 0 {
					return fmt.Errorf("apt-get dist-upgrade failed configuring packages %v; see state.Details[\"dist_upgrade_first_apt_term_tail\"]", recovery.FailedPackages)
				}
				return fmt.Errorf("apt-get dist-upgrade: %w (stderr=%q)", res.Err, res.Stderr)
			}
		}

		// autoremove + dpkg --configure -a are best-effort cleanup.
		_ = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:   []string{"apt-get", "-y", "autoremove"},
			Env:    []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a", "APT_LISTCHANGES_FRONTEND=none"},
			Sudo:   true,
			DryRun: deps.DryRun,
		})
		_ = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:   []string{"dpkg", "--configure", "-a"},
			Env:    []string{"DEBIAN_FRONTEND=noninteractive"},
			Sudo:   true,
			DryRun: deps.DryRun,
		})

		// dpkg-audit + apt-get check gate: do NOT mark the hop done
		// (and therefore do NOT request a reboot) if dpkg still has
		// half-configured packages. The previous behavior happily
		// requested a reboot while grub-pc was half-configured, which
		// produced a host that failed to boot.
		auditFailed, audit := dpkgAuditFailedPackages(ctx, deps)
		details["dpkg_audit_failed_packages"] = audit
		if auditFailed && !deps.DryRun {
			return fmt.Errorf("post-dist-upgrade dpkg --audit reports half-configured packages %v; refusing to declare hop done", audit)
		}
		aptCheckErr := runAptGetCheck(ctx, deps)
		details["apt_check_ok"] = aptCheckErr == nil
		if aptCheckErr != nil && !deps.DryRun {
			details["apt_check_error"] = aptCheckErr.Error()
			return fmt.Errorf("post-dist-upgrade `apt-get check` failed: %w", aptCheckErr)
		}
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

// officialUbuntuSourceFilenames are the apt-source filenames that
// SHIP with Ubuntu and must NOT be quarantined. Everything else in
// /etc/apt/sources.list.d/ is treated as third-party regardless of
// what URL is inside, because:
//
//   - the codename rewrite below tries to mutate ANY file containing
//     the old codename string,
//   - leaving a Jammy-pinned PPA list active after the host becomes
//     Noble breaks apt-get update with a "Release file" mismatch,
//   - the cuda / tailscale / docker apt phases that come AFTER the
//     upgrade will recreate their lists pinned to the new codename
//     anyway.
//
// Filenames we keep:
//
//   - ubuntu.sources             - Ubuntu deb822 archive (24.04+).
//   - ubuntu-cdrom.list          - CD-ROM installer (rare on cloud).
//   - <distro>-updates.list      - some images split out updates.
//   - <distro>-security.list     - some images split out security.
//
// Live VM 2026-05-22: docker.list and tailscale.list were left active
// across the Jammy -> Noble hop and caused "Release file is not yet
// valid" warnings; this allowlist eliminates that class of bug.
var officialUbuntuSourceFilenames = map[string]bool{
	"ubuntu.sources":       true,
	"ubuntu-cdrom.list":    true,
	"ubuntu-updates.list":  true,
	"ubuntu-security.list": true,
}

// officialUbuntuSourceURLPrefixes are the URL prefixes that, when
// present in a *.list / *.sources body, mean "this is an Ubuntu-
// archive source the codename rewriter is allowed to touch".
// Anything else is third-party and gets quarantined.
var officialUbuntuSourceURLPrefixes = []string{
	"http://archive.ubuntu.com",
	"https://archive.ubuntu.com",
	"http://security.ubuntu.com",
	"https://security.ubuntu.com",
	"http://ports.ubuntu.com",
	"https://ports.ubuntu.com",
	"mirror+file:/etc/apt/apt-mirrors.txt",
	// Common cloud-provider Ubuntu mirrors.
	"http://us.archive.ubuntu.com",
	"http://eu.archive.ubuntu.com",
	"http://uk.archive.ubuntu.com",
	"http://de.archive.ubuntu.com",
	"http://fr.archive.ubuntu.com",
	"http://ca.archive.ubuntu.com",
	"http://au.archive.ubuntu.com",
	"http://jp.archive.ubuntu.com",
	// Anything ending in .archive.ubuntu.com.
}

// quarantineNonOfficialAptSources walks sourcesDir and moves EVERY
// non-official file aside into disabledDir. Returns the list of
// moved paths so the caller can record them in state diagnostics.
//
// "Official" is identified by either filename allowlist OR a body
// scan that finds only ubuntu-archive URLs (so a hand-rolled
// "my-ubuntu-mirror.list" pointing at archive.ubuntu.com still
// stays). Anything else - docker.list, tailscale.list, cuda-*.list,
// graphics-drivers PPA, vendor-pinned ROS lists, etc. - moves aside.
//
// Returns (moved, kept, error). kept includes ubuntu-archive
// equivalents so the caller can confirm the codename rewriter has
// something to rewrite.
func quarantineNonOfficialAptSources(sourcesDir, disabledDir string, log *slog.Logger) ([]string, []string, error) {
	if err := os.MkdirAll(disabledDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", disabledDir, err)
	}
	entries, err := os.ReadDir(sourcesDir)
	if err != nil {
		return nil, nil, fmt.Errorf("readdir %s: %w", sourcesDir, err)
	}
	var moved, kept []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		// Only *.list and *.sources are apt-managed source files;
		// leave keyring directories, .gpg files, .save backups etc.
		// alone.
		if ext != ".list" && ext != ".sources" {
			continue
		}
		full := filepath.Join(sourcesDir, name)
		if officialUbuntuSourceFilenames[name] {
			kept = append(kept, full)
			continue
		}
		body := ""
		if b, err := os.ReadFile(full); err == nil {
			body = string(b)
		}
		if isOfficialUbuntuSourceBody(body) {
			kept = append(kept, full)
			continue
		}
		dst := filepath.Join(disabledDir, name)
		log.Info("phase ubuntu-upgrade: quarantining non-official apt source", "src", full, "dst", dst)
		if err := os.Rename(full, dst); err != nil {
			return moved, kept, fmt.Errorf("rename %s -> %s: %w", full, dst, err)
		}
		moved = append(moved, full)
	}
	return moved, kept, nil
}

// isOfficialUbuntuSourceBody reports whether the file body looks
// like an ubuntu-archive deb / deb-src / URIs source. We require
// EVERY non-comment, non-blank line that references a URL to point
// at a recognized Ubuntu-archive host; a single third-party line
// disqualifies the file.
func isOfficialUbuntuSourceBody(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	sawURL := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// deb822 keys like "Types:", "Suites:", "Components:" etc.
		// have no URL on them; skip. We only care about URI lines.
		lowerLine := strings.ToLower(line)
		isURIKey := strings.HasPrefix(lowerLine, "uris:") ||
			strings.HasPrefix(lowerLine, "deb ") ||
			strings.HasPrefix(lowerLine, "deb-src ")
		if !isURIKey {
			continue
		}
		// Extract the URL token(s) from the line.
		tokens := strings.Fields(line)
		for _, tok := range tokens {
			if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
				continue
			}
			sawURL = true
			if !urlIsOfficialUbuntuArchive(tok) {
				return false
			}
		}
	}
	return sawURL
}

func urlIsOfficialUbuntuArchive(u string) bool {
	for _, prefix := range officialUbuntuSourceURLPrefixes {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	// Suffix match catches *.archive.ubuntu.com mirrors we don't
	// have prefixed above.
	hostStart := strings.Index(u, "//")
	if hostStart < 0 {
		return false
	}
	rest := u[hostStart+2:]
	host := rest
	if slash := strings.Index(rest, "/"); slash >= 0 {
		host = rest[:slash]
	}
	host = strings.ToLower(host)
	if strings.HasSuffix(host, ".archive.ubuntu.com") || strings.HasSuffix(host, ".security.ubuntu.com") {
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// GRUB preseed + dist-upgrade hardening
// -----------------------------------------------------------------------------

// grubPreseed describes what we ran in case the operator wants to
// confirm the disk we selected for grub-pc/install_devices.
type grubPreseed struct {
	RootSource string // e.g. /dev/vda2
	Disk       string // e.g. /dev/vda
	EFIPresent bool
}

// resolveBootDiskFromOutputs is the pure-function half of
// preseedGrubInstallDevice: given the (already-captured) outputs of
// findmnt + lsblk + the fallback "first disk in lsblk" command, it
// picks the disk path to feed to debconf. Pure for unit tests.
//
//	rootSource    : trimmed first line of `findmnt -n -o SOURCE /`,
//	                e.g. "/dev/vda2" or "/dev/dm-0".
//	parentName    : trimmed first line of `lsblk -no PKNAME <root>`,
//	                e.g. "vda" (or "" when the kernel doesn't know
//	                of a parent disk for a device-mapper child).
//	firstDiskPath : trimmed first line of
//	                `lsblk -dpno NAME,TYPE | awk '$2=="disk"{print $1; exit}'`,
//	                e.g. "/dev/nvme0n1". Used as the fallback when
//	                PKNAME yielded nothing.
//
// Returns "" only when nothing resolvable was provided.
func resolveBootDiskFromOutputs(rootSource, parentName, firstDiskPath string) string {
	parentName = strings.TrimSpace(parentName)
	if parentName != "" {
		// PKNAME can be either "vda" or already "/dev/vda"; the
		// canonical lsblk -no PKNAME output is just the name.
		if strings.HasPrefix(parentName, "/dev/") {
			return parentName
		}
		return "/dev/" + parentName
	}
	if first := strings.TrimSpace(firstDiskPath); first != "" {
		return first
	}
	// If we have a root source that IS itself a disk (no partition
	// suffix), fall back to that. Cloud VMs sometimes mount /
	// straight off /dev/vda with no partition.
	if rs := strings.TrimSpace(rootSource); rs != "" {
		// Heuristic: if rs is /dev/sda or /dev/vda or /dev/nvme0n1
		// with no trailing digit/p<n>, accept it as the disk.
		base := strings.TrimPrefix(rs, "/dev/")
		if base != "" && !strings.ContainsAny(base, "0123456789") {
			return rs
		}
	}
	return ""
}

// preseedGrubInstallDevice detects the host's boot disk and feeds it
// to debconf so the grub-pc postinst does not stop the upgrade with
// an interactive "which disk should I install GRUB on?" prompt.
//
// Live VM 2026-05-22: a Vast jammy host with grub-pc 2.12-1ubuntu7.3
// died with "installed grub-pc package post-installation script
// subprocess returned error exit status 1" in the middle of the
// jammy -> noble dist-upgrade. The postinst was waiting on
// `grub-pc/install_devices` which had been migrated out of debconf
// during the upgrade. Preseeding it back removes the failure mode.
//
// We also write install_devices_empty=false + install_devices_failed
// =false so a previous failed run's "I gave up" markers do not
// persist.
//
// Strategy:
//
//  1. ROOT_SRC = `findmnt -n -o SOURCE /`           (e.g. /dev/vda2)
//  2. DISK     = `lsblk -no PKNAME ${ROOT_SRC}`     (e.g. /dev/vda)
//  3. Fallback when PKNAME is empty: first `disk` entry from
//     `lsblk -dpno NAME,TYPE`.
//
// EFI presence is recorded only as a diagnostic; we always preseed
// grub-pc because mixed BIOS-grub + EFI installs are common on
// upgraded cloud VMs and the preseed is harmless when grub-pc is
// not installed.
func preseedGrubInstallDevice(ctx context.Context, deps *Deps, log *slog.Logger) (grubPreseed, error) {
	gp := grubPreseed{EFIPresent: pathExists("/sys/firmware/efi")}
	if deps == nil || deps.Runner == nil {
		return gp, fmt.Errorf("no runner available")
	}
	// 1. Root source.
	root := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"findmnt", "-n", "-o", "SOURCE", "/"},
		Sudo:    false,
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	rootSrc := strings.TrimSpace(strings.Split(root.Stdout, "\n")[0])
	gp.RootSource = rootSrc
	// 2. Parent disk + 3. fallback to first system disk. The actual
	//    pick happens inside resolveBootDiskFromOutputs (pure
	//    function, unit-tested below); this part just gathers the
	//    raw command outputs.
	pkname := ""
	if rootSrc != "" {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"lsblk", "-no", "PKNAME", rootSrc},
			Sudo:    false,
			LogFile: "-",
			Timeout: 10 * time.Second,
		})
		pkname = strings.TrimSpace(strings.Split(res.Stdout, "\n")[0])
	}
	firstDisk := ""
	all := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"bash", "-lc", `lsblk -dpno NAME,TYPE | awk '$2=="disk"{print $1; exit}'`},
		Sudo:    false,
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	firstDisk = strings.TrimSpace(strings.Split(all.Stdout, "\n")[0])
	disk := resolveBootDiskFromOutputs(rootSrc, pkname, firstDisk)
	if disk == "" {
		return gp, fmt.Errorf("could not determine boot/root disk (root source=%q, pkname=%q, first_disk=%q)", rootSrc, pkname, firstDisk)
	}
	gp.Disk = disk

	// Feed the selections to debconf. We use bash -lc so the heredoc
	// + multiple debconf-set-selections invocations run cleanly under
	// the runner.
	script := `set -e
echo "grub-pc grub-pc/install_devices multiselect ` + disk + `" | debconf-set-selections
echo "grub-pc grub-pc/install_devices_empty boolean false" | debconf-set-selections
echo "grub-pc grub-pc/install_devices_failed boolean false" | debconf-set-selections
echo "grub-pc grub-pc/install_devices_failed_upgrade boolean false" | debconf-set-selections
# Some grub2-common variants ask the same on every dist-upgrade.
echo "grub2-common grub2/linux_cmdline string"          | debconf-set-selections || true
echo "grub2-common grub2/linux_cmdline_default string ` + grubDefaultCmdline() + `" | debconf-set-selections || true
`
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"bash", "-lc", script},
		Sudo:    true,
		LogFile: "-",
		Timeout: 30 * time.Second,
	})
	if res.Err != nil {
		if log != nil {
			log.Warn("phase ubuntu-upgrade: debconf-set-selections returned non-zero (grub-pc may not be installed yet)", "err", res.Err, "stderr", res.Stderr)
		}
		// Non-fatal: when grub-pc is not yet installed the debconf
		// owner doesn't exist; that's fine because the same package
		// install during dist-upgrade will see the queued seed.
	}
	if log != nil {
		log.Info("phase ubuntu-upgrade: preseeded grub-pc install_devices", "disk", disk, "root_source", rootSrc, "efi_present", gp.EFIPresent)
	}
	return gp, nil
}

// grubDefaultCmdline mirrors what we want GRUB_CMDLINE_LINUX_DEFAULT
// to contain after the upgrade. We do NOT touch /etc/default/grub
// here -- the edid phase already manages that. We only need a sane
// default for grub2-common's debconf prompt so it doesn't ask.
func grubDefaultCmdline() string {
	// "quiet splash" is Ubuntu's distro default; the edid phase
	// rewrites /etc/default/grub with the real cmdline anyway.
	return "quiet splash"
}

// runHardenedDistUpgrade is the actual `apt-get dist-upgrade`
// invocation. The flags / env collectively neutralise every known
// interactive trap in the Ubuntu noninteractive-upgrade lore:
//
//   - DEBIAN_FRONTEND=noninteractive             :: no dpkg prompts.
//   - APT_LISTCHANGES_FRONTEND=none              :: skip "what changed
//     in this version" pager.
//   - NEEDRESTART_MODE=a / NEEDRESTART_SUSPEND=1 :: needrestart
//     auto-restarts services without asking.
//   - Dpkg::Options::=--force-confdef            :: keep maintainer
//     default for changed conffiles.
//   - Dpkg::Options::=--force-confold            :: keep operator's
//     version when in doubt (vs --force-confnew, which clobbered
//     /etc/default/grub on the live VM and may have contributed
//     to the grub-pc postinst failure).
//   - --allow-downgrades / --allow-remove-essential / --allow-change-
//     held-packages :: noble's package set differs from jammy's in
//     ways that legitimately require these flags.
func runHardenedDistUpgrade(ctx context.Context, deps *Deps) runner.Result {
	return deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"apt-get",
			"-y",
			"-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold",
			"--allow-downgrades",
			"--allow-remove-essential",
			"--allow-change-held-packages",
			"dist-upgrade",
		},
		Env: []string{
			"DEBIAN_FRONTEND=noninteractive",
			"APT_LISTCHANGES_FRONTEND=none",
			"NEEDRESTART_MODE=a",
			"NEEDRESTART_SUSPEND=1",
		},
		Sudo:    true,
		Timeout: deps.APT.AptTimeout,
		DryRun:  deps.DryRun,
	})
}

// -----------------------------------------------------------------------------
// dpkg recovery + audit
// -----------------------------------------------------------------------------

// aptTermLogPath is the standard apt+dpkg transcript on a Debian/
// Ubuntu host. Live VM 2026-05-22 had the real grub-pc failure here
// while the apt-get exit reported only "Sub-process /usr/bin/dpkg
// returned an error code (1)".
const aptTermLogPath = "/var/log/apt/term.log"

// dpkgRecoveryResult captures one targeted recovery attempt.
type dpkgRecoveryResult struct {
	FailedPackages    []string
	AptTermTail       string
	Attempted         bool
	RecoverySucceeded bool
}

// runDpkgRecovery tries ONE bounded recovery cycle after a failed
// dist-upgrade:
//
//  1. Read /var/log/apt/term.log tail.
//  2. Parse failed packages out of the tail.
//  3. If any failed package is a grub/shim variant, re-run the
//     grub-pc preseed.
//  4. `dpkg --configure -a` and `apt-get -f install` (both with the
//     hardened env).
//  5. One re-attempt of `apt-get dist-upgrade`.
//
// Bounded: we set Attempted=true and either succeed or give up. No
// loop. RecoverySucceeded=true iff the rerun of dist-upgrade
// returned exit 0 AND dpkg --audit afterwards is clean.
func runDpkgRecovery(ctx context.Context, deps *Deps, log *slog.Logger, firstFail runner.Result, _ map[string]any) dpkgRecoveryResult {
	out := dpkgRecoveryResult{}
	out.AptTermTail = readLastLines(aptTermLogPath, 240)
	out.FailedPackages = parseAptFailedPackages(out.AptTermTail + "\n" + firstFail.Stdout + "\n" + firstFail.Stderr)
	if !mentionsGrubOrShim(out.FailedPackages) && len(out.FailedPackages) == 0 {
		// No actionable failure signature to recover from.
		return out
	}
	out.Attempted = true
	if log != nil {
		log.Warn("phase ubuntu-upgrade: dist-upgrade failed; attempting one targeted recovery", "failed_packages", out.FailedPackages)
	}
	// Re-preseed grub-pc, in case the previous dist-upgrade migrated
	// the old debconf answers out from under us.
	if mentionsGrubOrShim(out.FailedPackages) {
		_, _ = preseedGrubInstallDevice(ctx, deps, log)
	}
	// dpkg --configure -a + apt-get -f install with the hardened env.
	configureRes := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"dpkg", "--configure", "-a"},
		Env:     []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_MODE=a"},
		Sudo:    true,
		Timeout: deps.APT.AptTimeout,
		LogFile: "-",
	})
	if log != nil && configureRes.Err != nil {
		log.Warn("phase ubuntu-upgrade: dpkg --configure -a returned non-zero during recovery", "err", configureRes.Err)
	}
	fixRes := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"apt-get",
			"-y",
			"-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold",
			"-f", "install",
		},
		Env:     []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_MODE=a"},
		Sudo:    true,
		Timeout: deps.APT.AptTimeout,
		LogFile: "-",
	})
	if log != nil && fixRes.Err != nil {
		log.Warn("phase ubuntu-upgrade: apt-get -f install returned non-zero during recovery", "err", fixRes.Err)
	}
	// One more dist-upgrade attempt with the hardened env.
	rerun := runHardenedDistUpgrade(ctx, deps)
	if rerun.Err == nil {
		auditFailed, _ := dpkgAuditFailedPackages(ctx, deps)
		if !auditFailed {
			out.RecoverySucceeded = true
			return out
		}
		if log != nil {
			log.Warn("phase ubuntu-upgrade: dist-upgrade rerun exited 0 but dpkg --audit still dirty; attempting BIOS-only EFI/shim purge fallback")
		}
	} else if log != nil {
		log.Warn("phase ubuntu-upgrade: second dist-upgrade attempt also failed; considering BIOS-only purge fallback", "err", rerun.Err)
	}

	// BIOS-only purge fallback. Live VM 2026-05-22: when grub-pc is
	// the actual boot path on a non-UEFI cloud VM, the residual
	// grub-efi-amd64-signed + shim-signed packages keep dpkg dirty
	// even after grub-pc configures cleanly. They serve no purpose
	// on BIOS; purge them so dpkg --audit goes clean and the deploy
	// can advance. We guard on /sys/firmware/efi to avoid bricking
	// real UEFI hosts.
	if !pathExists("/sys/firmware/efi") && stillStuckOnGrubEFIOrShim(ctx, deps) {
		if log != nil {
			log.Warn("phase ubuntu-upgrade: BIOS-only host with stuck grub-efi/shim packages; purging them and rerunning")
		}
		purgeRes := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv: []string{
				"apt-get",
				"-y",
				"-o", "Dpkg::Options::=--force-confdef",
				"-o", "Dpkg::Options::=--force-confold",
				"purge",
				"grub-efi-amd64", "grub-efi-amd64-signed", "grub-efi-amd64-bin", "shim-signed",
			},
			Env:     []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_MODE=a"},
			Sudo:    true,
			LogFile: "-",
			Timeout: deps.APT.AptTimeout,
		})
		if log != nil && purgeRes.Err != nil {
			log.Warn("phase ubuntu-upgrade: BIOS purge fallback returned non-zero", "err", purgeRes.Err)
		}
		_ = deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"dpkg", "--configure", "-a"},
			Env:     []string{"DEBIAN_FRONTEND=noninteractive"},
			Sudo:    true,
			LogFile: "-",
			Timeout: deps.APT.AptTimeout,
		})
		// Final audit check.
		auditFailed, _ := dpkgAuditFailedPackages(ctx, deps)
		if !auditFailed {
			out.RecoverySucceeded = true
			return out
		}
	}

	out.RecoverySucceeded = false
	return out
}

// stillStuckOnGrubEFIOrShim returns true when dpkg --audit reports at
// least one grub-efi-* or shim-* package as broken. Used by the
// BIOS-only purge fallback to decide whether the residual EFI
// packages are the actual blocker.
func stillStuckOnGrubEFIOrShim(ctx context.Context, deps *Deps) bool {
	failed, pkgs := dpkgAuditFailedPackages(ctx, deps)
	if !failed {
		return false
	}
	return biosEFIPurgeCandidate(pkgs)
}

func biosEFIPurgeCandidate(pkgs []string) bool {
	hasEFIOrShim := false
	for _, p := range pkgs {
		lp := strings.ToLower(p)
		switch {
		case lp == "grub-pc" || lp == "grub-gfxpayload-lists":
			// Conservative guard: only purge residual EFI/shim packages
			// after the BIOS grub-pc path itself is no longer broken.
			return false
		case strings.HasPrefix(lp, "grub-efi") || strings.HasPrefix(lp, "shim-"):
			hasEFIOrShim = true
		}
	}
	return hasEFIOrShim
}

// parseAptFailedPackages walks the combined dist-upgrade output and
// pulls out package names from the two well-known apt failure lines:
//
//	dpkg: error processing package <name> (--configure):
//	  Errors were encountered while processing:
//	   <name1>
//	   <name2>
//
// Returns a deduplicated, ordered slice.
func parseAptFailedPackages(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	inErrorList := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trim := strings.TrimSpace(line)
		// "dpkg: error processing package NAME ..."
		if idx := strings.Index(trim, "error processing package "); idx >= 0 {
			rest := trim[idx+len("error processing package "):]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				add(strings.TrimSuffix(fields[0], ":"))
			}
		}
		// The "Errors were encountered while processing:" block. apt
		// prints one indented package per line until the block ends.
		if strings.HasPrefix(trim, "Errors were encountered while processing") {
			inErrorList = true
			continue
		}
		if inErrorList {
			// The block ends at an unindented or empty line.
			if !strings.HasPrefix(line, " ") || trim == "" {
				inErrorList = false
				continue
			}
			add(trim)
		}
	}
	return out
}

// mentionsGrubOrShim reports whether any failed-package name looks
// like a GRUB / shim / EFI bootloader package. We use this both to
// gate "should I re-preseed grub-pc?" and to make the human-readable
// error tag the failure clearly.
func mentionsGrubOrShim(pkgs []string) bool {
	for _, p := range pkgs {
		lp := strings.ToLower(p)
		if strings.HasPrefix(lp, "grub-") || strings.HasPrefix(lp, "shim-") || lp == "grub2-common" {
			return true
		}
	}
	return false
}

// parseDpkgAuditOutput is the pure-function half of
// dpkgAuditFailedPackages. The audit output has two well-known
// sections; we collect every indented single-token entry from any of
// them. See the dpkgAuditFailedPackages doc-comment for the live VM
// fixture this matches.
func parseDpkgAuditOutput(out string) []string {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	var pkgs []string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trim := strings.TrimSpace(line)
		// Indented + single token + no spaces + does NOT end with ":"
		// (which would be a section header).
		if strings.HasPrefix(line, " ") && trim != "" && !strings.Contains(trim, " ") && !strings.HasSuffix(trim, ":") {
			pkgs = append(pkgs, trim)
		}
	}
	return pkgs
}

// dpkgAuditFailedPackages runs `dpkg --audit` and returns the set of
// package names dpkg reports as not fully installed/configured.
// Returns (true, names) when any are present; (false, nil) on clean.
//
// Live VM 2026-05-22:
//
//	$ dpkg --audit
//	The following packages are in a mess [...]
//	  grub-efi-amd64-signed
//	  grub-gfxpayload-lists
//	  shim-signed
//
//	The following packages have been unpacked but not yet configured.
//	They must be configured using dpkg --configure or the configure
//	menu option in dselect for them to work:
//	  grub-pc
//
// Both blocks need to be empty before we declare the hop done.
func dpkgAuditFailedPackages(ctx context.Context, deps *Deps) (bool, []string) {
	if deps == nil || deps.Runner == nil {
		return false, nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"dpkg", "--audit"},
		Sudo:    true,
		LogFile: "-",
		Timeout: 30 * time.Second,
		DryRun:  deps.DryRun,
	})
	pkgs := parseDpkgAuditOutput(res.Stdout)
	if len(pkgs) == 0 {
		return false, nil
	}
	return true, pkgs
}

// runAptGetCheck wraps `apt-get check`. Returns nil when apt's
// dependency closure is consistent. Useful as a final-pass sanity
// gate after dist-upgrade + recovery before we set RebootNeeded.
func runAptGetCheck(ctx context.Context, deps *Deps) error {
	if deps == nil || deps.Runner == nil {
		return nil
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"apt-get", "-y", "check"},
		Env:     []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"},
		Sudo:    true,
		LogFile: "-",
		Timeout: 60 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("%w (stderr=%q)", res.Err, lastLines(res.Stderr, 8))
	}
	return nil
}

// readLastLines returns the last N lines of a file as a string,
// trimmed. Used for capturing apt term.log tails without pulling
// the whole multi-MB file into memory.
func readLastLines(path string, n int) string {
	if n <= 0 {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// rewriteAptCodename replaces every occurrence of `from` with `to`
// in all active *.list/*.sources files plus /etc/apt/sources.list.
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
func activeAptSourceFilesForRewrite(sourcesDir, sourcesListPath string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	if entries, err := os.ReadDir(sourcesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".list" || ext == ".sources" {
				add(filepath.Join(sourcesDir, e.Name()))
			}
		}
	}
	add(sourcesListPath)
	return out
}
func rewriteAptCodename(sourcesDir, sourcesListPath, from, to string, log *slog.Logger) error {
	rewrote := false
	alreadyAtTarget := false

	candidates := activeAptSourceFilesForRewrite(sourcesDir, sourcesListPath)
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
