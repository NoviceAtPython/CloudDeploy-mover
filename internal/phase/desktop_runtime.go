package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// DesktopRuntimeName is the canonical state-key.
const DesktopRuntimeName = "desktop_runtime"

// DefaultDRIDir is the kernel's DRM device-node root.
const DefaultDRIDir = "/dev/dri"

// DefaultNvidiaDRMModesetPath is the sysfs entry the phase reads to
// verify the NVIDIA DRM kernel module is in modeset mode (required
// for KWin Wayland on NVIDIA).
const DefaultNvidiaDRMModesetPath = "/sys/module/nvidia_drm/parameters/modeset"

// DefaultProcCmdlinePath is /proc/cmdline.
const DefaultProcCmdlinePath = "/proc/cmdline"

// RequiredCmdlineTokens are the kernel cmdline tokens the edid phase
// is supposed to have stamped via update-grub. desktop_runtime
// confirms they actually made it to the running kernel; missing
// tokens mean the operator skipped the reboot after edid.
var RequiredCmdlineTokens = []string{
	"nvidia-drm.modeset=1",
	"nvidia-drm.fbdev=1",
}

// CmdlineTokenForConnector returns the connector-disable / enable
// token the edid phase stamps. e.g. "video=DP-1:e". The check is
// a substring match against /proc/cmdline.
func CmdlineTokenForConnector(connector string, enabled bool) string {
	if connector == "" {
		return ""
	}
	suffix := "d"
	if enabled {
		suffix = "e"
	}
	return fmt.Sprintf("video=%s:%s", connector, suffix)
}

// CmdlineTokenForEdid is the kernel cmdline token that points at the
// edid firmware blob. e.g. "drm.edid_firmware=DP-1:edid/virtual-4k120-hdr.bin".
func CmdlineTokenForEdid(connector, edid string) string {
	if connector == "" || edid == "" {
		return ""
	}
	return fmt.Sprintf("drm.edid_firmware=%s:edid/%s", connector, edid)
}

// DesktopRuntime is the third Milestone 4A phase. It does NOT
// install anything; it verifies that the host has every prerequisite
// the KWin Wayland session needs:
//
//  1. /dev/dri/card* and /dev/dri/renderD* exist (DRM nodes).
//  2. /sys/module/nvidia_drm/parameters/modeset is Y (NVIDIA KMS).
//  3. /proc/cmdline contains the EDID + modeset tokens the edid
//     phase stamped. Missing tokens mean the operator skipped the
//     reboot after edid (or the cmdline write didn't land).
//  4. The headless user is in the `video` AND `render` groups so it
//     can open the DRM nodes without root.
//
// Anything missing is recorded in state.Details and the phase fails
// fatal so kwin_session doesn't try to start a compositor against a
// missing /dev/dri/render*.
type DesktopRuntime struct {
	// DRIDir overrides /dev/dri for tests.
	DRIDir string
	// NvidiaDRMModesetPath overrides the sysfs read for tests.
	NvidiaDRMModesetPath string
	// CmdlinePath overrides /proc/cmdline for tests.
	CmdlinePath string

	// UserGroupsFn returns the supplementary groups for a given
	// user. Defaults to reading via the runner (id -nG). Tests can
	// fake it.
	UserGroupsFn func(ctx context.Context, deps *Deps, user string) ([]string, error)
}

// Name implements Phase.
func (DesktopRuntime) Name() string { return DesktopRuntimeName }

