// Command clouddeployctl is the v3 CloudDeploy orchestrator entry point.
//
// State at Milestone 4 (in progress): doctor (apt/nvidia/cuda/system) is
// real and read-only; apply / resume / phase ubuntu-upgrade /
// base-packages / nvidia-driver / cuda / edid are real and implemented.
// The reboot continuation service is implemented. KWin real-VT and the
// patched-KWin build/install phase are implemented; Sunshine / Tailscale /
// PipeWire / HDR stream validators are NOT yet implemented, so apply exits
// after the implemented phases with a clear partial-apply banner.
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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	kwinpkg "github.com/NoviceAtPython/CloudDeploy-mover/internal/kwin"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/phase"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/reboot"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
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

	root.AddCommand(newApplyCmd())
	root.AddCommand(newResumeCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newPhaseCmd())
	root.AddCommand(newStateCmd())
	root.AddCommand(newCollectLogsCmd())

	return root
}

const longDescription = `clouddeployctl is the v3 CloudDeploy orchestrator.

Ubuntu-only. See docs/UBUNTU-ONLY.md.

State is persisted in /var/lib/clouddeploy/state.json. Logs go under
/var/log/clouddeploy/. See docs/ARCHITECTURE.md for the design,
docs/V3-ROADMAP.md for milestone status, and
docs/V3-DEPLOYMENT-READINESS.md for the current readiness audit.

Milestone 4 (in progress):
  doctor apt | nvidia | cuda | system                  read-only checks
  phase ubuntu-upgrade | base-packages |
        nvidia-driver | cuda | edid |
        kwin-patch                                      implemented
  apply                                                runs the implemented
                                                       phases above (in
                                                       that order) then
                                                       exits with a
                                                       partial-apply
                                                       banner
  resume                                               continues after
                                                       a reboot
  phase sunshine-build | services                       NOT implemented;
                                                       use the v2
                                                       CloudDeploy-
                                                       wayland.sh entry
                                                       for now.`

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
		Runner:    r,
		APT:       tx,
		State:     st,
		Profile:   profile,
		Logger:    logger,
		DryRun:    dryRun,
		StatePath: statePath,
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
  Milestone 4 partial apply complete (v3 is NOT yet v2-equivalent).

  Implemented:   apt-health, ubuntu-upgrade, base-packages, nvidia-driver,
                 cuda, edid, headless-user, desktop-packages, desktop-runtime,
                 kwin-patch, kwin-session, drm-display-validate
  NOT yet:       Sunshine fork build, Tailscale install, PipeWire virtual sink,
                 full Sunshine/Plasma systemd unit chain, HDR DRM validation,
                 HDR stream validation

  Note: ubuntu-upgrade or the EDID/GRUB phase may write changes that
        request a reboot. apply will install the continuation service
        and exit with code 2 in that case; resume picks up afterward.

  For a full deploy that reaches "AV1 10-bit HDR" in Moonlight today,
  use the v2 entrypoint:

      sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh

  v3 will own the remaining phases in subsequent Milestone 4 sub-cuts.
  See docs/V2-V3-PARITY.md for the capability audit and
  docs/V3-ROADMAP.md for milestone-by-milestone status.
================================================================================
`

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Run the implemented phases (Milestone 4 partial)",
		Long: `Run ubuntu-upgrade, base-packages, nvidia-driver, cuda, and edid
in order, then exit with a clear partial-apply banner. Each phase is
idempotent: re-running apply on the same VM skips phases already
marked done.

ubuntu-upgrade is the first phase because dist-upgrade rewrites the
apt world; it only acts when profile.ubuntu_version differs from the
host VERSION_ID AND deploy.auto_upgrade_ubuntu=true. See
docs/V2-V3-PARITY.md for the supported upgrade hops.

When a phase requests a reboot, apply writes state, schedules a
reboot via the clouddeploy-continue.service, and exits 2. After the
reboot the continuation service invokes 'clouddeployctl resume' which
picks up where apply left off.

Note: deploy.auto_reboot defaults to false unless explicitly set to true
in the active profile. When false, the operator must manually run
'sudo reboot'.`,
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
				os.Exit(10)
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
				os.Exit(10)
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
	doctor.AddCommand(newDoctorSunshineCmd())
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
			fmt.Println("doctor sunshine: NOT IMPLEMENTED in Milestone 3 partial.")
			fmt.Println("Will check (Milestone 4):")
			fmt.Println("  - /usr/local/bin/sunshine-clouddeploy presence")
			fmt.Println("  - /usr/local/bin/sunshine shadow presence")
			fmt.Println("  - getcap on both binaries (cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep)")
			fmt.Println("  - /opt/sunshine-src HEAD vs profile-pinned fork_commit")
			fmt.Println("  - SUNSHINE_FORCE_AV1_HDR10 + SUNSHINE_SYNTHESIZE_HDR10_METADATA env in sunshine-headless.service")
			return nil
		},
	}
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
			fmt.Println("validate hdr-stream: NOT IMPLEMENTED (Milestone 4).")
			fmt.Println("v2 helper: /usr/local/sbin/clouddeploy-validate-hdr-stream.")
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
	// Still not implemented: Sunshine fork build and the
	// Sunshine/Plasma systemd unit chain.
	for _, name := range []string{"sunshine-build", "services"} {
		n := name
		p.AddCommand(&cobra.Command{
			Use:   n,
			Short: fmt.Sprintf("Run the %s phase (NOT IMPLEMENTED)", n),
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("phase %s: NOT IMPLEMENTED yet (Milestone 4B/5). The desktop substrate and KWin patch phase (apt-health -> drm-display-validate) are real; Sunshine build / runtime services come next", n)
			},
		})
	}
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
	if deps.Profile != nil {
		profileName = deps.Profile.Profile
	}
	args := reboot.Args{
		Profile:   profileName,
		StatePath: deps.StatePath,
	}

	fmt.Println("Installing continuation service...")
	if err := svc.Install(ctx, args); err != nil {
		return fmt.Errorf("install continuation unit: %w", err)
	}

	if deps.Profile != nil && deps.Profile.Deploy.AutoReboot {
		fmt.Println("deploy.auto_reboot=true: Scheduling reboot now...")
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

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

func evOrUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func profileName(cmd *cobra.Command) string {
	p, _ := cmd.Flags().GetString("profile")
	if p == "" {
		return "hdr-4k120"
	}
	return p
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
