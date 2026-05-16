// Command clouddeployctl is the v3 CloudDeploy orchestrator entry point.
//
// CloudDeploy turns a freshly-provisioned Ubuntu VM into a Sunshine
// streaming host with KMS capture, AV1 NVENC, and (optionally) the
// patched-KWin NVIDIA private HDR path documented in
// docs/HDR-NVIDIA-PRIVATE.md.
//
// This binary is the v3 replacement for the v2
// CloudDeploy-wayland.sh monolith. See docs/ARCHITECTURE.md for the
// design rationale and docs/MIGRATION.md for the v2 -> v3 plan.
//
// First-pass scope: this binary exposes the full subcommand surface
// from the v3 brief but most subcommands are stubs that explain what
// they will do in subsequent milestones. The three modules with real
// logic in this commit are internal/nvidia, internal/cuda, and
// internal/apt (each with their own unit tests).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/cuda"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/nvidia"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/state"
)

// Version is overridden via -ldflags '-X main.Version=...'.
var Version = "v3.0.0-dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		// Cobra prints its own errors. Exit with non-zero so callers
		// (bootstrap.sh, CI, systemd) can detect failure.
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "clouddeployctl",
		Short:         "CloudDeploy v3 orchestrator",
		Long:          longDescription,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	// Persistent flags live on the root command.
	root.PersistentFlags().String("profile", "hdr-4k120", "deploy profile from config/profiles/<name>.yaml")
	root.PersistentFlags().String("state-path", state.DefaultPath, "path to the CloudDeploy state file")
	root.PersistentFlags().String("config-dir", defaultConfigDir, "path to the config/ directory")
	root.PersistentFlags().Bool("dry-run", false, "do not make destructive changes; show what would happen")
	root.PersistentFlags().Bool("verbose", false, "verbose logging")

	root.AddCommand(newApplyCmd())
	root.AddCommand(newResumeCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newPhaseCmd())
	root.AddCommand(newStateCmd())

	return root
}

// defaultConfigDir is the install-time default. bootstrap.sh deploys
// the repo under /opt/clouddeploy-mover, so config/ lives at
// /opt/clouddeploy-mover/config. Developers running `go run` from the
// repo root get the current directory.
const defaultConfigDir = "/opt/clouddeploy-mover/config"

const longDescription = `clouddeployctl is the v3 CloudDeploy orchestrator.

It owns: NVIDIA driver package-family selection, CUDA policy, apt
transaction wrapping, KWin patch pinning, Sunshine fork build, EDID /
GRUB / systemd unit management, and HDR streaming validation.

State is persisted in /var/lib/clouddeploy/state.json. Logs go under
/var/log/clouddeploy/. See docs/ARCHITECTURE.md for the design and
docs/MIGRATION.md for the migration from the v2 Bash script.

In this milestone (1), apply / resume / phase are stubs. doctor
nvidia and doctor cuda use real selectors from internal/nvidia and
internal/cuda. State, config, and apt transaction wrappers are
implemented and unit-tested.

The v2 entrypoint, CloudDeploy-wayland.sh at the repo root, is
unaffected by this binary and remains the validated deploy path
until v3 reaches Milestone 5.`

// -----------------------------------------------------------------------------
// apply
// -----------------------------------------------------------------------------

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Run all required phases for the selected profile",
		Long: `Run all deploy phases (base-packages, nvidia-driver, cuda,
kwin-patch, sunshine-build, services, validate) for the selected
profile. Phases already marked done in state.json are skipped.

Milestone 1: stub. See docs/MIGRATION.md for the rollout plan.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, _ := cmd.Flags().GetString("profile")
			fmt.Printf("clouddeployctl apply --profile %s\n", profile)
			fmt.Println()
			fmt.Println("This subcommand is a Milestone-1 stub. Production deploys")
			fmt.Println("should keep using the v2 entry point:")
			fmt.Println()
			fmt.Println("    sudo ENABLE_HDR=1 bash ./CloudDeploy-wayland.sh")
			fmt.Println()
			fmt.Println("See docs/MIGRATION.md.")
			return nil
		},
	}
}

// -----------------------------------------------------------------------------
// resume
// -----------------------------------------------------------------------------

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume a deploy after a reboot",
		Long: `resume reads /var/lib/clouddeploy/state.json, identifies the