// Run implements Phase.
func (p DesktopRuntime) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if shouldSkip(deps.State, DesktopRuntimeName) {
		log.Info("phase desktop-runtime: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(DesktopRuntimeName)
	_ = deps.PersistState()

	desk := deps.Profile.EffectiveDesktop()
	details := map[string]any{
		"user":   desk.User,
		"groups": desk.Groups,
	}
	var problems []string

	// 1. DRI nodes.
	driDir := p.DRIDir
	if driDir == "" {
		driDir = DefaultDRIDir
	}
	cards, renderNodes, driErr := listDRINodes(driDir)
	details["dri_cards"] = cards
	details["render_nodes"] = renderNodes
	if driErr != nil {
		details["dri_err"] = driErr.Error()
	}
	if len(cards) == 0 || len(renderNodes) == 0 {
		problems = append(problems, fmt.Sprintf("no DRM device nodes under %s (cards=%v render=%v err=%v)",
			driDir, cards, renderNodes, driErr))
	}

	// 2. NVIDIA DRM modeset.
	nvmsPath := p.NvidiaDRMModesetPath
	if nvmsPath == "" {
		nvmsPath = DefaultNvidiaDRMModesetPath
	}
	nvmsRaw := readTrimmedFile(nvmsPath)
	details["nvidia_drm_modeset"] = nvmsRaw
	if nvmsRaw != "Y" {
		problems = append(problems, fmt.Sprintf("%s = %q; want \"Y\". Reboot after edid + ensure nvidia-drm.modeset=1 on the cmdline",
			nvmsPath, nvmsRaw))
	}

	// 3. /proc/cmdline checks.
	cmdlinePath := p.CmdlinePath
	if cmdlinePath == "" {
		cmdlinePath = DefaultProcCmdlinePath
	}
	cmdline := readTrimmedFile(cmdlinePath)
	details["cmdline"] = cmdline
	missingTokens := []string{}
	for _, tok := range RequiredCmdlineTokens {
		if !strings.Contains(cmdline, tok) {
			missingTokens = append(missingTokens, tok)
		}
	}
	if deps.Profile != nil {
		conn := strings.TrimSpace(deps.Profile.Display.ForcedConnector)
		edid := strings.TrimSpace(deps.Profile.Display.Edid)
		if conn != "" {
			tok := CmdlineTokenForConnector(conn, true)
			if !strings.Contains(cmdline, tok) {
				missingTokens = append(missingTokens, tok)
			}
		}
		if conn != "" && edid != "" {
			tok := CmdlineTokenForEdid(conn, edid)
			if !strings.Contains(cmdline, tok) {
				missingTokens = append(missingTokens, tok)
			}
		}
	}
	details["cmdline_missing"] = missingTokens
	details["cmdline_ok"] = len(missingTokens) == 0
	if len(missingTokens) > 0 {
		problems = append(problems, fmt.Sprintf("kernel cmdline is missing %d required token(s): %v. Did the operator reboot after edid?",
			len(missingTokens), missingTokens))
	}

	// 4. Headless user has DRM access groups.
	groupsFn := p.UserGroupsFn
	if groupsFn == nil {
		groupsFn = defaultUserGroups
	}
	gotGroups, gerr := groupsFn(ctx, deps, desk.User)
	if gerr != nil {
		details["groups_lookup_err"] = gerr.Error()
		problems = append(problems, fmt.Sprintf("user %q groups lookup failed: %v", desk.User, gerr))
	}
	have := map[string]bool{}
	for _, g := range gotGroups {
		have[g] = true
	}
	deviceAccessOk := have["video"] && have["render"]
	details["user_groups_now"] = gotGroups
	details["user_device_access_ok"] = deviceAccessOk
	if !deviceAccessOk {
		problems = append(problems, fmt.Sprintf("user %q is missing one of {video,render} groups (got %v). Re-run phase headless-user",
			desk.User, gotGroups))
	}

	if len(problems) > 0 {
		err := fmt.Errorf("desktop-runtime preconditions not met:\n  - %s", strings.Join(problems, "\n  - "))
		details["problems"] = problems
		deps.State.MarkFailed(DesktopRuntimeName, "desktop runtime preconditions", err, true)
		deps.State.Get(DesktopRuntimeName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase desktop-runtime: %w", err)
	}

	deps.State.MarkDone(DesktopRuntimeName, details)
	_ = deps.PersistState()
	log.Info("phase desktop-runtime: done",
		"dri_cards", cards, "render_nodes", renderNodes,
		"nvidia_drm_modeset", nvmsRaw, "cmdline_ok", true,
		"user_device_access_ok", deviceAccessOk)
	return nil
}

// listDRINodes returns ("/dev/dri/card*", "/dev/dri/renderD*") as
// absolute paths.
func listDRINodes(dir string) (cards, render []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		switch {
		case strings.HasPrefix(name, "card"):
			cards = append(cards, full)
		case strings.HasPrefix(name, "renderD"):
			render = append(render, full)
		}
	}
	return cards, render, nil
}

// readTrimmedFile reads a small text file and returns its trimmed
// contents. Returns "" on any error (the desktop-runtime checks
// surface the failure through the absent-token / non-Y compare
// instead of crashing).
func readTrimmedFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// defaultUserGroups shells out to `id -nG <user>` via the runner.
// Mirrors defaultLookup in headless_user but exposes a stand-alone
// signature so desktop_runtime can be re-tested in isolation.
func defaultUserGroups(ctx context.Context, deps *Deps, user string) ([]string, error) {
	if deps == nil || deps.Runner == nil {
		return nil, fmt.Errorf("runner unavailable")
	}
	info, err := defaultLookup(ctx, deps, user)
	if err != nil {
		return nil, err
	}
	if info.UID == "" {
		return nil, fmt.Errorf("user %q does not exist", user)
	}
	return info.Groups, nil
}
