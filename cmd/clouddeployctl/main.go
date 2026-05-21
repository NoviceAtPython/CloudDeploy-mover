// Command clouddeployctl is the v3 CloudDeploy orchestrator entry point.
//
// State at Milestone 5 (in progress): doctor (apt/nvidia/cuda/system) is
// real and read-only; apply / resume implement apt-health -> ubuntu-
// upgrade -> base-packages -> nvidia-driver -> cuda -> edid ->
// headless-user -> desktop-packages -> desktop-runtime -> kwin-patch
// -> kwin-session -> drm-display-validate -> Sunshine/Tailscale/
// PipeWire/service/stream phases. KWin real-VT (direct kwin_wayland)
// is the validated compositor default.
//
// See docs/V2-V3-PARITY.md for the formal v2 -> v3 capability audit,
// docs/V3-DEPLOYMENT-READINESS.md for the rollout plan and
// docs/V3-ROADMAP.md for milestone-by-milestone status.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/discovery"
	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/phase"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/reboot"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
	sunshinecodec "github.com/NoviceAtPython/CloudDeploy-mover/internal/sunshine"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/ubuntu"
)

// Version is overridden via -ldflags '-X main.Version=...'.
var Version = "v3.0.0-dev"

// defaultConfigDir is where bootstrap.sh deploys the repo.
const defaultConfigDir = "/opt/clouddeploy-mover/config"

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clouddeployctl",
		Short:         "CloudDeploy v3 orchestrator (Ubuntu-only)",
		Long:          longDescription,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().String("profile", "hdr-4k120", "deploy profile from config/profiles/<name>.yaml")
	root.PersistentFlags().String("state-path", state.DefaultPath, "path to the CloudDeploy state file")
	root.PersistentFlags().String("lock-path", state.DefaultLockPath, "path to the CloudDeploy state lock file")
	root.PersistentFlags().String("config-dir", defaultConfigDir, "path to the config/ directory")
	root.PersistentFlags().Bool("dry-run", false, "do not make destructive changes; print what would happen")
	root.PersistentFlags().Bool("verbose", false, "verbose logging")
	root.PersistentFlags().Bool("allow-unsupported", false, "allow applying on explicitly unsupported Ubuntu versions")
	root.PersistentFlags().Bool("auto-reboot", false, "override deploy.auto_reboot=true for this run")
	root.PersistentFlags().Bool("unattended", false, "unattended autopilot mode; implies --auto-reboot and noninteractive deploy behavior")

	root.AddCommand(newApplyCmd())
	root.AddCommand(newResumeCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newPhaseCmd())
	root.AddCommand(newStateCmd())
	root.AddCommand(newCollectLogsCmd())
	root.AddCommand(newMonitorCmd())

	return root
}

const longDescription = `clouddeployctl is the v3 CloudDeploy orchestrator.

Ubuntu-only. See docs/UBUNTU-ONLY.md.

State is persisted in /var/lib/clouddeploy/state.json. Logs go under
/var/log/clouddeploy/. See docs/ARCHITECTURE.md for the design,
docs/V3-ROADMAP.md for milestone status, and
docs/V3-DEPLOYMENT-READINESS.md for the current readiness audit.

Milestone 5 (in progress):
  doctor apt | nvidia | cuda | system | kwin |
         kwin-patch | drm-display | sunshine |
         tools | network | lock                         read-only checks
  phase apt-health | ubuntu-upgrade | base-packages |
        nvidia-driver | cuda | edid |
        headless-user | desktop-packages |
        desktop-runtime | kwin-patch |
        kwin-session | drm-display-validate |
        sunshine-build | sunshine-config |
        tailscale | pipewire-audio |
        streaming-services | stream-validate |
        optional-apps                                     implemented
  apply                                                runs the implemented
                                                       phases above
  resume                                               continues after
                                                       a reboot`

// -----------------------------------------------------------------------------
// helpers shared across commands
// -----------------------------------------------------------------------------

