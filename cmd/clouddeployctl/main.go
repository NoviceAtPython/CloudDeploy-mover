// Command clouddeployctl is the v3 CloudDeploy orchestrator entry point.
//
// State at Milestone 3 partial: doctor (apt/nvidia/cuda/system) is
// real and read-only; apply / resume / phase base-packages /
// nvidia-driver / cuda / edid are real and implemented. The reboot
// continuation service is implemented. (KWin / Sunshine / 
// KWin/Plasma/Sunshine systemd units / HDR validation are NOT yet 
// implemented; apply exits after the implemented phases with a clear 
// partial-apply banner).
//
// See docs/V3-DEPLOYMENT-READINESS.md for the rollout plan and
// docs/V3-ROADMAP.md for milestone-by-milestone status.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/apt"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
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

Milestone 3 partial:
  doctor apt | nvidia | cuda | system                read-only checks
    phase base-packages | nvidia-driver | cuda | edid   implemented
    apply                                                runs the implemented
                                                       phases above
                                                       then exits with
                                                       a partial-apply
                                                       banner
  resume                                               continues after
                                                       a reboot
  phase kwin-patch | sunshine-build | services         NOT implemented;
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
	var lock *state.Lock
	if profileRequired {
		l, err := state.Acquire("")
		if err != nil {
			return nil, nil, fmt.Errorf("acquire process lock: %w", err)
		}
		lock = l
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

// -----------------------------------------------------------------------------
// apply
// -----------------------------------------------------------------------------

const partialApplyBanner = `
================================================================================
  Milestone 3 partial apply complete.

  Implemented:   base-packages, nvidia-driver, cuda, edid
  NOT yet:       kwin-patch, sunshine-build, services,
                 HDR DRM validation, HDR stream validation

  Note: The EDID/GRUB phase may write kernel arguments and request a reboot.

  For a full deploy that reaches "AV1 10-bit HDR" in Moonlight today,
  use the v2 entrypoint:

      sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh

  v3 will own the remaining phases in Milestone 4. See docs/V3-ROADMAP.md.
================================================================================
`

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Run the implemented phases (Milestone 3 partial)",
		Long: `Run base-packages, nvidia-driver, cuda, and edid in order, then exit
with a clear partial-apply banner. Each phase is idempotent: re-running
apply on the same VM skips phases already marked done.

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

			allowUnsup, _ := cmd.Flags().GetBool("allow-unsupported")

			var targetVersion string
			if deps.Profile != nil {
				targetVersion = deps.Profile.UbuntuVersion
			}
			res, err := ubuntu.ReadAndGate(ubuntu.DefaultOSReleasePath, targetVersion)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("ubuntu gate read: %w", err)
			}
			if !res.Supported {
				fmt.Printf("OS unsupported:\n  Found: %s (version %s)\n  Supported: %v\n  Reason: %s\n",
					res.Release.ID, res.Release.VersionID, ubuntu.SupportedVersions(), res.Reason)
				if !allowUnsup {
					return fmt.Errorf("bailing out due to unsupported OS. Use --allow-unsupported to override")
				}
				fmt.Println("Warning: proceeding anyway due to --allow-unsupported")
			}

			err = runPhasesAndBanner(ctx, deps, false)

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
func runPhasesAndBanner(ctx context.Context, deps *phase.Deps, isResume bool) error {
	phases := []phase.Phase{
		phase.BasePackages{},
		phase.NvidiaDriver{},
		phase.Cuda{},
		phase.Edid{},
	}

	if isResume {
		svc := &reboot.Service{Runner: deps.Runner, DryRun: deps.DryRun}
		if svc.IsInstalled() {
			fmt.Println("resume: Disabling continuation service...")
			_ = svc.Disable(ctx)
		}
	}

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
	fmt.Print(partialApplyBanner)
	return nil
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

			if deps.State.RebootNeeded {
				deps.State.ClearRebootNeeded()
				_ = deps.PersistState()
				fmt.Println("resume: clearing RebootNeeded marker")
			}

			err = runPhasesAndBanner(ctx, deps, true)
			lock.Release()

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
	doctor.AddCommand(newDoctorSunshineCmd())
	return doctor
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
			if deps.Profile != nil {
				if m, perr := cuda.ParseMode(deps.Profile.CUDA.Mode); perr == nil {
					mode = m
				}
			}
			plan := cuda.Plan(mode)

			// Probe the host for nvcc.
			_, nvccErr := lookExecutable("nvcc")
			nvccPresent := nvccErr == nil

			fmt.Println("doctor cuda:")
			if deps.Profile != nil {
				fmt.Printf("  Profile                 : %s\n", deps.Profile.Profile)
			} else {
				fmt.Printf("  Profile                 : (no profile loaded)\n")
			}
			fmt.Printf("  cuda.mode               : %s\n", plan.Mode)
			fmt.Printf("  Will attempt install    : %v\n", plan.WillAttemptInstall)
			fmt.Printf("  Fails deploy on error   : %v\n", plan.FailsDeployOnError)
			fmt.Printf("  Sunshine CUDA module    : %v\n", plan.WillAttemptInstall)
			fmt.Printf("  nvcc on PATH            : %v\n", nvccPresent)
			fmt.Println()
			fmt.Println("Rationale:")
			fmt.Printf("  %s\n", plan.Rationale)
			fmt.Println()
			fmt.Println("Runfile retry policy (when runfile install lands in Milestone 4):")
			fmt.Println("  - same (sha256, size) that already failed --check => refuse to redownload.")
			fmt.Println("  - 3 consecutive --check failures => give up.")
			fmt.Println("  - mode=optional => mark phase failed_nonfatal and continue.")
			fmt.Println("  - mode=required => mark phase failed_fatal.")
			return nil
		},
	}
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
		Short: "Report patched-KWin / NVIDIA private HDR state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("doctor kwin: NOT IMPLEMENTED in Milestone 3 partial.")
			fmt.Println("Will check (Milestone 4):")
			fmt.Println("  - /var/lib/clouddeploy/patched-kwin-installed marker")
			fmt.Println("  - apt-mark hold status for kwin packages")
			fmt.Println("  - kscreen-doctor -o HDR + Wide Color Gamut state")
			fmt.Println("  - NV_CRTC_REGAMMA_TF / NV_INPUT_COLORSPACE / NV_PLANE_DEGAMMA_TF via drm_info")
			fmt.Println()
			fmt.Println("For HDR-side validation today, run scripts/validate-hdr-drm-state.py")
			fmt.Println("(invoked by v2's validate_hdr_final_state).")
			return nil
		},
	}
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
	p.AddCommand(newPhaseImplCmd("base-packages", phase.BasePackages{}))
	p.AddCommand(newPhaseImplCmd("nvidia-driver", phase.NvidiaDriver{}))
	p.AddCommand(newPhaseImplCmd("cuda", phase.Cuda{}))
	p.AddCommand(newPhaseImplCmd("edid", phase.Edid{}))
	for _, name := range []string{"kwin-patch", "sunshine-build", "services"} {
		n := name
		p.AddCommand(&cobra.Command{
			Use:   n,
			Short: fmt.Sprintf("Run the %s phase (NOT IMPLEMENTED)", n),
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("phase %s: NOT IMPLEMENTED in Milestone 3 partial. Use v2 CloudDeploy-wayland.sh for now", n)
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
