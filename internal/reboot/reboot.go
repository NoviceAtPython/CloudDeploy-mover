// Package reboot owns the systemd continuation unit
// (clouddeploy-v3-continue.service) and the reboot-scheduling glue.
//
// Lifecycle:
//
//  0. In unattended/auto-reboot mode, apply may install the unit before
//     the first risky phase. That preinstalled unit protects against
//     package-upgrade reboots that happen before a phase can return
//     ErrRebootRequired.
//
//  1. A phase that needs a reboot (e.g. nvidia-driver, edid) sets
//     state.RebootNeeded = true + state.ResumeTarget = phase-name,
//     persists state, and returns phase.ErrRebootRequired.
//
//  2. apply detects ErrRebootRequired. It writes a continuation unit
//     to /etc/systemd/system/clouddeploy-v3-continue.service that
//     runs `clouddeployctl resume --profile <name> --state-path <path>`
//     on the next boot. The unit is enabled (systemctl enable).
//
//  3. apply exits with code 2. With --auto-reboot, apply calls
//     `systemctl reboot` before exit; without it, the operator runs
//     `sudo reboot`.
//
//  4. After boot, the continuation unit fires. clouddeployctl resume
//     reads state, clears RebootNeeded / ResumeTarget, replays the
//     implemented phases. shouldSkip() on each phase makes already-
//     done work a no-op.
//
//  5. When resume finishes without another reboot, it calls
//     reboot.Disable() to systemctl disable + remove the unit.
//
// The continuation unit is intentionally simple: Type=oneshot,
// EnvironmentFile=-/etc/clouddeploy/continue.env so the profile +
// state-path travel through systemd cleanly.
package reboot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// UnitName is the systemd unit name.
const UnitName = "clouddeploy-v3-continue.service"

// DefaultUnitPath is where the unit file is written.
const DefaultUnitPath = "/etc/systemd/system/" + UnitName

// DefaultEnvFile holds the continuation environment (profile +
// state path). The unit's EnvironmentFile= references it. This file
// is non-secret and lives at 0644 by design - it contains paths +
// boolean flags only. NEVER write secrets here.
const DefaultEnvFile = "/etc/clouddeploy/continue.env"

// DefaultSecretsEnvFile holds operator-supplied secrets that must
// survive across reboot (Tailscale auth key, future API tokens, etc).
// systemd's EnvironmentFile= reads it on resume. The bootstrap +
// internal/clouddeploy/secrets helper write it 0600 root:root. The
// unit references it with a leading dash so a deploy without
// secrets still starts cleanly.
const DefaultSecretsEnvFile = "/etc/clouddeploy/secrets.env"

// DefaultBinary is the path the continuation unit invokes. bootstrap
// installs the binary here.
const DefaultBinary = "/usr/local/bin/clouddeployctl"

const DefaultConfigDir = "/opt/clouddeploy-mover/config"

// Service is the configurable surface. Construct one per CLI
// invocation; safe to leave fields zero-valued for the defaults.
type Service struct {
	UnitPath       string // default: /etc/systemd/system/clouddeploy-v3-continue.service
	EnvFile        string // default: /etc/clouddeploy/continue.env
	SecretsEnvFile string // default: /etc/clouddeploy/secrets.env (loaded with EnvironmentFile=- so its absence is fine)
	Binary         string // default: /usr/local/bin/clouddeployctl
	Runner         *runner.Runner
	DryRun         bool
}

// Args is the data the unit template consumes.
type Args struct {
	Profile    string
	StatePath  string
	ConfigDir  string
	AutoReboot bool
	Unattended bool
}

const unitTemplate = `[Unit]
Description=CloudDeploy v3 continuation (resume deploy after reboot)
After=network-online.target
Wants=network-online.target
ConditionPathExists={{ .StatePath }}

[Service]
Type=oneshot
RemainAfterExit=no
TimeoutStartSec=infinity
Restart=no
EnvironmentFile=-{{ .EnvFile }}
# Secrets file (Tailscale auth key, future API tokens). The leading
# dash makes this load best-effort: a deploy with no secrets at all
# still starts the unit cleanly. The file itself is written 0600
# root:root by the bootstrap secrets prompt + the internal/clouddeploy/
# secrets helper. systemd imports its KEY=VALUE pairs into the
# resume process's environment.
EnvironmentFile=-{{ .SecretsEnvFile }}
ExecStartPre=/bin/mkdir -p /var/log/clouddeploy
ExecStart={{ .Binary }} resume --profile {{ .Profile }} --config-dir {{ .ConfigDir }} --state-path {{ .StatePath }}{{ .AutoRebootFlag }}{{ .UnattendedFlag }}
# Append to a dedicated host log so the operator can grep without
# going through journalctl. systemd 240+ supports append: directly;
# every Ubuntu we target ships 245+.
StandardOutput=append:/var/log/clouddeploy/continue.log
StandardError=append:/var/log/clouddeploy/continue.log
SyslogIdentifier=clouddeploy-continue

[Install]
WantedBy=multi-user.target
`