// loadDeps builds the shared phase Deps from CLI flags. profileRequired
// = true means we must have a parseable profile; false = best-effort
// (used by doctor commands that should still print something when no
// profile is configured).
func loadDeps(cmd *cobra.Command, profileRequired bool) (*phase.Deps, *state.Lock, error) {
	profileName, _ := cmd.Flags().GetString("profile")
	statePath, _ := cmd.Flags().GetString("state-path")
	configDir, _ := cmd.Flags().GetString("config-dir")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	verbose, _ := cmd.Flags().GetBool("verbose")
	autoRebootFlag, _ := cmd.Flags().GetBool("auto-reboot")
	unattendedFlag, _ := cmd.Flags().GetBool("unattended")
	autoReboot := autoRebootFlag || envBool("CLOUDDEPLOY_AUTO_REBOOT")
	unattended := unattendedFlag || envBool("CLOUDDEPLOY_UNATTENDED")
	if unattended {
		autoReboot = true
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	var profile *config.Profile
	if configDir != "" && profileName != "" {
		p, err := config.LoadProfile(configDir, profileName)
		if err != nil {
			if profileRequired {
				return nil, nil, fmt.Errorf("load profile %q from %s: %w", profileName, configDir, err)
			}
			logger.Warn("could not load profile (continuing)",
				"profile", profileName, "config_dir", configDir, "err", err)
		} else {
			if err := config.ValidateProfile(p); err != nil {
				if profileRequired {
					return nil, nil, fmt.Errorf("invalid profile %q: %w", profileName, err)
				}
				logger.Warn("profile validation failed (continuing)",
					"profile", profileName, "err", err)
			}
			profile = p
		}
	}
	if profile != nil {
		if autoReboot {
			profile.Deploy.AutoReboot = true
		}
		if unattended {
			profile.Deploy.Unattended = true
			profile.Deploy.AutoReboot = true
		}
	}

	st, err := state.Load(statePath)
	if err != nil {
		if !os.IsNotExist(err) && profileRequired {
			return nil, nil, fmt.Errorf("load state %s: %w", statePath, err)
		}
		// Fresh state.
		profileNameForState := profileName
		if profile != nil {
			profileNameForState = profile.Profile
		}
		st = state.New(profileNameForState)
	}

	// Acquire the process lock if this is a mutating command. Doctor
	// / state-show callers can skip it; we surface a lock holder in
	// the error returned from this helper.
	//
	// AcquireWithRecovery also tells us whether a previous run left a
	// dead-PID lock that we just stole; we surface that to the
	// operator and record it under state.Details["startup_recovery"].
	var lock *state.Lock
	if profileRequired {
		l, rec, err := state.AcquireWithRecovery("")
		if err != nil {
			return nil, nil, fmt.Errorf("acquire process lock: %w", err)
		}
		lock = l
		if rec.Recovered() {
			fmt.Println()
			if rec.StalePID > 0 {
				fmt.Printf("Stale CloudDeploy lock recovered: previous holder PID %d is dead. Lock=%s\n",
					rec.StalePID, rec.LockPath)
			} else if rec.StaleLockUnreadable {
				fmt.Printf("Stale CloudDeploy lock recovered: lock file at %s was unreadable.\n", rec.LockPath)
			}
			fmt.Println()
			if st != nil {
				if st.StartupRecovery == nil {
					st.StartupRecovery = &state.StartupRecovery{}
				}
				st.StartupRecovery.StaleLockRecovered = true
				st.StartupRecovery.StalePID = rec.StalePID
				st.StartupRecovery.LockPath = rec.LockPath
				st.StartupRecovery.At = nowPtr()
			}
		}
	}

	r := runner.New()
	r.PrintToStdout = false

	tx := &apt.Transaction{
		Env:       &apt.Env{FS: apt.RealFS{}},
		Runner:    r,
		AssumeYes: true,
		DryRun:    dryRun,
	}

	deps := &phase.Deps{
		Runner:     r,
		APT:        tx,
		State:      st,
		Profile:    profile,
		Logger:     logger,
		DryRun:     dryRun,
		AutoReboot: autoReboot,
		Unattended: unattended,
		StatePath:  statePath,
		ConfigDir:  configDir,
	}
	return deps, lock, nil
}

// nowPtr returns a non-nil *time.Time of "now in UTC" for the state
// startup-recovery / phase-completion timestamps.
func nowPtr() *time.Time {
	t := time.Now().UTC()
	return &t
}

func recoverInterruptedPhasesOnStartup(deps *phase.Deps, profileName string) error {
	if deps == nil || deps.State == nil {
		return nil
	}
	recovered := deps.State.RecoverInterruptedRunningPhases(profileName)
	if len(recovered) == 0 {
		return nil
	}
	sort.Slice(recovered, func(i, j int) bool {
		return recovered[i].Name < recovered[j].Name
	})

	// Fold the recovered phase names into the startup_recovery
	// summary so `state show` exposes the full picture without the
	// operator having to scroll back through stdout.
	if deps.State.StartupRecovery == nil {
		deps.State.StartupRecovery = &state.StartupRecovery{}
	}
	names := make([]string, 0, len(recovered))
	for _, r := range recovered {
		names = append(names, r.Name)
	}
	deps.State.StartupRecovery.RecoveredPhases = names
	if deps.State.StartupRecovery.At == nil {
		deps.State.StartupRecovery.At = nowPtr()
	}

	fmt.Println()
	fmt.Println("Detected interrupted CloudDeploy phase(s) from a previous run:")
	for _, r := range recovered {
		if r.PreviousStartedAt != nil {
			fmt.Printf("  - %s (was running since %s)\n", r.Name, r.PreviousStartedAt.Format(time.RFC3339))
		} else {
			fmt.Printf("  - %s (was running)\n", r.Name)
		}
	}
	fmt.Println("No active previous clouddeployctl process lock blocked this run, so these phase(s) were marked pending and will be retried.")
	fmt.Println("Manual recovery guidance, if you prefer to reset explicitly:")
	for _, r := range recovered {
		for _, g := range r.Guidance {
			fmt.Printf("  sudo %s\n", g)
		}
	}
	fmt.Println()

	if err := deps.PersistState(); err != nil {
		return fmt.Errorf("persist interrupted phase recovery: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// apply
// -----------------------------------------------------------------------------

const partialApplyBanner = `
================================================================================
  Milestone 5 apply reached the end of the implemented v3 pipeline.

  Implemented:   apt-health, ubuntu-upgrade, base-packages, nvidia-driver,
                 cuda, edid, headless-user, desktop-packages, desktop-runtime,
                 kwin-patch, kwin-session, drm-display-validate,
                 sunshine-build, sunshine-config, tailscale, pipewire-audio,
                 streaming-services, stream-validate, optional-apps

  Optional apps are nonfatal and run last. stream-validate may leave the
  state at pending_moonlight_connect when Sunshine is reachable but no
  Moonlight client has produced KMS/NVENC journal markers yet.

  Use clouddeployctl monitor for live status and continuation logs.
================================================================================
`

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Run the v3 deploy phases through Sunshine stream validation",
		Long: `Run the v3 deploy pipeline in order: apt-health, ubuntu-upgrade,
base-packages, nvidia-driver, cuda, edid, headless-user,
desktop-packages, desktop-runtime, kwin-patch, kwin-session,
drm-display-validate, Sunshine build/config, Tailscale, PipeWire,
streaming services, stream validation, and optional apps. Each phase is
idempotent: re-running apply on the same VM skips phases already marked
done.

ubuntu-upgrade is the first phase because dist-upgrade rewrites the
apt world; it only acts when profile.ubuntu_version differs from the
host VERSION_ID AND deploy.auto_upgrade_ubuntu=true. See
docs/V2-V3-PARITY.md for the supported upgrade hops.

When a phase requests a reboot, apply writes state, schedules a
reboot via the clouddeploy-continue.service, and exits 2. After the
reboot the continuation service invokes 'clouddeployctl resume' which
picks up where apply left off.

Note: deploy.auto_reboot defaults to false unless explicitly set to true
in the active profile or via --auto-reboot / CLOUDDEPLOY_AUTO_REBOOT=1.
--unattended / CLOUDDEPLOY_UNATTENDED=1 implies automatic reboot.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			deps, lock, err := loadDeps(cmd, true)
			if err != nil {
				return err
			}
			defer lock.Release()

			if err := recoverInterruptedPhasesOnStartup(deps, profileName(cmd)); err != nil {
				return err
			}

			allowUnsup, _ := cmd.Flags().GetBool("allow-unsupported")

			var targetVersion string
			autoUpgrade := false
			useResolver := false
			if deps.Profile != nil {
				targetVersion = deps.Profile.UbuntuVersion
				autoUpgrade = deps.Profile.Deploy.AutoUpgradeUbuntu
				useResolver = len(deps.Profile.Deploy.UbuntuCandidates) > 0
			}
			res, err := ubuntu.ReadAndGate(ubuntu.DefaultOSReleasePath, targetVersion)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("ubuntu gate read: %w", err)
			}
			// Special case: if the host is currently a v3-supported
			// release but profile targets a different one, and
			// deploy.auto_upgrade_ubuntu=true, defer the exact-match
			// gate to the ubuntu-upgrade phase. Without this, apply
			// would refuse to run on a fresh 24.04 image even though
			// ubuntu-upgrade is the whole point of going first.
			//
			// Same deferral applies when the profile uses the OS
			// resolver (ubuntu_version="" + ubuntu_candidates=[...]):
			// the resolver authoritatively picks a target during the
			// phase, so the exact-match gate is meaningless here.
			deferToUpgradePhase := false
			if !res.Supported && autoUpgrade && (targetVersion != "" || useResolver) {
				if ubuntu.IsSupportedVersion(res.Release.VersionID) {
					deferToUpgradePhase = true
				}
			}
			if !res.Supported && !deferToUpgradePhase {
				fmt.Printf("OS unsupported:\n  Found: %s (version %s)\n  Supported: %v\n  Reason: %s\n",
					res.Release.ID, res.Release.VersionID, ubuntu.SupportedVersions(), res.Reason)
				if !allowUnsup {
					return fmt.Errorf("bailing out due to unsupported OS. Use --allow-unsupported to override")
				}
				fmt.Println("Warning: proceeding anyway due to --allow-unsupported")
			}
			if deferToUpgradePhase {
				if useResolver {
					fmt.Printf("Host on Ubuntu %s; profile uses ubuntu_candidates=%v and deploy.auto_upgrade_ubuntu=true; ubuntu-upgrade phase will resolve a target.\n",
						res.Release.VersionID, deps.Profile.Deploy.UbuntuCandidates)
				} else {
					fmt.Printf("Host on Ubuntu %s; profile targets %s and deploy.auto_upgrade_ubuntu=true; ubuntu-upgrade phase will reconcile.\n",
						res.Release.VersionID, targetVersion)
				}
			}

			err = runPhasesAndBanner(ctx, deps, false)
			releaseErr := lock.Release()
			if err == nil && releaseErr != nil {
				return releaseErr
			}

			if errors.Is(err, phase.ErrRebootRequired) {
				os.Exit(2)
			}
			if err == nil {
				return nil
			}
			return err
		},
	}
}

// runPhasesAndBanner is shared between apply and resume.
//
// Order is load-bearing:
//
//  1. apt-health: preserve operator sudo + repair dpkg journal +
//     detect mid-upgrade state. MUST run before any destructive apt
//     work so a broken dpkg database doesn't half-finish a
//     dist-upgrade and lock the operator out of sudo.
//  2. ubuntu-upgrade: rewrites the apt world; must precede any apt
//     install. No-op on a host that already matches profile.ubuntu_version.
//  3. base-packages: build-essential / git / dkms / curl etc.
//  4. nvidia-driver: picks family + installs the right metapackage.
//  5. cuda: only if profile.cuda.mode != none.
//  6. edid: stamps a kernel cmdline edid override if the profile asks.
func runPhasesAndBanner(ctx context.Context, deps *phase.Deps, isResume bool) error {
	phases := applyPhases()
	if isResume {
		waitForResumeBootReadiness(ctx, deps)
	}

	// Live-VM regression fix: previously we disabled the continuation
	// systemd unit at the START of resume. That unit was the very
	// thing that had just invoked us, so systemd treated the
	// disable+daemon-reload as the unit removing itself mid-execution
	// and killed the resume run before any phase started -
	// /var/log/clouddeploy/continue.log would contain only:
	//   "resume: Disabling continuation service..."
	// followed by silence. We now leave the unit in place while we
	// run; it gets cleaned up ONLY when every phase reached a
	// terminal-done state AND no further reboot is queued.
	for _, p := range phases {
		fmt.Printf("\n=== phase %s ===\n", p.Name())
		if err := p.Run(ctx, deps); err != nil {
			if errors.Is(err, phase.ErrRebootRequired) {
				fmt.Println("\nphase requested a reboot to continue.")
				return handleRebootRequired(ctx, deps)
			}
			return err
		}
	}
	// Clean final completion. NOW we can safely disable the
	// continuation unit (only on resume; apply never installed it on
	// its own without a phase needing reboot, but Disable is
	// idempotent so the apply path can also call it safely).
	if isResume {
		if err := disableContinuationIfPresent(ctx, deps); err != nil {
			// Non-fatal: the deploy reached terminal-done. We log
			// rather than fail the whole apply.
			fmt.Printf("resume: warning: failed to clean up continuation unit: %v\n", err)
		}
	}
	fmt.Print(partialApplyBanner)
	return nil
}

func waitForResumeBootReadiness(ctx context.Context, deps *phase.Deps) {
	if deps == nil || deps.Runner == nil {
		return
	}
	fmt.Println("resume: waiting briefly for boot-time package managers/cloud-init to settle...")
	deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"bash", "-lc", "command -v cloud-init >/dev/null 2>&1 && timeout 120 cloud-init status --wait || true"},
		Sudo:    true,
		LogFile: "-",
		Timeout: 130 * time.Second,
		DryRun:  deps.DryRun,
	})
	if deps.Unattended {
		deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv: []string{"bash", "-lc", "systemctl stop apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service 2>/dev/null || true"},
			Sudo: true, LogFile: "-", Timeout: 30 * time.Second, DryRun: deps.DryRun,
		})
	}
	if deps.APT != nil {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			if holders := deps.APT.LockHolders(ctx); len(holders) == 0 {
				return
			}
			time.Sleep(2 * time.Second)
		}
	}
}

func applyPhases() []phase.Phase {
	return []phase.Phase{
		phase.AptHealth{},
		phase.UbuntuUpgrade{},
		phase.BasePackages{},
		phase.NvidiaDriver{},
		phase.Cuda{},
		phase.Edid{},
		// Milestone 4A/4B: headless KDE/KWin Wayland substrate. These
		// phases run after the kernel-cmdline reboot from edid so
		// /proc/cmdline already has nvidia-drm.modeset=1 +
		// drm.edid_firmware=DP-1:edid/<file>.
		phase.HeadlessUser{},
		phase.DesktopPackages{},
		phase.DesktopRuntime{},
		phase.KWinPatch{},
		phase.KWinSession{},
		phase.DRMDisplayValidate{},
		phase.SunshineBuild{},
		phase.SunshineConfigPhase{},
		phase.Tailscale{},
		phase.PipeWireAudio{},
		phase.StreamingServices{},
		phase.StreamValidate{},
		phase.OptionalApps{},
	}
}

// disableContinuationIfPresent removes /etc/systemd/system/clouddeploy-
// v3-continue.service + the env-file iff the unit exists. Idempotent.
// Called ONLY after every phase reached terminal-done AND no reboot is
// queued.
func disableContinuationIfPresent(ctx context.Context, deps *phase.Deps) error {
	svc := &reboot.Service{Runner: deps.Runner, DryRun: deps.DryRun}
	if !svc.IsInstalled() {
		return nil
	}
	fmt.Println("resume: deploy fully complete; disabling continuation service")
	return svc.Disable(ctx)
}

// -----------------------------------------------------------------------------
// resume
// -----------------------------------------------------------------------------

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume a deploy after a reboot",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, lock, err := loadDeps(cmd, true)
			if err != nil {
				return err
			}
			defer lock.Release()

			if err := recoverInterruptedPhasesOnStartup(deps, profileName(cmd)); err != nil {
				return err
			}

			if deps.State.RebootNeeded {
				deps.State.ClearRebootNeeded()
				_ = deps.PersistState()
				fmt.Println("resume: clearing RebootNeeded marker")
			}

			err = runPhasesAndBanner(ctx, deps, true)
			releaseErr := lock.Release()
			if err == nil && releaseErr != nil {
				return releaseErr
			}

			if errors.Is(err, phase.ErrRebootRequired) {
				os.Exit(2)
			}
			if err == nil {
				return nil
			}
			return err
		},
	}
}

// -----------------------------------------------------------------------------
// doctor
// -----------------------------------------------------------------------------

func newDoctorCmd() *cobra.Command {
	doctor := &cobra.Command{
		Use:   "doctor [subsystem]",
		Short: "Print a per-subsystem readiness report (read-only)",
	}
	doctor.AddCommand(newDoctorAptCmd())
	doctor.AddCommand(newDoctorNvidiaCmd())
	doctor.AddCommand(newDoctorCudaCmd())
	doctor.AddCommand(newDoctorSystemCmd())
	doctor.AddCommand(newDoctorKwinCmd())
	doctor.AddCommand(newDoctorKwinPatchCmd())
	doctor.AddCommand(newDoctorDRMDisplayCmd())
	doctor.AddCommand(newDoctorSunshineCmd())
	doctor.AddCommand(newDoctorSunshineWebCmd())
	doctor.AddCommand(newDoctorStreamSessionCmd())
	doctor.AddCommand(newDoctorCodecsCmd())
	doctor.AddCommand(newDoctorMoonlightCmd())
	doctor.AddCommand(newDoctorToolsCmd())
	doctor.AddCommand(newDoctorNetworkCmd())
	doctor.AddCommand(newDoctorLockCmd())
	return doctor
}

func newDoctorLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Inspect the clouddeployctl state lock (read-only)",
		Long: `Report whether /var/lib/clouddeploy/state.lock exists and whether
the recorded PID is still alive. Useful when an apply/resume run
errors with "lock is held by another clouddeployctl process".

Typical recovery flow when the lock looks stale:

    sudo clouddeployctl doctor lock          # confirm holder is dead
    sudo ps -fp <pid>                        # paranoia check
    sudo journalctl -u clouddeploy-v3-continue.service -n 200 --no-pager
    sudo rm /var/lib/clouddeploy/state.lock  # only if holder is dead
    sudo clouddeployctl resume               # retry`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("lock-path")
			info := state.Inspect(path)
			fmt.Println("doctor lock:")
			fmt.Printf("  path                 : %s\n", info.Path)
			fmt.Printf("  exists               : %v\n", info.Exists)
			if !info.Exists {
				fmt.Println("  (no holder; clouddeployctl can acquire freely)")
				return nil
			}
			if info.ReadErr != nil {
				fmt.Printf("  pid                  : (could not read: %v)\n", info.ReadErr)
				fmt.Println("  recovery             : sudo rm", info.Path)
				return nil
			}
			fmt.Printf("  pid                  : %d\n", info.PID)
			fmt.Printf("  holder process alive : %v\n", info.HolderLive)
			if info.HolderLive {
				fmt.Printf("  recovery             : another clouddeployctl run is active. Inspect with:\n")
				fmt.Printf("                           sudo ps -fp %d\n", info.PID)
				fmt.Printf("                           sudo journalctl -u clouddeploy-v3-continue.service -n 200 --no-pager\n")
			} else {
				fmt.Printf("  recovery             : holder is dead. Stale lock:\n")
				fmt.Printf("                           sudo rm %s\n", info.Path)
				fmt.Printf("                           sudo clouddeployctl resume\n")
			}
			return nil
		},
	}
}

func newDoctorAptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apt",
		Short: "Report dpkg / apt state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			st, _ := apt.InspectPolicyRcD(deps.APT.Env)
			holders := deps.APT.LockHolders(ctx)
			fmt.Println("doctor apt:")
			fmt.Printf("  policy-rc.d path        : %s\n", st.Path)
			fmt.Printf("  policy-rc.d exists      : %v\n", st.Exists)
			fmt.Printf("  CloudDeploy-owned       : %v\n", st.IsClouddeploy)
			fmt.Printf("  backup file present     : %v\n", st.BackupExists)
			if len(holders) == 0 {
				fmt.Printf("  Lock holders            : (none)\n")
			} else {
				fmt.Printf("  Lock holders            :\n")
				for _, h := range holders {
					fmt.Printf("    %s\n", h)
				}
			}
			// dpkg --audit via a quick runner call (read-only). On a
			// developer host without dpkg this just prints "(missing)".
			if _, err := lookExecutable("dpkg"); err == nil {
				res := deps.Runner.Exec(ctx, runner.CommandSpec{
					Argv:    []string{"dpkg", "--audit"},
					LogFile: "-",
				})
				out := res.Stdout
				if out == "" {
					fmt.Printf("  dpkg --audit            : (no findings)\n")
				} else {
					fmt.Printf("  dpkg --audit            :\n%s\n", indent(out, "    "))
				}
			} else {
				fmt.Printf("  dpkg --audit            : (dpkg not on PATH)\n")
			}
			return nil
		},
	}
}

func newDoctorNvidiaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "nvidia",
		Short: "Report NVIDIA driver readiness (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}

			// Driver major: flag overrides profile.
			driverMajor, _ := cmd.Flags().GetString("driver-major")
			if driverMajor == "" && deps.Profile != nil {
				driverMajor = deps.Profile.NVIDIA.DriverMajor
			}
			opts := nvidia.EvidenceOptions{DriverMajor: driverMajor}
			ev, err := nvidia.GatherEvidenceFromHost(opts)
			if err != nil {
				fmt.Printf("doctor nvidia: could not gather evidence: %v\n", err)
				return nil
			}
			if deps.Profile != nil {
				ev.PreferOpenFamily = ev.PreferOpenFamily || deps.Profile.NVIDIA.PreferOpenFamily
				ev.PreferServerFamily = ev.PreferServerFamily || deps.Profile.NVIDIA.PreferServer
			}
			cls := nvidia.Classify(ev.GPUName, ev.PCIID)
			fam, reason, selErr := nvidia.SelectFamily(ev)
			scanned := nvidia.ScannedMajors(opts)

			fmt.Println("doctor nvidia:")
			fmt.Printf("  Detected GPU            : %s\n", evOrUnknown(ev.GPUName))
			fmt.Printf("  PCI ID                  : %s\n", evOrUnknown(ev.PCIID))
			fmt.Printf("  Classification          : %s / %s\n", cls.Category, cls.Kind)
			fmt.Printf("  Blackwell (consumer)    : %v\n", ev.IsBlackwellConsumer)
			fmt.Printf("  Blackwell (workstation) : %v\n", ev.IsBlackwellPro)
			fmt.Printf("  Blackwell (datacenter)  : %v\n", ev.IsBlackwellDC)
			fmt.Printf("  Data-center             : %v\n", ev.IsDataCenter)
			fmt.Printf("  Pascal legacy           : %v\n", ev.IsLegacyPascal)
			fmt.Printf("  dmesg open required     : %v\n", ev.DmesgRequiresOpenKernelModule)
			fmt.Printf("  Installed server        : %v\n", ev.InstalledServer)
			fmt.Printf("  Installed server-open   : %v\n", ev.InstalledServerOpen)
			fmt.Printf("  Installed non-server    : %v\n", ev.InstalledNonServer)
			fmt.Printf("  Installed non-server-open: %v\n", ev.InstalledNonServerOpen)
			fmt.Printf("  Driver majors scanned   : %v\n", scanned)
			fmt.Printf("  Availability known      : %v\n", ev.AvailabilityKnown)
			fmt.Printf("  Available server        : %v\n", ev.AvailableServer)
			fmt.Printf("  Available server-open   : %v\n", ev.AvailableServerOpen)
			fmt.Printf("  Available non-server    : %v\n", ev.AvailableNonServer)
			fmt.Printf("  Available non-server-open: %v\n", ev.AvailableNonServerOpen)
			fmt.Printf("  nvidia-smi works        : %v\n", ev.NvidiaSmiWorks)
			fmt.Printf("  AV1 encode supported    : %v\n", cls.Kind.SupportsAV1Encode())
			fmt.Printf("  HDR streaming supported : %v\n", cls.Kind.SupportsHDRStreaming())
			fmt.Printf("  Selected family         : %s\n", fam)
			if fam != nvidia.FamilyUnknown {
				major := driverMajor
				if major == "" {
					major = "580"
				}
				fmt.Printf("  Suggested driver pkg    : %s\n", fam.DriverPackage(major))
				fmt.Printf("  Suggested dkms pkg      : %s\n", fam.DkmsPackage(major))
				fmt.Printf("  Open module required    : %v\n", ev.IsBlackwellConsumer || ev.IsBlackwellPro || ev.IsBlackwellDC || ev.DmesgRequiresOpenKernelModule)
			}
			fmt.Printf("  Reason                  : %s\n", reason)
			if selErr != nil {
				fmt.Printf("  Error                   : %v\n", selErr)
			}
			if !ev.AvailabilityKnown {
				fmt.Println("  Note                    : apt-cache was not consulted; availability is approximate.")
			}
			if driverMajor == "" {
				fmt.Printf("  Note                    : no --driver-major and no profile.nvidia.driver_major; scanned %v as a best-effort guess.\n", scanned)
			}

			// Suggested action.
			fmt.Println()
			fmt.Println("Suggested action:")
			switch {
			case ev.AlreadyInstalled() == fam && ev.NvidiaSmiWorks:
				fmt.Printf("  Keep installed family %q. No action needed.\n", fam)
			case selErr != nil:
				fmt.Printf("  Fix: %v.\n", selErr)
			default:
				fmt.Printf("  Run: clouddeployctl phase nvidia-driver --profile %s\n", profileName(cmd))
			}
			return nil
		},
	}
	c.Flags().String("driver-major", "", "NVIDIA driver major version to scan (e.g. 580); falls back to profile.nvidia.driver_major")
	return c
}

func newDoctorCudaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cuda",
		Short: "Report CUDA toolkit readiness (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			mode := cuda.ModeNone
			method := cuda.MethodAuto
			policy := cuda.PolicyLatestCompatible
			packageName := ""
			runfileURL := ""
			runfileSHA := ""
			runfileMaxAttempts := 0
			driverMajor := profileDriverMajor(deps)
			selection := cuda.Selection{DriverPreferredMajor: cuda.PreferredMajorForDriver(driverMajor)}
			compileSmokeTest := false
			if deps.Profile != nil {
				if m, perr := cuda.ParseMode(deps.Profile.CUDA.Mode); perr == nil {
					mode = m
				}
				if mt, perr := cuda.ParseMethod(deps.Profile.CUDA.Method); perr == nil {
					method = mt
				}
				if pol, perr := cuda.ParseSelectionPolicy(deps.Profile.CUDA.SelectionPolicy); perr == nil {
					policy = pol
				}
				packageName = deps.Profile.CUDA.PackageName
				runfileURL = deps.Profile.CUDA.RunfileURL
				runfileSHA = deps.Profile.CUDA.RunfileSHA256
				runfileMaxAttempts = deps.Profile.CUDA.RunfileMaxAttempts
				selection.Policy = policy
				selection.ExpectedMajor = deps.Profile.CUDA.ExpectedMajor
				selection.MinMajor = deps.Profile.CUDA.MinMajor
				selection.AllowUbuntuArchiveFallback = deps.Profile.CUDA.AllowUbuntuArchiveFallback
				compileSmokeTest = deps.Profile.CUDA.CompileSmokeTest
			}
			plan := cuda.Plan(mode)

			// Probe the host for nvcc.
			_, nvccErr := lookExecutable("nvcc")
			nvccPresent := nvccErr == nil

			fmt.Println("doctor cuda:")
			if deps.Profile != nil {
				fmt.Printf("  Profile                       : %s\n", deps.Profile.Profile)
			} else {
				fmt.Printf("  Profile                       : (no profile loaded)\n")
			}
			fmt.Printf("  cuda.mode                     : %s\n", plan.Mode)
			fmt.Printf("  cuda.method                   : %s\n", method)
			fmt.Printf("  cuda.selection_policy         : %s\n", policy)
			fmt.Printf("  cuda.expected_major           : %s\n", evOrUnknown(selection.ExpectedMajor))
			fmt.Printf("  cuda.min_major                : %s\n", evOrUnknown(selection.MinMajor))
			fmt.Printf("  cuda.allow_ubuntu_archive_fallback: %v\n", selection.AllowUbuntuArchiveFallback)
			fmt.Printf("  cuda.compile_smoke_test       : %v\n", compileSmokeTest)
			if deps.Profile != nil {
				fmt.Printf("  cuda.prefer_major             : %s\n", evOrUnknown(deps.Profile.CUDA.PreferMajor))
				fmt.Printf("  cuda.prefer_newest            : %v\n", deps.Profile.CUDA.PreferNewest)
				fmt.Printf("  cuda.allow_cross_distro_cuda_repo: %v\n", deps.Profile.CUDA.AllowCrossDistroCudaRepo)
				cands := deps.Profile.CUDA.CudaRepoDistroCandidates
				if len(cands) == 0 {
					cands = []string{"auto-host"}
				}
				fmt.Printf("  cuda.cuda_repo_distro_candidates: %v\n", cands)
			}
			fmt.Printf("  nvidia.driver_major           : %s\n", evOrUnknown(driverMajor))
			fmt.Printf("  driver-preferred CUDA major   : %s\n", evOrUnknown(selection.DriverPreferredMajor))
			fmt.Printf("  Will attempt install          : %v\n", plan.WillAttemptInstall && method != cuda.MethodNone)
			fmt.Printf("  Fails deploy on error         : %v\n", plan.FailsDeployOnError)
			fmt.Printf("  cuda.package_name             : %s\n", evOrUnknown(packageName))
			fmt.Printf("  cuda.runfile_url              : %s\n", evOrUnknown(runfileURL))
			fmt.Printf("  cuda.runfile_sha256           : %s\n", evOrUnknown(runfileSHA))
			fmt.Printf("  cuda.runfile_max_attempts     : %d (effective: %d)\n",
				runfileMaxAttempts,
				cuda.ResolveRunfileMaxAttempts(runfileMaxAttempts, os.Getenv("CLOUDDEPLOY_CUDA_RUNFILE_MAX_ATTEMPTS")))
			fmt.Printf("  nvcc on PATH                  : %v\n", nvccPresent)
			fmt.Println()
			fmt.Println("Apt candidate ladder (policy-filtered):")
			for _, c := range cuda.CandidateLadder(selection.CandidateOptions(packageName)) {
				fmt.Printf("  - %s\n", c)
			}
			fmt.Println()
			fmt.Println("Rationale:")
			fmt.Printf("  %s\n", plan.Rationale)
			fmt.Println()
			fmt.Println("Selection policy semantics:")
			fmt.Println("  - latest-compatible : pick newest installable; honor min_major as soft floor.")
			fmt.Println("  - exact-major       : nvcc reported major must equal expected_major.")
			fmt.Println("  - min-major         : nvcc reported major must be >= min_major.")
			fmt.Println("  - any               : any parseable nvcc satisfies (broad-compat).")
			fmt.Println("Runfile retry policy:")
			fmt.Println("  - same (sha256, size) that already failed --check => refuse to redownload.")
			fmt.Println("  - 3 consecutive --check failures => give up.")
			fmt.Println("  - mode=optional => mark phase failed_nonfatal and continue.")
			fmt.Println("  - mode=required => mark phase failed_fatal.")
			fmt.Println("  - apt is always toolkit-only; runfile uses --toolkit (never --driver).")
			return nil
		},
	}
}

// profileDriverMajor reads the NVIDIA driver major from the profile,
// or "" if no profile is loaded.
func profileDriverMajor(deps *phase.Deps) string {
	if deps == nil || deps.Profile == nil {
		return ""
	}
	return deps.Profile.NVIDIA.DriverMajor
}

func newDoctorSystemCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "system",
		Short: "Report OS / kernel / state summary (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			fmt.Println("doctor system:")
			fmt.Printf("  Kernel                  : %s\n", readSmall("/proc/sys/kernel/osrelease"))
			fmt.Printf("  OS release id           : %s\n", lsbRelease("ID"))
			fmt.Printf("  OS version              : %s\n", lsbRelease("VERSION_ID"))

			var targetVersion string
			if deps.Profile != nil {
				targetVersion = deps.Profile.UbuntuVersion
			}
			res, gateErr := ubuntu.ReadAndGate(ubuntu.DefaultOSReleasePath, targetVersion)
			if gateErr == nil {
				fmt.Printf("  Ubuntu Gate:\n")
				fmt.Printf("    Found                   : %s\n", res.Release.VersionID)
				fmt.Printf("    Profile Target          : %s\n", targetVersion)
				fmt.Printf("    Global Supported        : %v\n", ubuntu.SupportedVersions())
				exactMatch := targetVersion != "" && res.Release.VersionID == targetVersion
				fmt.Printf("    Exact Match Passed      : %v\n", exactMatch)
				fmt.Printf("    Gate Supported          : %v\n", res.Supported)
				if !res.Supported {
					fmt.Printf("    Gate Reason             : %s\n", res.Reason)
				}
			}

			fmt.Printf("  PID 1 / init            : %s\n", readSmall("/proc/1/comm"))
			fmt.Printf("  State file              : %s\n", deps.StatePath)
			fmt.Printf("  Profile loaded          : %v\n", deps.Profile != nil)
			fmt.Printf("  State.RebootNeeded      : %v\n", deps.State.RebootNeeded)
			fmt.Printf("  State.ResumeTarget      : %s\n", deps.State.ResumeTarget)
			for name, p := range deps.State.Phases {
				fmt.Printf("  Phase %-18s : %s\n", name, p.Status)
			}
			return nil
		},
	}
}

func newDoctorKwinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kwin",
		Short: "Report KWin Wayland session state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			fmt.Println("doctor kwin:")
			user := ""
			uid := ""
			backend := config.DefaultSessionBackend
			mode := config.DefaultCompositorMode
			vt := config.DefaultKwinVT
			drm := config.DefaultKwinDRMDevice
			if deps.Profile != nil {
				desk := deps.Profile.EffectiveDesktop()
				user = desk.User
				backend = desk.SessionBackend
				mode = desk.CompositorMode
				vt = desk.KwinVT
				drm = desk.KwinDRMDevice
			}
			unitName := phase.UnitNameForSession(backend, mode)
			unitPath := "/etc/systemd/system/" + unitName
			fmt.Printf("  Backend                : %s\n", backend)
			fmt.Printf("  Compositor mode        : %s\n", mode)
			fmt.Printf("  VT                     : %d\n", vt)
			fmt.Printf("  KWIN_DRM_DEVICES       : %s\n", drm)
			fmt.Printf("  Unit name              : %s\n", unitName)
			fmt.Printf("  Unit path              : %s\n", unitPath)
			if hu := deps.State.Get(phase.HeadlessUserName); hu != nil {
				if v, ok := hu.Details["uid"].(string); ok {
					uid = v
				}
			}
			fmt.Printf("  Headless user          : %s\n", evOrUnknown(user))
			fmt.Printf("  Recorded UID           : %s\n", evOrUnknown(uid))
			if _, err := os.Stat(unitPath); err == nil {
				fmt.Printf("  Unit installed         : yes\n")
			} else {
				fmt.Printf("  Unit installed         : no\n")
			}
			// Best-effort systemctl is-active probe.
			res := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"systemctl", "is-active", unitName},
				LogFile: "-",
				Timeout: 5 * time.Second,
			})
			active := strings.TrimSpace(res.Stdout)
			if active == "" {
				active = "(systemctl unavailable)"
			}
			fmt.Printf("  Service is-active      : %s\n", active)

			// Wayland socket check.
			if uid != "" {
				socket := "/run/user/" + uid + "/wayland-0"
				if _, err := os.Stat(socket); err == nil {
					fmt.Printf("  Wayland socket         : %s (present)\n", socket)
				} else {
					fmt.Printf("  Wayland socket         : %s (missing: %v)\n", socket, err)
				}
				fmt.Printf("  Manual kscreen command : sudo -u %s env XDG_RUNTIME_DIR=/run/user/%s WAYLAND_DISPLAY=wayland-0 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%s/bus QT_QPA_PLATFORM=wayland XDG_CURRENT_DESKTOP=KDE XDG_SESSION_TYPE=wayland kscreen-doctor -o\n", evOrUnknown(user), uid, uid)
			}

			// Quick journal tail for the unit.
			fmt.Println()
			fmt.Println("Recent journal lines (last 200):")
			jres := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"journalctl", "-u", unitName, "-n", "200", "--no-pager"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			if jres.Err != nil {
				fmt.Printf("  (journalctl unavailable: %v)\n", jres.Err)
			} else {
				fmt.Println(indent(jres.Stdout, "  "))
			}
			fmt.Println()
			fmt.Println("Recovery hints:")
			fmt.Printf("  sudo systemctl stop %s\n", unitName)
			fmt.Println("  sudo clouddeployctl state reset --phase kwin_session")
			fmt.Println("  sudo clouddeployctl phase kwin-session     # re-render + restart")
			fmt.Printf("  sudo journalctl -u %s -e\n", unitName)
			fmt.Println("  switch diagnostic fallback: set desktop.session_backend=weston and run phase kwin-session")
			return nil
		},
	}
}

func newDoctorKwinPatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kwin-patch",
		Short: "Report patched KWin private-HDR build/install state",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			cfg := (&config.Profile{}).EffectiveKWin()
			profile := ""
			if deps.Profile != nil {
				cfg = deps.Profile.EffectiveKWin()
				profile = deps.Profile.Profile
			}
			patchPath := cfg.Patch
			if !filepath.IsAbs(patchPath) {
				if _, err := os.Stat(patchPath); err != nil {
					patchPath = filepath.Join("/opt/clouddeploy-mover", cfg.Patch)
				}
			}
			fmt.Println("doctor kwin-patch:")
			fmt.Printf("  Profile                : %s\n", evOrUnknown(profile))
			fmt.Printf("  patched_hdr            : %v\n", cfg.PatchedHDR)
			fmt.Printf("  source_mode            : %s\n", cfg.SourceMode)
			fmt.Printf("  install_mode           : %s\n", cfg.InstallMode)
			fmt.Printf("  require_patch          : %v\n", cfg.RequirePatchEnabled())
			fmt.Printf("  allow_packaged_fallback: %v\n", cfg.AllowPackagedFallback)
			fmt.Printf("  patch path             : %s\n", patchPath)
			if _, err := os.Stat(patchPath); err == nil {
				fmt.Println("  patch exists           : yes")
				if sum, herr := kwinpkg.HashFile(patchPath); herr == nil {
					fmt.Printf("  patch sha256           : %s\n", sum)
				}
			} else {
				fmt.Printf("  patch exists           : no (%v)\n", err)
			}

			marker, merr := kwinpkg.ReadMarker(kwinpkg.DefaultMarkerPath)
			if merr != nil {
				fmt.Printf("  marker                 : missing (%v)\n", merr)
			} else {
				fmt.Printf("  marker                 : %s\n", kwinpkg.DefaultMarkerPath)
				fmt.Printf("  marker patch sha256    : %s\n", marker.PatchSHA256)
				fmt.Printf("  marker source version  : %s\n", evOrUnknown(marker.KWinSourceVersion))
				fmt.Printf("  marker install mode    : %s\n", marker.InstallMode)
				fmt.Printf("  marker runtime bin     : %s\n", evOrUnknown(marker.RuntimeBin))
				fmt.Printf("  installed packages     : %v\n", marker.InstalledPackages)
				if marker.FallbackReason != "" {
					fmt.Printf("  fallback reason        : %s\n", marker.FallbackReason)
				}
				holds := deps.Runner.Exec(ctx, runner.CommandSpec{
					Argv:    []string{"apt-mark", "showhold"},
					LogFile: "-",
					Timeout: 10 * time.Second,
				})
				if holds.Err == nil {
					fmt.Printf("  apt holds              : %v\n", heldPackages(marker.InstalledPackages, holds.Stdout))
				}
			}
			if ph := deps.State.Get(phase.KWinPatchName); ph != nil {
				fmt.Printf("  state status           : %s\n", ph.Status)
				if ph.Reason != "" {
					fmt.Printf("  state reason           : %s\n", ph.Reason)
				}
				if ph.Details != nil {
					if fallback, ok := ph.Details["fallback_reason"].(string); ok && fallback != "" {
						fmt.Printf("  state fallback reason  : %s\n", fallback)
					}
					for _, k := range []string{"deb_src_enabled_before", "deb_src_modified", "deb_src_backup_path", "apt_update_after_deb_src"} {
						if v, ok := ph.Details[k]; ok {
							fmt.Printf("  %-23s: %v\n", k, v)
						}
					}
				}
			}
			fmt.Println()
			fmt.Println("Recommended next command:")
			fmt.Println("  sudo clouddeployctl phase kwin-patch")
			return nil
		},
	}
}

func newDoctorDRMDisplayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "drm-display",
		Short: "Report KScreen/DRM display validation evidence (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			fmt.Println("doctor drm-display:")
			if deps.Profile != nil {
				fmt.Printf("  forced connector       : %s\n", evOrUnknown(deps.Profile.Display.ForcedConnector))
				w, h := phase.ParseResolution(deps.Profile.Display.Resolution)
				fmt.Printf("  expected mode          : %dx%d@%d\n", w, h, deps.Profile.Display.Refresh)
				fmt.Printf("  hdr requested          : %v\n", deps.Profile.Display.HDR)
			}
			ph := deps.State.Get(phase.DRMDisplayValidateName)
			if ph == nil {
				fmt.Println("  state                  : (phase missing)")
				return nil
			}
			fmt.Printf("  state status           : %s\n", ph.Status)
			if ph.Reason != "" {
				fmt.Printf("  state reason           : %s\n", ph.Reason)
			}
			for _, k := range []string{
				"enabled", "selected_mode", "hdr_enabled", "wcg_enabled",
				"kscreen_connector_names", "kscreen_raw_mentions_connector",
				"kscreen_raw_excerpt", "kscreen_stripped_excerpt",
			} {
				if v, ok := ph.Details[k]; ok {
					fmt.Printf("  %-24s: %v\n", k, v)
				}
			}
			if ph.LastError != "" {
				fmt.Printf("  last_error             : %s\n", ph.LastError)
			}
			fmt.Println("  manual command         : sudo -u <user> env XDG_RUNTIME_DIR=/run/user/<uid> WAYLAND_DISPLAY=wayland-0 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/<uid>/bus QT_QPA_PLATFORM=wayland NO_COLOR=1 TERM=dumb CLICOLOR=0 kscreen-doctor -o")
			return nil
		},
	}
}

func heldPackages(installed []string, holdOutput string) []string {
	want := map[string]bool{}
	for _, pkg := range installed {
		pkg = strings.TrimSpace(pkg)
		if pkg != "" {
			want[pkg] = true
		}
	}
	var out []string
	for _, line := range strings.Split(holdOutput, "\n") {
		pkg := strings.TrimSpace(line)
		if want[pkg] {
			out = append(out, pkg)
		}
	}
	sort.Strings(out)
	return out
}

func newDoctorSunshineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sunshine",
		Short: "Report Sunshine fork build/install state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			cfg := config.SunshineConfig{}
			if deps.Profile != nil {
				cfg = deps.Profile.EffectiveSunshine()
			} else {
				cfg = (&config.Profile{}).EffectiveSunshine()
			}
			fmt.Println("doctor sunshine:")
			fmt.Printf("  source                 : %s\n", cfg.Source)
			fmt.Printf("  fork repo              : %s\n", evOrUnknown(cfg.ForkRepo))
			fmt.Printf("  fork branch            : %s\n", evOrUnknown(cfg.ForkBranch))
			fmt.Printf("  fork commit            : %s\n", evOrUnknown(cfg.ForkCommit))
			fmt.Printf("  build dir              : %s\n", cfg.BuildDir)
			fmt.Printf("  install bin            : %s\n", cfg.InstallBin)
			confPath := cfg.ConfigPath
			if confPath == "" && deps.Profile != nil {
				desk := deps.Profile.EffectiveDesktop()
				confPath = "/home/" + desk.User + "/.config/sunshine/sunshine.conf"
			}
			fmt.Printf("  config path            : %s\n", evOrUnknown(confPath))
			fmt.Printf("  install bin exists     : %v\n", fileExists(cfg.InstallBin))
			fmt.Printf("  /usr/local/bin/sunshine: %v\n", fileExists("/usr/local/bin/sunshine"))
			fmt.Printf("  assets apps.json       : %v\n", fileExists("/usr/local/assets/apps.json"))
			fmt.Printf("  assets web/index.html  : %v\n", fileExists("/usr/local/assets/web/index.html"))
			fmt.Printf("  config exists          : %v\n", fileExists(confPath))
			if confPath != "" {
				credsDir := filepath.Join(filepath.Dir(confPath), "credentials")
				fmt.Printf("  credentials dir exists : %v\n", fileExists(credsDir))
			}
			user := "cloudgamer"
			if deps.Profile != nil {
				user = deps.Profile.EffectiveDesktop().User
			}
			fmt.Printf("  /dev/uinput exists     : %v\n", fileExists("/dev/uinput"))
			uinput := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"runuser", "-u", user, "--", "test", "-w", "/dev/uinput"},
				LogFile: "-",
				Timeout: 5 * time.Second,
			})
			fmt.Printf("  /dev/uinput writable   : %v\n", uinput.Err == nil)
			drmDevice := "/dev/dri/card1"
			if ph := deps.State.Get(phase.KWinSessionName); ph != nil && ph.Details != nil {
				if v, ok := ph.Details["selected_drm_device"].(string); ok && strings.TrimSpace(v) != "" {
					drmDevice = strings.TrimSpace(v)
				} else if v, ok := ph.Details["kwin_drm_device_resolved"].(string); ok && strings.TrimSpace(v) != "" {
					drmDevice = strings.TrimSpace(v)
				}
			}
			renderNode := doctorRenderNodeForCard(drmDevice)
			fmt.Printf("  selected DRM device    : %s\n", drmDevice)
			if renderNode != "" {
				fmt.Printf("  expected render node   : %s\n", renderNode)
			}
			id := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: []string{"id", user}, LogFile: "-", Timeout: 5 * time.Second})
			fmt.Printf("  user groups            : %s\n", strings.TrimSpace(id.Stdout))
			for _, path := range []string{drmDevice, renderNode, "/dev/uinput", "/dev/nvidiactl", "/dev/nvidia0", "/dev/nvidia-uvm"} {
				if strings.TrimSpace(path) == "" {
					continue
				}
				ls := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: []string{"ls", "-l", path}, LogFile: "-", Timeout: 5 * time.Second})
				if ls.Err == nil {
					fmt.Printf("  %-22s: %s\n", path, strings.TrimSpace(ls.Stdout))
				} else {
					fmt.Printf("  %-22s: missing/unreadable (%v)\n", path, ls.Err)
				}
			}
			for _, path := range []string{drmDevice, renderNode, "/dev/uinput"} {
				if strings.TrimSpace(path) == "" {
					continue
				}
				acl := deps.Runner.Exec(ctx, runner.CommandSpec{Argv: []string{"getfacl", "-p", path}, LogFile: "-", Timeout: 5 * time.Second})
				if acl.Err == nil {
					fmt.Printf("  getfacl %-14s: %s\n", path, oneLine(strings.TrimSpace(acl.Stdout)))
				}
			}
			if fileExists(cfg.InstallBin) {
				res := deps.Runner.Exec(ctx, runner.CommandSpec{
					Argv:    []string{"getcap", cfg.InstallBin},
					LogFile: "-",
					Timeout: 10 * time.Second,
				})
				fmt.Printf("  capabilities           : %s\n", strings.TrimSpace(res.Stdout))
			}
			if fileExists(filepath.Join(cfg.BuildDir, ".git")) {
				head := deps.Runner.Exec(ctx, runner.CommandSpec{
					Argv:    []string{"git", "-C", cfg.BuildDir, "rev-parse", "HEAD"},
					LogFile: "-",
					Timeout: 10 * time.Second,
				})
				fmt.Printf("  build dir HEAD         : %s\n", strings.TrimSpace(head.Stdout))
			}
			unit := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"systemctl", "cat", "sunshine-headless.service"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			show := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"systemctl", "show", "sunshine-headless.service", "-p", "DevicePolicy", "-p", "DeviceAllow", "-p", "AmbientCapabilities", "-p", "CapabilityBoundingSet", "-p", "NoNewPrivileges"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			if show.Err == nil {
				fmt.Printf("  service restrictions   : %s\n", oneLine(strings.TrimSpace(show.Stdout)))
			}
			if unit.Err == nil {
				fmt.Printf("  service has force HDR  : %v\n", strings.Contains(unit.Stdout, "SUNSHINE_FORCE_AV1_HDR10=1"))
				fmt.Printf("  service uses kwin-realvt: %v\n", strings.Contains(unit.Stdout, "kwin-realvt.service"))
				fmt.Printf("  service has CAP_SYS_ADMIN: %v\n", strings.Contains(unit.Stdout, "AmbientCapabilities=CAP_SYS_ADMIN"))
				fmt.Printf("  service standalone DeviceAllow: %v\n", strings.Contains(unit.Stdout, "DeviceAllow="))
			} else {
				fmt.Println("  service                : sunshine-headless.service not installed")
			}
			fmt.Printf("  localhost serverinfo   : %s\n", curlHTTPStatus(ctx, deps, "http://127.0.0.1:47989/serverinfo"))
			fmt.Printf("  localhost Web UI       : %s\n", curlHTTPStatus(ctx, deps, "https://127.0.0.1:47990"))
			journal := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"journalctl", "-u", "sunshine-headless.service", "-n", "300", "--no-pager"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			if journal.Err == nil {
				for _, marker := range []string{
					"STREAM_DIAG kms capture selected",
					"Found monitor for DRM screencasting",
					"hevc_nvenc",
					"Color coding: HDR",
					"selected_pix_fmt=p010",
				} {
					fmt.Printf("  log marker %-36s: %v\n", marker, strings.Contains(journal.Stdout, marker))
				}
			}
			return nil
		},
	}
}

func newDoctorSunshineWebCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sunshine-web",
		Short: "Report Sunshine Web UI asset/rendering state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			root := "/usr/local/assets/web"
			index := filepath.Join(root, "index.html")
			js, css := countWebAssets(root)
			raw := doctorRawWebMarkers(root)
			fmt.Println("doctor sunshine-web:")
			fmt.Printf("  runtime asset root     : %s\n", root)
			fmt.Printf("  index path             : %s\n", index)
			fmt.Printf("  index exists           : %v\n", fileExists(index))
			fmt.Printf("  JS asset count         : %d\n", js)
			fmt.Printf("  CSS asset count        : %d\n", css)
			fmt.Printf("  raw template markers   : %v\n", raw)
			fmt.Printf("  localhost Web UI       : %s\n", curlHTTPStatus(ctx, deps, "https://127.0.0.1:47990"))
			fmt.Printf("  localhost /welcome     : %s\n", curlHTTPStatus(ctx, deps, "https://127.0.0.1:47990/welcome"))
			if raw {
				fmt.Println("  diagnosis              : runtime web assets still look like raw Vue/template source; pairing helper remains CLI/API-safe.")
			}
			return nil
		},
	}
}

func newDoctorStreamSessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stream-session",
		Short: "Report latest Sunshine/Moonlight session negotiation markers (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			journal := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"journalctl", "-u", "sunshine-headless.service", "-n", "800", "--no-pager"},
				LogFile: "-",
				Timeout: 15 * time.Second,
			})
			logs := journal.Stdout
			fmt.Println("doctor stream-session:")
			for _, label := range []string{
				"STREAM_DIAG kms capture selected",
				"Encode selection:",
				"h264_nvenc: dynamic range not supported",
				"hevc_nvenc initialized successfully",
				"Color coding: HDR",
				"Color depth: 10-bit",
				"selected_pix_fmt=p010",
			} {
				fmt.Printf("  %-42s: %s\n", label, lastMatchingLine(logs, label))
			}
			if strings.Contains(logs, "codec=H.264") && (strings.Contains(logs, "selected_colorspace=HDR") || strings.Contains(logs, "selected_pix_fmt=p010")) {
				fmt.Println("  invalid combination    : H.264 selected with HDR/10-bit/p010; this must fail validation.")
			}
			return nil
		},
	}
}

func newDoctorCodecsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "codecs",
		Short: "Report GPU/NVENC codec and Sunshine advertisement clues (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			cfg := config.SunshineConfig{}
			if deps.Profile != nil {
				cfg = deps.Profile.EffectiveSunshine()
			} else {
				cfg = (&config.Profile{}).EffectiveSunshine()
			}
			fmt.Println("doctor codecs:")
			fmt.Printf("  config hevc_mode       : %d\n", cfg.HevcModeValue())
			fmt.Printf("  config av1_mode        : %d\n", cfg.Av1ModeValue())
			fmt.Printf("  config force HDR env   : %v\n", cfg.ForceAV1HDR10)
			gpu := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			fmt.Printf("  GPU / driver           : %s\n", strings.TrimSpace(gpu.Stdout))
			serverInfo := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"curl", "-fsS", "--max-time", "5", "http://127.0.0.1:47989/serverinfo"},
				LogFile: "-",
				Timeout: 10 * time.Second,
			})
			fmt.Printf("  serverinfo reachable   : %v\n", serverInfo.Err == nil)
			codecRaw, codecOK := sunshinecodec.ServerInfoInt(serverInfo.Stdout, "ServerCodecModeSupport")
			codecSupport := sunshinecodec.DecodeCodecModeSupport(codecRaw)
			maxLuma, maxLumaOK := sunshinecodec.ServerInfoInt(serverInfo.Stdout, "MaxLumaPixelsHEVC")
			codecValue := sunshinecodec.ServerInfoValue(serverInfo.Stdout, "ServerCodecModeSupport")
			if codecValue == "" {
				codecValue = "(missing)"
			}
			fmt.Printf("  ServerCodecModeSupport : %s\n", codecValue)
			if codecOK {
				fmt.Printf("  decoded codec flags    : %s\n", codecSupport.String())
				fmt.Printf("  advertises HEVC Main10 : %v\n", codecSupport.HEVCMain10)
				fmt.Printf("  advertises AV1 Main10  : %v\n", codecSupport.AV1Main10)
			}
			if maxLumaOK {
				fmt.Printf("  MaxLumaPixelsHEVC      : %d\n", maxLuma)
			} else {
				fmt.Println("  MaxLumaPixelsHEVC      : (missing)")
			}
			journal := deps.Runner.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"journalctl", "-u", "sunshine-headless.service", "-n", "800", "--no-pager"},
				LogFile: "-",
				Timeout: 15 * time.Second,
			})
			logs := journal.Stdout
			fmt.Printf("  H.264 NVENC seen       : %v\n", strings.Contains(logs, "h264_nvenc"))
			fmt.Printf("  HEVC NVENC seen        : %v\n", strings.Contains(logs, "hevc_nvenc"))
			fmt.Printf("  AV1 NVENC seen         : %v\n", strings.Contains(logs, "av1_nvenc"))
			fmt.Printf("  AV1 unsupported marker : %v\n", strings.Contains(logs, "does not support AV1"))
			fmt.Printf("  active_hevc_mode line  : %s\n", lastMatchingLine(logs, "active_hevc_mode"))
			fmt.Printf("  active_av1_mode line   : %s\n", lastMatchingLine(logs, "active_av1_mode"))
			fmt.Printf("  HEVC HDR Main10 marker : %v\n", strings.Contains(logs, "Color coding: HDR") && strings.Contains(logs, "Color depth: 10-bit"))
			return nil
		},
	}
}

func newDoctorMoonlightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "moonlight",
		Short: "Print Moonlight-facing URLs and stream diagnostic hints",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			ip := ""
			if ph := deps.State.Get(phase.TailscaleName); ph != nil && ph.Details != nil {
				if v, ok := ph.Details["tailscale_ip"].(string); ok {
					ip = strings.TrimSpace(v)
				}
			}
			fmt.Println("doctor moonlight:")
			fmt.Printf("  local serverinfo       : %s\n", curlHTTPStatus(ctx, deps, "http://127.0.0.1:47989/serverinfo"))
			if ip != "" {
				fmt.Printf("  Tailscale serverinfo   : %s\n", curlHTTPStatus(ctx, deps, "http://"+ip+":47989/serverinfo"))
				fmt.Printf("  Pair/Web UI            : https://%s:47990\n", ip)
				fmt.Printf("  Moonlight host         : %s\n", ip)
			} else {
				fmt.Println("  Tailscale IP           : missing; use doctor network for firewall/tunnel fallback.")
			}
			fmt.Println("  target                 : HEVC Main10 HDR on Ampere/A5000/A6000; AV1 only when GPU/client support it.")
			return nil
		},
	}
}

func newDoctorToolsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tools",
		Short: "Report runtime command discovery choices (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("doctor tools:")
			resolver := discovery.Resolver{}
			report := func(label string, names []string) {
				c, err := resolver.ResolveCommand(names)
				if err != nil {
					fmt.Printf("  %-16s: missing (%v)\n", label, err)
					return
				}
				version := strings.TrimSpace(c.Version)
				if version == "" {
					version = "(version unavailable)"
				}
				fmt.Printf("  %-16s: %s [%s]\n", label, c.Path, version)
			}
			report("qdbus", []string{"qdbus6", "qdbus", "qdbus-qt5", "/usr/lib/qt6/bin/qdbus", "/usr/lib/qt5/bin/qdbus"})
			report("kscreen-doctor", []string{"kscreen-doctor"})
			report("doxygen", []string{"/usr/local/bin/doxygen", "doxygen"})
			report("nvcc", []string{"/usr/local/cuda/bin/nvcc", "nvcc"})
			report("cmake", []string{"cmake"})
			report("ninja", []string{"ninja", "ninja-build"})
			report("sunshine", []string{"/usr/local/bin/sunshine-clouddeploy", "/usr/local/bin/sunshine", "sunshine"})
			report("tailscale", []string{"tailscale"})
			return nil
		},
	}
}

func newDoctorNetworkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "network",
		Short: "Report Sunshine/Tailscale network reachability (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, _, err := loadDeps(cmd, false)
			if err != nil {
				return err
			}
			tailscaleIP := ""
			if ph := deps.State.Get(phase.TailscaleName); ph != nil && ph.Details != nil {
				if v, ok := ph.Details["tailscale_ip"].(string); ok {
					tailscaleIP = strings.TrimSpace(v)
				}
			}
			cfg := (&config.Profile{}).EffectiveSunshine()
			desk := (&config.Profile{}).EffectiveDesktop()
			if deps.Profile != nil {
				cfg = deps.Profile.EffectiveSunshine()
				desk = deps.Profile.EffectiveDesktop()
			}
			confPath := cfg.ConfigPath
			if confPath == "" {
				confPath = "/home/" + desk.User + "/.config/sunshine/sunshine.conf"
			}
			fmt.Println("doctor network:")
			fmt.Printf("  tailscale ip           : %s\n", evOrUnknown(tailscaleIP))
			fmt.Printf("  config path            : %s\n", confPath)
			if b, err := os.ReadFile(confPath); err == nil {
				fmt.Printf("  csrf_allowed_origins   : %s\n", firstLineContainingLocal(string(b), "csrf_allowed_origins"))
			} else {
				fmt.Printf("  csrf_allowed_origins   : (config unreadable: %v)\n", err)
			}
			for _, url := range []string{"http://127.0.0.1:47989/serverinfo", "https://127.0.0.1:47990"} {
				fmt.Printf("  %-32s: %s\n", url, curlHTTPStatus(ctx, deps, url))
			}
			if tailscaleIP != "" {
				for _, url := range []string{"http://" + tailscaleIP + ":47989/serverinfo", "https://" + tailscaleIP + ":47990"} {
					fmt.Printf("  %-32s: %s\n", url, curlHTTPStatus(ctx, deps, url))
				}
				fmt.Println()
				fmt.Println("Client checks:")
				fmt.Printf("  curl.exe http://%s:47989/serverinfo\n", tailscaleIP)
				fmt.Printf("  curl.exe -k https://%s:47990\n", tailscaleIP)
			} else {
				fmt.Println()
				fmt.Println("No Tailscale IP recorded. If using public networking, verify provider firewall allows Sunshine ports:")
				fmt.Println("  TCP 47984, TCP 47989, TCP 47990, and Sunshine/Moonlight UDP streaming ports.")
			}
			publicIP := strings.TrimSpace(os.Getenv("CLOUDDEPLOY_PUBLIC_IP"))
			if publicIP != "" {
				fmt.Println()
				fmt.Printf("SSH tunnel helper: ssh -L 47990:127.0.0.1:47990 -L 47989:127.0.0.1:47989 %s@%s\n", desk.User, publicIP)
			}
			return nil
		},
	}
}

func curlHTTPStatus(ctx context.Context, deps *phase.Deps, url string) string {
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"curl", "-k", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", url},
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	if res.Err != nil {
		return "error: " + res.Err.Error()
	}
	status := strings.TrimSpace(res.Stdout)
	if status == "" {
		status = "000"
	}
	return "HTTP " + status
}

func firstLineContainingLocal(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimSpace(line)
		}
	}
	return "(missing)"
}

func oneLine(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	out := strings.Join(fields, " ")
	if len(out) > 500 {
		return out[:500] + "..."
	}
	return out
}

func countWebAssets(root string) (js int, css int) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".js":
			js++
		case ".css":
			css++
		}
		return nil
	})
	return js, css
}

func doctorRawWebMarkers(root string) bool {
	raw := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || raw || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".html" && ext != ".js" && ext != ".ts" && ext != ".vue" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil && (strings.Contains(string(b), "<%- header %>") ||
			strings.Contains(string(b), "import { createApp } from 'vue'") ||
			strings.Contains(string(b), `import { createApp } from "vue"`) ||
			strings.Contains(string(b), ".vue'") ||
			strings.Contains(string(b), `.vue"`) ||
			strings.Contains(string(b), "{{ $t(")) {
			raw = true
		}
		return nil
	})
	return raw
}

func lastMatchingLine(s, needle string) string {
	out := "(missing)"
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			out = strings.TrimSpace(line)
		}
	}
	if len(out) > 220 {
		return out[:220] + "..."
	}
	return out
}

func serverInfoValue(s, key string) string {
	if strings.TrimSpace(s) == "" {
		return "(missing)"
	}
	for _, pat := range []string{"<" + key + ">", key + "=\""} {
		idx := strings.Index(s, pat)
		if idx < 0 {
			continue
		}
		rest := s[idx+len(pat):]
		if strings.HasSuffix(pat, ">") {
			if end := strings.Index(rest, "</"+key+">"); end >= 0 {
				return strings.TrimSpace(rest[:end])
			}
		} else if end := strings.Index(rest, "\""); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return "(missing)"
}

func doctorRenderNodeForCard(card string) string {
	card = strings.TrimSpace(card)
	if !strings.HasPrefix(card, "/dev/dri/card") {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimPrefix(card, "/dev/dri/card"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("/dev/dri/renderD%d", 128+n)
}

// -----------------------------------------------------------------------------
// validate
// -----------------------------------------------------------------------------

func newValidateCmd() *cobra.Command {
	v := &cobra.Command{Use: "validate [check]"}
	v.AddCommand(&cobra.Command{
		Use:   "hdr-stream",
		Short: "Confirm Sunshine emitted the HDR control packet to Moonlight",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("validate hdr-stream: use 'clouddeployctl phase stream-validate' for the v3 Sunshine substrate check.")
			fmt.Println("Deep Moonlight HDR packet validation is still journal-marker based and may require an active Moonlight stream attempt.")
			return nil
		},
	})
	return v
}

// -----------------------------------------------------------------------------
// phase
// -----------------------------------------------------------------------------

func newPhaseCmd() *cobra.Command {
	p := &cobra.Command{Use: "phase [name]"}
	p.AddCommand(newPhaseImplCmd("apt-health", phase.AptHealth{}))
	p.AddCommand(newPhaseImplCmd("ubuntu-upgrade", phase.UbuntuUpgrade{}))
	p.AddCommand(newPhaseImplCmd("base-packages", phase.BasePackages{}))
	p.AddCommand(newPhaseImplCmd("nvidia-driver", phase.NvidiaDriver{}))
	p.AddCommand(newPhaseImplCmd("cuda", phase.Cuda{}))
	p.AddCommand(newPhaseImplCmd("edid", phase.Edid{}))
	// Milestone 4A: headless KDE/KWin Wayland substrate.
	p.AddCommand(newPhaseImplCmd("headless-user", phase.HeadlessUser{}))
	p.AddCommand(newPhaseImplCmd("desktop-packages", phase.DesktopPackages{}))
	p.AddCommand(newPhaseImplCmd("desktop-runtime", phase.DesktopRuntime{}))
	p.AddCommand(newPhaseImplCmd("kwin-patch", phase.KWinPatch{}))
	p.AddCommand(newPhaseImplCmd("kwin-session", phase.KWinSession{}))
	p.AddCommand(newPhaseImplCmd("drm-display-validate", phase.DRMDisplayValidate{}))
	p.AddCommand(newPhaseImplCmd("sunshine-build", phase.SunshineBuild{}))
	p.AddCommand(newPhaseImplCmd("sunshine-config", phase.SunshineConfigPhase{}))
	p.AddCommand(newPhaseImplCmd("tailscale", phase.Tailscale{}))
	p.AddCommand(newPhaseImplCmd("pipewire-audio", phase.PipeWireAudio{}))
	p.AddCommand(newPhaseImplCmd("streaming-services", phase.StreamingServices{}))
	p.AddCommand(newPhaseImplCmd("stream-validate", phase.StreamValidate{}))
	p.AddCommand(newPhaseImplCmd("optional-apps", phase.OptionalApps{}))
	return p
}

func newPhaseImplCmd(name string, ph phase.Phase) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("Run the %s phase", name),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			deps, lock, err := loadDeps(cmd, true)
			if err != nil {
				return err
			}
			defer lock.Release()
			if err := ph.Run(ctx, deps); err != nil {
				if errors.Is(err, phase.ErrRebootRequired) {
					fmt.Println("phase requested reboot.")
					err = handleRebootRequired(ctx, deps)
					if errors.Is(err, phase.ErrRebootRequired) {
						os.Exit(2)
					}
					return err
				}
				return err
			}
			return nil
		},
	}
}