last-completed phase, and continues from the next pending phase.

The continuation systemd service calls this on boot. It is functionally
equivalent to 'apply' after a reboot but assumes the profile from
state.json rather than --profile.

Milestone 1: stub.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("resume is a Milestone-1 stub. See docs/MIGRATION.md.")
			return nil
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
		Long: `doctor runs the same idempotency checks each phase uses, but
read-only. It never installs, removes, or modifies anything; it
reports what the system looks like and what the next 'apply' would do.

With no subsystem argument, doctor runs every check and prints a
summary.

Subsystems:

  doctor nvidia    GPU detection, driver package family, dkms, nvidia-smi.
  doctor cuda      CUDA toolkit policy + presence + version.
  doctor apt       dpkg state, lock holders, policy-rc.d.
  doctor kwin      Patched-KWin marker + apt pinning + private HDR props.
  doctor sunshine  Sunshine binary, capabilities, fork-commit pin, env.

Milestone 1: nvidia and cuda are implemented; the rest are stubs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// No subsystem: print a summary.
			fmt.Println("Run a subsystem: clouddeployctl doctor [nvidia|cuda|apt|kwin|sunshine]")
			return nil
		},
	}
	doctor.AddCommand(newDoctorNvidiaCmd())
	doctor.AddCommand(newDoctorCudaCmd())
	doctor.AddCommand(newDoctorAptCmd())
	doctor.AddCommand(newDoctorKwinCmd())
	doctor.AddCommand(newDoctorSunshineCmd())
	return doctor
}

func newDoctorNvidiaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "nvidia",
		Short: "Report NVIDIA driver readiness (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			driverMajor, _ := cmd.Flags().GetString("driver-major")
			opts := nvidia.EvidenceOptions{DriverMajor: driverMajor}
			ev, err := nvidia.GatherEvidenceFromHost(opts)
			if err != nil {
				fmt.Printf("doctor nvidia: could not gather evidence: %v\n", err)
				return nil
			}
			cls := nvidia.Classify(ev.GPUName, ev.PCIID)
			fam, reason, selErr := nvidia.SelectFamily(ev)
			scanned := nvidia.ScannedMajors(opts)
			fmt.Printf("doctor nvidia:\n")
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
			fmt.Printf("  Reason                  : %s\n", reason)
			if selErr != nil {
				fmt.Printf("  Error                   : %v\n", selErr)
			}
			if !ev.AvailabilityKnown {
				fmt.Printf("  Note                    : apt-cache was not consulted; availability is approximate.\n")
			}
			if driverMajor == "" {
				fmt.Printf("  Note                    : --driver-major not supplied; scanned %v as a best-effort guess.\n", scanned)
			}
			return nil
		},
	}
	c.Flags().String("driver-major", "", "NVIDIA driver major version to scan (e.g. 580); defaults to a fallback set when empty")
	return c
}

func newDoctorCudaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cuda",
		Short: "Report CUDA toolkit readiness (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Milestone 1: print the configured CUDA mode and what
			// that means for the deploy.
			mode := cuda.ModeNone // default for hdr-4k120
			plan := cuda.Plan(mode)
			fmt.Printf("doctor cuda:\n")
			fmt.Printf("  Configured mode      : %s\n", plan.Mode)
			fmt.Printf("  Will attempt install : %v\n", plan.WillAttemptInstall)
			fmt.Printf("  Fails deploy on err  : %v\n", plan.FailsDeployOnError)
			fmt.Printf("  Rationale            : %s\n", plan.Rationale)
			return nil
		},
	}
}

func newDoctorAptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apt",
		Short: "Report dpkg/apt state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("doctor apt: Milestone-2 stub. Will report:")
			fmt.Println("  - dpkg lock holders")
			fmt.Println("  - pending postinst")
			fmt.Println("  - policy-rc.d presence + provenance")
			fmt.Println("  - any service-start postinst that could deadlock")
			return nil
		},
	}
}

func newDoctorKwinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kwin",
		Short: "Report patched-KWin / NVIDIA private HDR state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("doctor kwin: Milestone-4 stub. Will report:")
			fmt.Println("  - patched-KWin marker presence")
			fmt.Println("  - apt pinning state")
			fmt.Println("  - kscreen-doctor HDR + WCG state")
			fmt.Println("  - NV_CRTC_REGAMMA_TF / NV_INPUT_COLORSPACE / NV_PLANE_DEGAMMA_TF")
			return nil
		},
	}
}

func newDoctorSunshineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sunshine",
		Short: "Report Sunshine fork build/install state (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("doctor sunshine: Milestone-4 stub. Will report:")
			fmt.Println("  - /usr/local/bin/sunshine-clouddeploy presence + caps")
			fmt.Println("  - /usr/local/bin/sunshine shadow presence + caps")
			fmt.Println("  - source tree HEAD vs profile-pinned fork commit")
			fmt.Println("  - SUNSHINE_FORCE_AV1_HDR10 / SUNSHINE_SYNTHESIZE_HDR10_METADATA env")
			return nil
		},
	}
}

// -----------------------------------------------------------------------------
// validate
// -----------------------------------------------------------------------------

func newValidateCmd() *cobra.Command {
	v := &cobra.Command{
		Use:   "validate [check]",
		Short: "Run validators (read-only)",
		Long: `validate runs an end-to-end check.