// Install writes the unit + env file and runs `systemctl daemon-reload`
// + `systemctl enable`. The unit fires on the next boot.
func (s *Service) Install(ctx context.Context, args Args) error {
	if err := s.writeUnit(args); err != nil {
		return err
	}
	if err := s.writeEnvFile(args); err != nil {
		return err
	}
	if s.DryRun {
		return nil
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := s.systemctl(ctx, "enable", UnitName); err != nil {
		return err
	}
	return nil
}

// Disable runs `systemctl disable` and removes the unit + env file.
// Idempotent: missing files are not errors. systemctl invocations are
// gated by DryRun (so tests can exercise Disable without root); file
// removal always happens so the on-disk state is consistent.
func (s *Service) Disable(ctx context.Context) error {
	unitPath := s.unitPath()
	envFile := s.envFile()
	if !s.DryRun {
		// systemctl disable is best-effort: if the unit is already
		// gone or never existed, we still want to clean up the
		// files.
		_ = s.systemctl(ctx, "disable", UnitName)
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reboot: remove %s: %w", unitPath, err)
	}
	if err := os.Remove(envFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reboot: remove %s: %w", envFile, err)
	}
	if !s.DryRun {
		return s.systemctl(ctx, "daemon-reload")
	}
	return nil
}

// Reboot invokes `systemctl reboot`. Honours DryRun.
func (s *Service) Reboot(ctx context.Context) error {
	if s.DryRun {
		return nil
	}
	// systemctl reboot returns 0 immediately and the kernel does the
	// rest. If it fails (e.g. inside a container) we surface the err
	// so apply can fall back to instructing the operator.
	return s.systemctl(ctx, "reboot")
}

// IsInstalled returns true when the unit file exists on disk.
func (s *Service) IsInstalled() bool {
	_, err := os.Stat(s.unitPath())
	return err == nil
}

// -----------------------------------------------------------------------------
// helpers (exported for testing)
// -----------------------------------------------------------------------------

// RenderUnit returns the rendered unit text for the given args.
// Exported so tests can assert the exact bytes without touching disk.
func RenderUnit(args Args, envFile, binary string) string {
	return RenderUnitWithSecrets(args, envFile, binary, DefaultSecretsEnvFile)
}

// RenderUnitWithSecrets is the explicit-arity overload that lets
// callers (and the Service helper) point at a non-default secrets
// env file. An empty `secretsEnvFile` falls back to DefaultSecretsEnvFile.
func RenderUnitWithSecrets(args Args, envFile, binary, secretsEnvFile string) string {
	if envFile == "" {
		envFile = DefaultEnvFile
	}
	if binary == "" {
		binary = DefaultBinary
	}
	if secretsEnvFile == "" {
		secretsEnvFile = DefaultSecretsEnvFile
	}
	if args.ConfigDir == "" {
		args.ConfigDir = DefaultConfigDir
	}
	out := unitTemplate
	out = strings.ReplaceAll(out, "{{ .StatePath }}", args.StatePath)
	out = strings.ReplaceAll(out, "{{ .EnvFile }}", envFile)
	out = strings.ReplaceAll(out, "{{ .SecretsEnvFile }}", secretsEnvFile)
	out = strings.ReplaceAll(out, "{{ .Binary }}", binary)
	out = strings.ReplaceAll(out, "{{ .Profile }}", args.Profile)
	out = strings.ReplaceAll(out, "{{ .ConfigDir }}", args.ConfigDir)
	autoFlag := ""
	if args.AutoReboot {
		autoFlag = " --auto-reboot"
	}
	unattendedFlag := ""
	if args.Unattended {
		unattendedFlag = " --unattended"
	}
	out = strings.ReplaceAll(out, "{{ .AutoRebootFlag }}", autoFlag)
	out = strings.ReplaceAll(out, "{{ .UnattendedFlag }}", unattendedFlag)
	return out
}

// RenderEnvFile returns the env-file content. Exported for tests.
func RenderEnvFile(args Args) string {
	if args.ConfigDir == "" {
		args.ConfigDir = DefaultConfigDir
	}
	return fmt.Sprintf("CLOUDDEPLOY_PROFILE=%s\nCLOUDDEPLOY_STATE_PATH=%s\nCLOUDDEPLOY_CONFIG_DIR=%s\nCLOUDDEPLOY_AUTO_REBOOT=%t\nCLOUDDEPLOY_UNATTENDED=%t\n",
		args.Profile, args.StatePath, args.ConfigDir, args.AutoReboot, args.Unattended)
}

func (s *Service) writeUnit(args Args) error {
	body := RenderUnitWithSecrets(args, s.envFile(), s.binary(), s.secretsEnvFile())
	path := s.unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("reboot: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("reboot: write unit: %w", err)
	}
	return nil
}

// secretsEnvFile returns the configured secrets env file path,
// falling back to DefaultSecretsEnvFile. Honors CLOUDDEPLOY_SECRETS_ENV
// as an environment override so tests / unusual deployments can
// redirect the path without code changes.
func (s *Service) secretsEnvFile() string {
	if s.SecretsEnvFile != "" {
		return s.SecretsEnvFile
	}
	if v := strings.TrimSpace(os.Getenv("CLOUDDEPLOY_SECRETS_ENV")); v != "" {
		return v
	}
	return DefaultSecretsEnvFile
}

func (s *Service) writeEnvFile(args Args) error {
	body := RenderEnvFile(args)
	path := s.envFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("reboot: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("reboot: write env file: %w", err)
	}
	return nil
}

func (s *Service) systemctl(ctx context.Context, args ...string) error {
	r := s.Runner
	if r == nil {
		r = runner.New()
	}
	argv := append([]string{"systemctl"}, args...)
	res := r.Exec(ctx, runner.CommandSpec{
		Argv:   argv,
		Sudo:   true,
		DryRun: s.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("reboot: systemctl %s: %w (stderr=%q)", strings.Join(args, " "), res.Err, res.Stderr)
	}
	return nil
}

func (s *Service) unitPath() string {
	if s.UnitPath != "" {
		return s.UnitPath
	}
	return DefaultUnitPath
}

func (s *Service) envFile() string {
	if s.EnvFile != "" {
		return s.EnvFile
	}
	return DefaultEnvFile
}

func (s *Service) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return DefaultBinary
}