// handleRebootRequired installs the continuation service and schedules a reboot
// if configured, returning phase.ErrRebootRequired on success.
func handleRebootRequired(ctx context.Context, deps *phase.Deps) error {
	svc := &reboot.Service{
		Runner: deps.Runner,
		DryRun: deps.DryRun,
	}

	profileName := ""
	maxAutoReboots := 8
	autoReboot := deps.AutoReboot
	unattended := deps.Unattended
	configDir := deps.ConfigDir
	if deps.Profile != nil {
		profileName = deps.Profile.Profile
		if deps.Profile.Deploy.MaxAutoReboots > 0 {
			maxAutoReboots = deps.Profile.Deploy.MaxAutoReboots
		}
		if deps.Profile.Deploy.AutoReboot {
			autoReboot = true
		}
		if deps.Profile.Deploy.Unattended {
			unattended = true
			autoReboot = true
		}
	}
	if unattended {
		autoReboot = true
	}
	if deps.State != nil {
		target := deps.State.ResumeTarget
		deps.State.RecordRebootRequest(target, maxAutoReboots)
		if deps.State.RebootCount > maxAutoReboots {
			return fmt.Errorf("auto-reboot loop protection: reboot_count=%d exceeds max_auto_reboots=%d (last phase=%s)",
				deps.State.RebootCount, maxAutoReboots, deps.State.LastRebootPhase)
		}
		if deps.State.SamePhaseRebootCount > 2 {
			return fmt.Errorf("auto-reboot loop protection: phase %s requested reboot %d times in a row",
				deps.State.LastRebootPhase, deps.State.SamePhaseRebootCount)
		}
		if err := deps.PersistState(); err != nil {
			return fmt.Errorf("persist reboot counters: %w", err)
		}
	}
	args := reboot.Args{
		Profile:    profileName,
		StatePath:  deps.StatePath,
		ConfigDir:  configDir,
		AutoReboot: autoReboot,
		Unattended: unattended,
	}

	fmt.Println("Installing continuation service...")
	if err := svc.Install(ctx, args); err != nil {
		return fmt.Errorf("install continuation unit: %w", err)
	}

	if autoReboot {
		fmt.Println("auto-reboot enabled: Scheduling reboot now...")
		if err := svc.Reboot(ctx); err != nil {
			return fmt.Errorf("auto-reboot failed: %w", err)
		}
		return phase.ErrRebootRequired
	}

	fmt.Println("(deploy.auto_reboot is false or no profile loaded.)")
	fmt.Println("Please run `sudo reboot` manually, then `sudo clouddeployctl resume`.")
	return phase.ErrRebootRequired
}

// -----------------------------------------------------------------------------
// state
// -----------------------------------------------------------------------------

func newStateCmd() *cobra.Command {
	s := &cobra.Command{Use: "state"}
	s.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print state.json with formatting",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("state-path")
			st, err := state.Load(path)
			if err != nil {
				fmt.Printf("state show: cannot read %s: %v\n", path, err)
				return nil
			}
			return st.WriteIndented(os.Stdout)
		},
	})
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Force a phase back to pending",
		RunE: func(cmd *cobra.Command, args []string) error {
			phaseName, _ := cmd.Flags().GetString("phase")
			if phaseName == "" {
				return fmt.Errorf("state reset: --phase is required")
			}
			path, _ := cmd.Flags().GetString("state-path")
			st, err := state.Load(path)
			if err != nil {
				return fmt.Errorf("load state: %w", err)
			}
			if !st.Reset(phaseName) {
				return fmt.Errorf("state reset: phase %q not found in state", phaseName)
			}
			if err := st.Save(path); err != nil {
				return fmt.Errorf("save state: %w", err)
			}
			fmt.Printf("state reset: phase %q cleared\n", phaseName)
			return nil
		},
	}
	reset.Flags().String("phase", "", "phase name to reset (e.g. nvidia_driver)")
	s.AddCommand(reset)
	return s
}

func newMonitorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "monitor",
		Short: "Print live CloudDeploy status, lock, process and continuation-log tail",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			statePath, _ := cmd.Flags().GetString("state-path")
			lockPath, _ := cmd.Flags().GetString("lock-path")
			fmt.Printf("clouddeploy monitor @ %s\n\n", time.Now().Format(time.RFC3339))
			if st, err := state.Load(statePath); err == nil {
				fmt.Printf("profile=%s reboot_needed=%v resume_target=%s reboot_count=%d same_phase_reboots=%d\n",
					st.Profile, st.RebootNeeded, evOrUnknown(st.ResumeTarget), st.RebootCount, st.SamePhaseRebootCount)
				names := make([]string, 0, len(st.Phases))
				for name := range st.Phases {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					ph := st.Phases[name]
					fmt.Printf("  %-24s %-24s %s\n", name, ph.Status, ph.Reason)
				}
			} else {
				fmt.Printf("state: cannot read %s: %v\n", statePath, err)
			}
			fmt.Println()
			info := state.Inspect(lockPath)
			fmt.Printf("lock: exists=%v pid=%d alive=%v path=%s\n\n", info.Exists, info.PID, info.HolderLive, info.Path)
			r := runner.New()
			for _, item := range []struct {
				title string
				argv  []string
			}{
				{"active deploy processes", []string{"bash", "-lc", "ps -eo pid,ppid,etime,args | grep -E 'clouddeployctl|apt-get|dpkg|cmake|ninja' | grep -v grep || true"}},
				{"continue.log tail", []string{"bash", "-lc", "tail -n 80 /var/log/clouddeploy/continue.log 2>/dev/null || true"}},
				{"apt/dpkg tail", []string{"bash", "-lc", "tail -n 80 /var/log/apt/term.log 2>/dev/null || true; tail -n 80 /var/log/dpkg.log 2>/dev/null || true"}},
			} {
				fmt.Printf("=== %s ===\n", item.title)
				res := r.Exec(ctx, runner.CommandSpec{Argv: item.argv, LogFile: "-", Timeout: 15 * time.Second})
				fmt.Println(strings.TrimRight(res.Stdout, "\n"))
			}
			return nil
		},
	}
}

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

func evOrUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func profileName(cmd *cobra.Command) string {
	p, _ := cmd.Flags().GetString("profile")
	if p == "" {
		return "hdr-4k120"
	}
	return p
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func lookExecutable(name string) (string, error) {
	if path, err := os.Executable(); err == nil && filepathBase(path) == name {
		return path, nil
	}
	// Standard PATH lookup via the runner's exec package.
	return execLookPath(name)
}

func indent(s, prefix string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += prefix + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
func newCollectLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "collect-logs",
		Short: "Collect logs and state for debugging",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			r := runner.New()
			r.PrintToStdout = false

			archive := fmt.Sprintf("/tmp/clouddeploy-logs-%d.tar.gz", time.Now().Unix())
			fmt.Printf("Collecting logs into %s...\n", archive)

			script := `set -e
mkdir -p /tmp/cdlogs
if [ ! -f /var/lib/clouddeploy/state.json ]; then echo "Warning: state.json missing"; fi
if [ ! -d /var/log/clouddeploy ]; then echo "Warning: /var/log/clouddeploy missing"; fi
cp -a /var/log/clouddeploy /tmp/cdlogs/ 2>/dev/null || true
cp -a /var/lib/clouddeploy/state.json /tmp/cdlogs/ 2>/dev/null || true
journalctl -n 5000 > /tmp/cdlogs/journal.log || true
systemctl status clouddeploy*.service > /tmp/cdlogs/systemctl.log || true
dmesg | grep -i 'nv\|drm' > /tmp/cdlogs/dmesg-nvidia.log || true
tail -n 2000 /var/log/dpkg.log > /tmp/cdlogs/dpkg.log || true
tail -n 2000 /var/log/apt/term.log > /tmp/cdlogs/apt-term.log || true
clouddeployctl state show > /tmp/cdlogs/state-show.txt 2>/dev/null || true
find /tmp/cdlogs -type f -exec sed -i -E 's/pass(word)?=[^ &]+/pass=REDACTED/gi' {} + || true
tar -czf ` + archive + ` -C /tmp cdlogs
rm -rf /tmp/cdlogs`
			res := r.Exec(ctx, runner.CommandSpec{
				Argv:    []string{"bash", "-c", script},
				Sudo:    true,
				LogFile: "-",
			})
			if res.ExitCode != 0 {
				return fmt.Errorf("collect-logs failed: %v", res.Stderr)
			}
			fmt.Println("Done.")
			return nil
		},
	}
}
