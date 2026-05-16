package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/edid"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// EdidName is the canonical state-key.
const EdidName = "edid"

// EdidGrubDropIn is where the EDID-phase writes its GRUB_CMDLINE
// fragment. /etc/default/grub.d/*.cfg is honoured by update-grub on
// Ubuntu 22.04+.
const EdidGrubDropIn = "/etc/default/grub.d/99-clouddeploy-edid.cfg"

// EdidScriptPath is the helpers/write-edids.py path the phase
// invokes. Override via Edid.ScriptPath in tests.
const EdidScriptPath = "/opt/clouddeploy-mover/helpers/write-edids.py"

// Edid is the EDID/GRUB phase. Opt-in: only runs when the profile
// supplies display.forced_connector AND a recognised resolution.
type Edid struct {
	// ScriptPath overrides where write-edids.py lives. Default
	// EdidScriptPath. Empty in production; tests set it to a stub
	// Python script that just `touch`es the expected output file.
	ScriptPath string

	// FirmwareDir overrides /lib/firmware/edid for tests.
	FirmwareDir string

	// GrubDropInPath overrides /etc/default/grub.d/... for tests.
	GrubDropInPath string
}

// Name implements Phase.
func (Edid) Name() string { return EdidName }

// Run implements Phase.
func (p Edid) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	if shouldSkip(deps.State, EdidName) {
		log.Info("phase edid: already done; skipping")
		return nil
	}

	if deps.Profile == nil {
		log.Info("phase edid: no profile loaded; skipping (opt-in phase)")
		deps.State.MarkSkipped(EdidName, "no profile loaded")
		_ = deps.PersistState()
		return nil
	}

	connector := deps.Profile.Display.ForcedConnector
	if connector == "" {
		log.Info("phase edid: profile has no forced_connector; skipping")
		deps.State.MarkSkipped(EdidName, "display.forced_connector empty in profile")
		_ = deps.PersistState()
		return nil
	}

	edidFile := edid.SelectFilename(
		deps.Profile.Display.Resolution,
		deps.Profile.Display.Refresh,
		deps.Profile.Display.HDR,
	)
	if edidFile == "" {
		log.Info("phase edid: profile display config does not match a supported EDID; skipping",
			"resolution", deps.Profile.Display.Resolution,
			"refresh", deps.Profile.Display.Refresh,
			"hdr", deps.Profile.Display.HDR)
		deps.State.MarkSkipped(EdidName, "no supported EDID for profile display config")
		_ = deps.PersistState()
		return nil
	}

	deps.State.MarkRunning(EdidName)
	_ = deps.PersistState()

	scriptPath := p.ScriptPath
	if scriptPath == "" {
		scriptPath = EdidScriptPath
	}
	firmwareDir := p.FirmwareDir
	if firmwareDir == "" {
		firmwareDir = "/lib/firmware/edid"
	}
	grubDropIn := p.GrubDropInPath
	if grubDropIn == "" {
		grubDropIn = EdidGrubDropIn
	}

	// 1. Generate the EDID binaries via write-edids.py.
	log.Info("phase edid: generating EDID binaries", "script", scriptPath)
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:   []string{"python3", scriptPath},
		Sudo:   true,
		DryRun: deps.DryRun,
	})
	if res.Err != nil {
		deps.State.MarkFailed(EdidName, "write-edids.py failed", res.Err, true)
		_ = deps.PersistState()
		return fmt.Errorf("phase edid: write-edids.py: %w (stderr=%q)", res.Err, res.Stderr)
	}

	// 2. Confirm the chosen EDID landed under the firmware directory
	//    (skipped in DryRun mode since we didn't actually run the
	//    script).
	edidPath := filepath.Join(firmwareDir, edidFile)
	if !deps.DryRun {
		if _, err := os.Stat(edidPath); err != nil {
			deps.State.MarkFailed(EdidName,
				fmt.Sprintf("EDID file missing after write-edids.py: %s", edidPath),
				err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase edid: missing %s: %w", edidPath, err)
		}
	}

	// 3. Plan + write the GRUB drop-in.
	args := edid.PlanGrubArgs(connector, edidFile, knownConnectorList(connector))
	dropInBody := edid.GrubDropIn(args)
	if deps.DryRun {
		log.Info("phase edid: would write GRUB drop-in", "path", grubDropIn, "tokens", args.Tokens())
	} else {
		if err := os.MkdirAll(filepath.Dir(grubDropIn), 0o755); err != nil {
			deps.State.MarkFailed(EdidName, "mkdir grub.d", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase edid: mkdir grub.d: %w", err)
		}
		if err := os.WriteFile(grubDropIn, []byte(dropInBody), 0o644); err != nil {
			deps.State.MarkFailed(EdidName, "write grub.d drop-in", err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase edid: write grub.d: %w", err)
		}
	}

	// 4. update-initramfs + update-grub. Both need root.
	for _, step := range [][]string{
		{"update-initramfs", "-u"},
		{"update-grub"},
	} {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:   step,
			Sudo:   true,
			DryRun: deps.DryRun,
		})
		if res.Err != nil {
			deps.State.MarkFailed(EdidName, fmt.Sprintf("%s failed", step[0]), res.Err, true)
			_ = deps.PersistState()
			return fmt.Errorf("phase edid: %s: %w", step[0], res.Err)
		}
	}

	// 5. Check whether the cmdline already reflects our tokens. If so,
	//    no reboot is needed; we were probably re-running on an
	//    already-configured host.
	cmdline := readCmdline()
	if edid.CmdlinePresent(cmdline, args) {
		deps.State.MarkDone(EdidName, map[string]any{
			"connector":     connector,
			"edid_file":     edidFile,
			"reboot_needed": false,
		})
		_ = deps.PersistState()
		log.Info("phase edid: cmdline already reflects forced EDID; no reboot needed")
		return nil
	}

	// 6. Reboot needed for the new cmdline to take effect.
	deps.State.MarkRunning(EdidName)
	deps.State.Get(EdidName).Details = map[string]any{
		"connector":     connector,
		"edid_file":     edidFile,
		"reboot_needed": true,
		"grub_drop_in":  grubDropIn,
	}
	deps.State.SetRebootNeeded(true, EdidName)
	_ = deps.PersistState()
	log.Warn("phase edid: GRUB updated; reboot required for the new cmdline to take effect")
	return ErrRebootRequired
}

// knownConnectorList returns the standard list of DRM connectors the
// EDID phase disables (so the kernel doesn't try to drive a
// non-existent display). DP-2 / DP-3 / HDMI-A-1 are the usual
// suspects; we include DP-2 explicitly because the v2 path adds
// `video=DP-2:d` unconditionally.
//
// `forced` is the connector the operator wants enabled; it's
// filtered out inside PlanGrubArgs.
func knownConnectorList(forced string) []string {
	return []string{"DP-2", "DP-3", "HDMI-A-1"}
}

// readCmdline returns the contents of /proc/cmdline, or "" on any
// error. Used to detect "already-configured host" so the phase can
// skip the reboot request.
func readCmdline() string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return string(b)
}