Checks:

  validate hdr-stream    Grep the Sunshine journal for the HDR success
                         markers. Equivalent to v2's
                         clouddeploy-validate-hdr-stream helper.

Milestone 1: stub.`,
	}
	v.AddCommand(&cobra.Command{
		Use:   "hdr-stream",
		Short: "Confirm Sunshine emitted the HDR control packet to Moonlight",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("validate hdr-stream: Milestone-4 stub. Will run the same checks as")
			fmt.Println("/usr/local/sbin/clouddeploy-validate-hdr-stream (v2 helper).")
			return nil
		},
	})
	return v
}

// -----------------------------------------------------------------------------
// phase
// -----------------------------------------------------------------------------

func newPhaseCmd() *cobra.Command {
	p := &cobra.Command{
		Use:   "phase [name]",
		Short: "Run a single phase (development tool)",
		Long: `Run exactly one deploy phase. Used during development when
iterating on a single module.

Phases:

  base-packages    apt update + base package install
  nvidia-driver    select family + install driver + dkms
  cuda             apply cuda.mode policy
  kwin-patch       apply + build patched KWin
  sunshine-build   pin + build + install Sunshine fork
  services         generate + start systemd units

Milestone 1: stubs.`,
	}
	for _, name := range []string{
		"base-packages",
		"nvidia-driver",
		"cuda",
		"kwin-patch",
		"sunshine-build",
		"services",
	} {
		n := name
		p.AddCommand(&cobra.Command{
			Use:   n,
			Short: fmt.Sprintf("Run the %s phase", n),
			RunE: func(cmd *cobra.Command, args []string) error {
				fmt.Printf("phase %s: Milestone-1 stub. See docs/MIGRATION.md.\n", n)
				return nil
			},
		})
	}
	return p
}

// -----------------------------------------------------------------------------
// state
// -----------------------------------------------------------------------------

func newStateCmd() *cobra.Command {
	s := &cobra.Command{
		Use:   "state",
		Short: "Inspect / reset the CloudDeploy state file",
	}
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
	s.AddCommand(&cobra.Command{
		Use:   "reset",
		Short: "Force a phase back to pending",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("state reset: Milestone-1 stub.")
			return nil
		},
	})
	return s
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func evOrUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
