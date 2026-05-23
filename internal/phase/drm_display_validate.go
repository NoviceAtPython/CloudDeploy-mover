package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/config"
	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// DRMDisplayValidateName is the canonical state-key.
const DRMDisplayValidateName = "drm_display_validate"

const (
	DRMFailNoCandidateConnector           = "drm_no_candidate_connector"
	DRMFailExpectedModeMissing            = "drm_expected_mode_missing"
	DRMFailKScreenRejectedOutputConfig    = "kscreen_rejected_output_config"
	DRMFailKScreenSysfsStateMismatch      = "kscreen_sysfs_state_mismatch"
	DRMFailSysfsEnabledKScreenDisabled    = "drm_sysfs_enabled_but_kscreen_disabled_unverified"
	DRMVerifiedSysfsWayland               = "drm_output_verified_via_sysfs_and_wayland"
	DRMWarnNvidiaDumbCreateAlloc          = "nvidia_dumb_create_alloc_warning"
	DRMDisplayTargetFallbackApplied       = "display_target_fallback_applied"
	DRMFailDisplayTargetStrictUnavailable = "display_target_strict_unavailable"
)

type DRMConnectorState struct {
	Card          string   `json:"drm_card"`
	Name          string   `json:"connector_name"`
	Path          string   `json:"sysfs_path"`
	SysfsBasename string   `json:"sysfs_basename"`
	Status        string   `json:"status"`
	Enabled       string   `json:"enabled"`
	Modes         []string `json:"modes"`
	NVIDIA        bool     `json:"nvidia"`
	Connected     bool     `json:"connected"`
	IsEnabled     bool     `json:"is_enabled"`
}

type displayTarget struct {
	Mode       string `json:"mode"`
	HDR        bool   `json:"hdr"`
	Reason     string `json:"reason"`
	Diagnostic bool   `json:"diagnostic"`
}

// KScreenDoctorOutput is the parsed shape of `kscreen-doctor -o`.
// We only model the subset of fields the phase actually cares about
// (existence + enabled + mode list).
type KScreenDoctorOutput struct {
	Connectors []KScreenConnector
}

// KScreenConnector is one output reported by kscreen-doctor. Modes
// are stored as "<width>x<height>@<refresh>" strings for stable
// equality checks.
type KScreenConnector struct {
	// ID is the kscreen output id used in commands like
	// `kscreen-doctor output.<ID>.enable`.
	ID string
	// Name is the connector name (e.g. "DP-1", "HDMI-A-1").
	Name string
	// Enabled is the boolean kscreen-doctor reports.
	Enabled bool
	// CurrentMode is the active mode, "" when disabled.
	CurrentMode string
	// Modes is every mode kscreen-doctor enumerated under this
	// connector, in the same order it printed them.
	Modes []string
	// ModeIDs maps canonical mode strings to kscreen mode ids.
	ModeIDs map[string]string
	HDR     bool
	WCG     bool
}

// FindConnector returns the connector matching name (case-sensitive),
// or nil when absent.
func (o KScreenDoctorOutput) FindConnector(name string) *KScreenConnector {
	for i := range o.Connectors {
		if o.Connectors[i].Name == name {
			return &o.Connectors[i]
		}
	}
	return nil
}

// HasMode reports whether the connector enumerates the given mode
// string anywhere in its mode list.
func (c KScreenConnector) HasMode(mode string) bool {
	for _, m := range c.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// kscreenOutputRE matches the "Output: <id> <Name>" header
// kscreen-doctor prints. The id is ignored.
var kscreenOutputRE = regexp.MustCompile(`^Output:\s+(\S+)\s+(\S+)`)

// kscreenEnabledRE matches "        enabled" / "        disabled" on
// its own line (older kscreen-doctor output shape).
var kscreenEnabledRE = regexp.MustCompile(`^\s*(enabled|disabled)\s*$`)

// kscreenHeaderEnabledRE matches the inline "enabled" / "disabled"
// token on the live-VM "Output: 1 DP-1 hdmi enabled connected ..."
// header line that Plasma 6.4.x kscreen-doctor emits. The parser
// inspects the rest of the same line when it sees an Output: header.
var kscreenHeaderEnabledRE = regexp.MustCompile(`\b(enabled|disabled)\b`)

// kscreenCurrentModeRE matches the "        Modes: 1!  3840x2160@120, ..." line.
// The leading "!" marks the currently active mode in some
// kscreen-doctor builds; we parse both formats.
var kscreenModesLineRE = regexp.MustCompile(`^\s*Modes:\s+(.*)$`)
var kscreenHDRRE = regexp.MustCompile(`^\s*HDR:\s+(enabled|disabled)\s*$`)
var kscreenWCGRE = regexp.MustCompile(`^\s*Wide Color Gamut:\s+(enabled|disabled)\s*$`)

// ansiEscapeRE matches CSI sequences (ESC [ ... letter) and the
// shorter ESC ( ... single-char sequences kscreen-doctor emits when
// it thinks the terminal supports color. Plasma 6.4.x emits these
// even when stdout is a pipe, so the parser strips them before any
// regex match runs. Live VM evidence:
//
//	"\x1b[01;34mOutput:\x1b[0;0m 1 DP-1 ..."
//	"\x1b[01;34mModes:\x1b[0;0m 1:3840x2160@60! 2:\x1b[01;32m3840x2160@120*\x1b[0;0m"
//	"\x1b[01;33mHDR:\x1b[0;0m enabled"
//	"\x1b[01;33mWide Color Gamut:\x1b[0;0m enabled"
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes ANSI CSI sequences from `text`. Exported so
// other phases (doctor kwin, force-kwin-mode helper diagnostics)
// can sanitize captured tool output the same way.
func StripANSI(text string) string {
	if !strings.Contains(text, "\x1b") {
		return text
	}
	return ansiEscapeRE.ReplaceAllString(text, "")
}

// ParseKScreenDoctor parses the textual output of `kscreen-doctor -o`.
// The parser is intentionally permissive: kscreen-doctor's output
// shape differs across Plasma 5/6 minor versions and we want the
// phase to extract what it can rather than fail-hard on a layout
// change. ANSI color escapes (Plasma 6.4.x emits them even on a
// pipe) are stripped before matching.
func ParseKScreenDoctor(text string) KScreenDoctorOutput {
	text = StripANSI(text)
	var out KScreenDoctorOutput
	var cur *KScreenConnector
	flush := func() {
		if cur != nil {
			out.Connectors = append(out.Connectors, *cur)
			cur = nil
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		if m := kscreenOutputRE.FindStringSubmatch(line); m != nil {
			flush()
			cur = &KScreenConnector{ID: m[1], Name: m[2], ModeIDs: map[string]string{}}
			// Live VM (Plasma 6.4.x): the Output: header line also
			// includes the connection state inline, e.g.
			//   "Output: 1 DP-1 hdmi enabled connected priority 1 1"
			// Parse it here so connectors lacking the standalone
			// "        enabled" / "        disabled" successor line
			// still have the right value. The standalone-line check
			// below still wins if it ever runs (safer signal).
			if hm := kscreenHeaderEnabledRE.FindStringSubmatch(line); hm != nil {
				cur.Enabled = hm[1] == "enabled"
			}
			continue
		}
		if cur == nil {
			continue
		}
		if m := kscreenEnabledRE.FindStringSubmatch(line); m != nil {
			cur.Enabled = m[1] == "enabled"
			continue
		}
		if m := kscreenModesLineRE.FindStringSubmatch(line); m != nil {
			cur.Modes, cur.CurrentMode, cur.ModeIDs = parseKScreenModesField(m[1])
			continue
		}
		if m := kscreenHDRRE.FindStringSubmatch(line); m != nil {
			cur.HDR = m[1] == "enabled"
			continue
		}
		if m := kscreenWCGRE.FindStringSubmatch(line); m != nil {
			cur.WCG = m[1] == "enabled"
			continue
		}
	}
	flush()
	return out
}

// parseKScreenModesField turns "1!  3840x2160@120, 2  1920x1080@60" into
// ([3840x2160@120, 1920x1080@60], "3840x2160@120") where the second
// return is the entry marked with "!" (the currently selected mode).
func parseKScreenModesField(field string) (modes []string, current string, modeIDs map[string]string) {
	modeIDs = map[string]string{}
	liveWithIDRE := regexp.MustCompile(`\b([0-9]+):([0-9]+x[0-9]+@[0-9.]+)([!*]?)`)
	for _, m := range liveWithIDRE.FindAllStringSubmatch(field, -1) {
		modeID := m[1]
		mode := m[2]
		modes = append(modes, mode)
		if _, ok := modeIDs[mode]; !ok {
			modeIDs[mode] = modeID
		}
		if strings.Contains(m[3], "*") {
			current = mode
		}
	}
	if len(modes) > 0 {
		return modes, current, modeIDs
	}
	for _, raw := range strings.Split(field, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// Strip the "<id>[!]  " prefix when present.
		parts := strings.Fields(s)
		if len(parts) == 0 {
			continue
		}
		head := parts[0]
		modeStr := s
		modeID := strings.TrimSuffix(strings.TrimSuffix(head, "!"), "*")
		if len(parts) >= 2 {
			modeStr = parts[1]
		}
		if strings.HasSuffix(head, "!") || strings.HasSuffix(head, "*") || strings.HasSuffix(modeStr, "*") {
			modeStr = strings.TrimSuffix(modeStr, "*")
			modeStr = strings.TrimSuffix(modeStr, "!")
			current = modeStr
		}
		modeStr = strings.TrimSuffix(strings.TrimSuffix(modeStr, "*"), "!")
		modes = append(modes, modeStr)
		if _, ok := modeIDs[modeStr]; !ok && modeID != "" {
			modeIDs[modeStr] = modeID
		}
	}
	return modes, current, modeIDs
}

// FormatMode returns the canonical "<W>x<H>@<refresh>" string from
// the profile's Display block.
func FormatMode(width, height, refresh int) string {
	if width <= 0 || height <= 0 || refresh <= 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d@%d", width, height, refresh)
}

// ParseResolution parses "3840x2160" into (3840, 2160).
func ParseResolution(s string) (width, height int) {
	parts := strings.SplitN(strings.TrimSpace(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

// DRMDisplayValidate is the fifth Milestone 4A phase. After
// kwin-session brings up the compositor, this phase queries
// `kscreen-doctor -o` (running as the headless user) and asserts
// the forced connector is enabled + advertises the configured mode.
// HDR-side validation is recorded if visible but does NOT hard-fail
// the phase yet - that is Milestone 4B/5.
type DRMDisplayValidate struct {
	// KScreenFn lets tests inject the kscreen-doctor invocation.
	// nil = `sudo -u <user> XDG_RUNTIME_DIR=/run/user/<uid>
	//        kscreen-doctor -o` via the runner.
	KScreenFn func(ctx context.Context, deps *Deps, user, uid string) (string, error)

	// KWinDBusFn is the preflight check: when KWin is half-initialized
	// kscreen-doctor hangs instead of returning an error, which made
	// the phase look "stuck" on the live VM. We probe org.kde.KWin's
	// DBus name first and bail out cleanly if it's missing. nil =
	// real `qdbus --session org.kde.KWin /KWin Introspect`.
	KWinDBusFn func(ctx context.Context, deps *Deps, user, uid string) (bool, string, error)

	// KScreenApplyFn lets tests inject kscreen-doctor repair
	// attempts. nil = `kscreen-doctor output.N...` via runner.
	KScreenApplyFn func(ctx context.Context, deps *Deps, user, uid string, args []string) (string, error)

	// WaylandSocketFn overrides the socket presence check.
	WaylandSocketFn func(uid string) bool

	// SysfsRoot overrides /sys for tests.
	SysfsRoot string

	// KernelLogFn returns recent kernel log text for warning
	// classification. nil = best-effort dmesg/journalctl via runner.
	KernelLogFn func(ctx context.Context, deps *Deps) string
}

// Name implements Phase.
func (DRMDisplayValidate) Name() string { return DRMDisplayValidateName }

// Run implements Phase.
func (p DRMDisplayValidate) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if shouldSkip(deps.State, DRMDisplayValidateName) {
		log.Info("phase drm-display-validate: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(DRMDisplayValidateName)
	_ = deps.PersistState()

	desk := deps.Profile.EffectiveDesktop()
	uid := uidFromState(deps)
	connector := ""
	wantMode := ""
	if deps.Profile != nil {
		connector = strings.TrimSpace(deps.Profile.Display.ForcedConnector)
		w, h := ParseResolution(deps.Profile.Display.Resolution)
		wantMode = FormatMode(w, h, deps.Profile.Display.Refresh)
	}
	details := map[string]any{
		"user":              desk.User,
		"uid":               uid,
		"connector":         connector,
		"expected_mode":     wantMode,
		"wayland_socket_ok": uid != "" && p.waylandSocketExists(uid),
		"kscreen_doctor_ok": false,
		"enabled":           false,
		"selected_mode":     "",
		"modes":             []string{},
	}
	unitName := UnitNameForSession(desk.SessionBackend, desk.CompositorMode)
	details["service_name"] = unitName

	if uid == "" {
		err := fmt.Errorf("drm-display-validate needs uid (from headless_user); uid=%q", uid)
		details["err"] = err.Error()
		deps.State.MarkFailed(DRMDisplayValidateName, "missing inputs", err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}

	// Wait briefly for the wayland socket if kwin-session just
	// started.
	if !p.waylandSocketExists(uid) && !deps.DryRun {
		log.Info("phase drm-display-validate: waiting for wayland socket",
			"socket", waylandSocketPath(uid))
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && !p.waylandSocketExists(uid) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	details["wayland_socket_ok"] = p.waylandSocketExists(uid)
	sysfsConnectors, sysfsNames, sysfsErr := p.discoverSysfsConnectors()
	details["sysfs_class_drm_entries"] = sysfsNames
	if sysfsErr != nil {
		details["sysfs_discovery_error"] = sysfsErr.Error()
	}
	details["sysfs_connectors"] = sysfsConnectors
	kernelWarnings := p.kernelDisplayWarnings(ctx, deps)
	if len(kernelWarnings) > 0 {
		details["kernel_display_warnings"] = kernelWarnings
		for _, w := range kernelWarnings {
			if w == DRMWarnNvidiaDumbCreateAlloc {
				details["nvidia_dumb_create_alloc_warning"] = true
			}
		}
	}

	// Preflight: confirm org.kde.KWin owns the session bus name
	// BEFORE asking kscreen-doctor to talk to it. Live VM (2026-05-21)
	// regression: kscreen-doctor hung indefinitely on a host where
	// the wayland-0 socket existed but KWin was still finishing
	// startup (or had picked the nested-Wayland backend). Failing
	// fast with a clean diagnostic is much friendlier than a
	// 15-minute hang.
	if !deps.DryRun {
		ok, info, derr := p.kwinDBusReady(ctx, deps, desk.User, uid)
		details["dbus_kwin_ok"] = ok
		if info != "" {
			details["dbus_kwin_info"] = info
		}
		if !ok {
			err := fmt.Errorf("org.kde.KWin is not present on %s's session bus; "+
				"kscreen-doctor would hang. Recover with: "+
				"sudo systemctl status %s; "+
				"sudo journalctl -u %s -n 200 --no-pager. "+
				"(probe error: %v info=%q)",
				desk.User, unitName, unitName, derr, info)
			details["err"] = err.Error()
			deps.State.MarkFailed(DRMDisplayValidateName, "org.kde.KWin missing on session bus", err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		}
	}

	rawOut, err := p.runKScreen(ctx, deps, desk.User, uid)
	if err != nil {
		details["kscreen_err"] = err.Error()
		details["kscreen_raw_excerpt"] = lastLines(rawOut, 12)
		deps.State.MarkFailed(DRMDisplayValidateName, "kscreen-doctor failed", err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}
	details["kscreen_doctor_ok"] = true
	details["kscreen_raw_bytes"] = len(rawOut)

	parsed := ParseKScreenDoctor(rawOut)
	details["kscreen_connector_names"] = connectorNames(parsed)
	connector, conn, sys := selectDisplayConnector(connector, parsed, sysfsConnectors, wantMode)
	details["connector"] = connector
	details["selected_connector"] = connector
	if sys != nil {
		details["selected_sysfs_connector"] = *sys
	}
	if conn == nil && sys == nil {
		err := fmt.Errorf("no candidate DRM connector found for requested=%q (kscreen=%v sysfs=%v)", connector, connectorNames(parsed), sysfsConnectorNames(sysfsConnectors))
		details["err"] = err.Error()
		details["drm_error_category"] = DRMFailNoCandidateConnector
		deps.State.MarkFailed(DRMDisplayValidateName, DRMFailNoCandidateConnector, err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}

	attempts := []map[string]any{}
	repairRejected := false
	displayHDR := deps.Profile != nil && deps.Profile.Display.HDR
	if conn != nil && (!conn.Enabled || (wantMode != "" && !conn.HasMode(wantMode)) || (displayHDR && (!conn.HDR || !conn.WCG))) {
		targets := displayRepairTargets(conn, wantMode, displayHDR)
		for _, target := range targets {
			args, ok := kscreenApplyArgs(*conn, target)
			if !ok {
				continue
			}
			out, applyErr := p.runKScreenApply(ctx, deps, desk.User, uid, args)
			attempt := map[string]any{
				"connector": connector,
				"target":    target,
				"args":      args,
				"ok":        applyErr == nil,
				"output":    lastLines(out, 8),
			}
			if applyErr != nil {
				attempt["err"] = applyErr.Error()
				if strings.Contains(strings.ToLower(out+" "+applyErr.Error()), "rejected") {
					repairRejected = true
					attempt["category"] = DRMFailKScreenRejectedOutputConfig
				}
			}
			attempts = append(attempts, attempt)
			sysfsConnectors, sysfsNames, _ = p.discoverSysfsConnectors()
			details["sysfs_class_drm_entries_after_repair"] = sysfsNames
			sys = findSysfsConnector(sysfsConnectors, connector)
			details["sysfs_connectors_after_repair"] = sysfsConnectors
			if sys != nil {
				details["selected_sysfs_connector"] = *sys
			}
			rawAfter, rerunErr := p.runKScreen(ctx, deps, desk.User, uid)
			if rerunErr == nil {
				parsed = ParseKScreenDoctor(rawAfter)
				if updated := parsed.FindConnector(connector); updated != nil {
					conn = updated
					details["kscreen_after_repair_excerpt"] = lastLines(rawAfter, 20)
					if conn.Enabled && (wantMode == "" || conn.HasMode(wantMode) || target.Mode != "") {
						break
					}
				}
			}
		}
	}
	if len(attempts) > 0 {
		details["kscreen_repair_attempts"] = attempts
	}
	if repairRejected {
		details["kscreen_rejected_output_config"] = true
	}
	if conn == nil {
		stripped := StripANSI(rawOut)
		mentionsConnector := connector != "" && strings.Contains(stripped, connector)
		details["kscreen_raw_excerpt"] = lastLines(rawOut, 20)
		details["kscreen_stripped_excerpt"] = lastLines(stripped, 20)
		details["kscreen_raw_mentions_connector"] = mentionsConnector
		if sys != nil && p.mixedSourceUsabilityOK(details, sys, wantMode) {
			details["enabled"] = false
			details["selected_mode"] = ""
			details["modes"] = sys.Modes
			details["verification_method"] = DRMVerifiedSysfsWayland
			details["drm_error_category"] = DRMVerifiedSysfsWayland
			deps.State.MarkDone(DRMDisplayValidateName, details)
			_ = deps.PersistState()
			log.Info("phase drm-display-validate: done via sysfs+wayland proof",
				"connector", connector, "expected_mode", wantMode)
			return nil
		}
		if mentionsConnector {
			err := fmt.Errorf("kscreen-doctor output contained %q but parser failed; possible format/color issue", connector)
			details["err"] = err.Error()
			deps.State.MarkFailed(DRMDisplayValidateName, "kscreen-doctor parser failed despite connector in raw output", err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		}
		category := DRMFailNoCandidateConnector
		if sys != nil {
			category = DRMFailKScreenSysfsStateMismatch
			if sys.IsEnabled {
				category = DRMFailSysfsEnabledKScreenDisabled
			}
			err := fmt.Errorf("kscreen-doctor did not report selected connector %q and usable output could not be proven (kscreen=%v sysfs=%+v)", connector, connectorNames(parsed), sys)
			details["err"] = err.Error()
			details["drm_error_category"] = category
			deps.State.MarkFailed(DRMDisplayValidateName, category, err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		} else {
			err := fmt.Errorf("kscreen-doctor did not report selected connector %q and usable output could not be proven (kscreen=%v sysfs=<nil> discovered=%s)", connector, connectorNames(parsed), formatDiscoveredSysfs(sysfsConnectors))
			details["err"] = err.Error()
			details["drm_error_category"] = category
			deps.State.MarkFailed(DRMDisplayValidateName, category, err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		}
	}
	details["enabled"] = conn.Enabled
	details["selected_mode"] = conn.CurrentMode
	details["modes"] = conn.Modes
	details["hdr_enabled"] = conn.HDR
	details["wcg_enabled"] = conn.WCG
	details["kscreen_output_id"] = conn.ID
	if !conn.Enabled {
		if p.mixedSourceUsabilityOK(details, sys, wantMode) {
			details["drm_error_category"] = DRMVerifiedSysfsWayland
			details["verification_method"] = DRMVerifiedSysfsWayland
		} else {
			var err error
			category := DRMFailKScreenSysfsStateMismatch
			if sys != nil {
				if sys.IsEnabled {
					category = DRMFailSysfsEnabledKScreenDisabled
				}
				err = fmt.Errorf("connector %q is disabled in kscreen-doctor and usable output could not be proven (sysfs=%+v)", connector, sys)
			} else {
				err = fmt.Errorf("connector %q is disabled in kscreen-doctor and usable output could not be proven (sysfs=<nil> discovered=%s)", connector, formatDiscoveredSysfs(sysfsConnectors))
			}
			details["err"] = err.Error()
			details["drm_error_category"] = category
			deps.State.MarkFailed(DRMDisplayValidateName, category, err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		}
	}
	if wantMode != "" && !conn.HasMode(wantMode) {
		if deps.Profile != nil {
			if target, ok := allowedDisplayFallback(deps.Profile.Display, conn.Modes); ok {
				details["display_target_original"] = displayTargetName(deps.Profile.Display.HDR, wantMode)
				details["display_target_effective"] = displayTargetName(target.HDR, target.Mode)
				details["fallback_reason"] = target.Reason
				details["drm_error_category"] = DRMDisplayTargetFallbackApplied
				wantMode = target.Mode
			} else {
				err := fmt.Errorf("connector %q does not advertise expected mode %q (saw modes %v)", connector, wantMode, conn.Modes)
				details["err"] = err.Error()
				details["drm_error_category"] = DRMFailExpectedModeMissing
				deps.State.MarkFailed(DRMDisplayValidateName, DRMFailExpectedModeMissing, err, true)
				deps.State.Get(DRMDisplayValidateName).Details = details
				_ = deps.PersistState()
				return fmt.Errorf("phase drm-display-validate: %w", err)
			}
		} else {
			err := fmt.Errorf("connector %q does not advertise expected mode %q (saw modes %v)", connector, wantMode, conn.Modes)
			details["err"] = err.Error()
			details["drm_error_category"] = DRMFailExpectedModeMissing
			deps.State.MarkFailed(DRMDisplayValidateName, DRMFailExpectedModeMissing, err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
		}
	}
	if deps.Profile != nil && deps.Profile.Display.HDR {
		if !conn.HDR || !conn.WCG {
			if deps.Profile.Display.AllowFallback && deps.Profile.Display.AllowSDRFallback {
				details["display_target_original"] = displayTargetName(true, wantMode)
				details["display_target_effective"] = displayTargetName(false, wantMode)
				details["fallback_reason"] = "kscreen_rejected_config"
				details["drm_error_category"] = DRMDisplayTargetFallbackApplied
			} else if details["verification_method"] == DRMVerifiedSysfsWayland {
				details["hdr_wcg_unverified_warning"] = true
			} else {
				err := fmt.Errorf("HDR profile requires kscreen-doctor to report HDR enabled and Wide Color Gamut enabled on %q (hdr=%v wcg=%v)",
					connector, conn.HDR, conn.WCG)
				details["err"] = err.Error()
				details["drm_error_category"] = DRMFailDisplayTargetStrictUnavailable
				deps.State.MarkFailed(DRMDisplayValidateName, DRMFailDisplayTargetStrictUnavailable, err, true)
				deps.State.Get(DRMDisplayValidateName).Details = details
				_ = deps.PersistState()
				return fmt.Errorf("phase drm-display-validate: %w", err)
			}
		}
	}

	deps.State.MarkDone(DRMDisplayValidateName, details)
	_ = deps.PersistState()
	log.Info("phase drm-display-validate: done",
		"connector", connector, "enabled", conn.Enabled,
		"selected_mode", conn.CurrentMode, "expected_mode", wantMode,
		"mode_count", len(conn.Modes))
	return nil
}

// kwinDBusReady probes the headless user's session bus for
// org.kde.KWin. Returns (true, info, nil) when the service answers.
// Tests inject KWinDBusFn; production shells out to `qdbus --session
// org.kde.KWin /KWin Introspect`.
func (p DRMDisplayValidate) kwinDBusReady(ctx context.Context, deps *Deps, user, uid string) (bool, string, error) {
	if p.KWinDBusFn != nil {
		return p.KWinDBusFn(ctx, deps, user, uid)
	}
	qdbus, qerr := resolveQDBus()
	if qerr != nil {
		return false, "", qerr
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"sudo", "-u", user,
			"env",
			"XDG_RUNTIME_DIR=/run/user/" + uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
			qdbus.Path, "--session", "org.kde.KWin", "/KWin",
			"org.freedesktop.DBus.Introspectable.Introspect",
		},
		Sudo:    true,
		LogFile: "-",
		Timeout: 8 * time.Second,
	})
	if res.Err != nil {
		return false, lastLines(res.Stderr, 2), res.Err
	}
	return true, "introspect OK via " + qdbus.Path, nil
}

func (p DRMDisplayValidate) runKScreen(ctx context.Context, deps *Deps, user, uid string) (string, error) {
	if p.KScreenFn != nil {
		return p.KScreenFn(ctx, deps, user, uid)
	}
	if deps.DryRun {
		// Synthesize a minimal happy-path output so apply --dry-run
		// can show the phase reaching done.
		conn := "DP-1"
		if deps.Profile != nil && deps.Profile.Display.ForcedConnector != "" {
			conn = deps.Profile.Display.ForcedConnector
		}
		mode := "3840x2160@120"
		w, h := ParseResolution(deps.Profile.Display.Resolution)
		if m := FormatMode(w, h, deps.Profile.Display.Refresh); m != "" {
			mode = m
		}
		extra := ""
		if deps.Profile != nil && deps.Profile.Display.HDR {
			extra = "        HDR: enabled\n        Wide Color Gamut: enabled\n"
		}
		return fmt.Sprintf("Output: 1 %s\n        enabled\n        Modes: 1:%s*\n%s", conn, mode, extra), nil
	}
	// Live VM regression: kscreen-doctor defaulted to Qt xcb without
	// an explicit QT_QPA_PLATFORM=wayland, which then crashed because
	// the headless host has no X server. Set the full env the KDE
	// shell expects so kscreen-doctor is forced through the Wayland
	// platform plugin.
	kscreen, kerr := resolveKScreenDoctor()
	if kerr != nil {
		return "", fmt.Errorf("missing kscreen-doctor: %w", kerr)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{
			"sudo", "-u", user,
			"env",
			"XDG_RUNTIME_DIR=/run/user/" + uid,
			"WAYLAND_DISPLAY=wayland-0",
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
			"QT_QPA_PLATFORM=wayland",
			"XDG_CURRENT_DESKTOP=KDE",
			"XDG_SESSION_TYPE=wayland",
			"XDG_SESSION_DESKTOP=KDE",
			"KDE_FULL_SESSION=true",
			// Belt-and-suspenders color disabling so kscreen-doctor
			// doesn't ship ANSI escapes the parser then has to
			// strip. NO_COLOR is the cross-tool standard
			// (https://no-color.org); TERM=dumb covers tools that
			// only check terminfo; CLICOLOR=0 covers the BSD/macOS
			// convention some upstream code paths still honor.
			// The parser also calls StripANSI() as a safety net,
			// so missing one of these is still recoverable.
			"NO_COLOR=1",
			"TERM=dumb",
			"CLICOLOR=0",
			kscreen.Path, "-o",
		},
		Sudo:    true,
		Timeout: 15 * time.Second,
	})
	if res.Err != nil {
		return res.Stdout, fmt.Errorf("kscreen-doctor: %w (stderr=%q)",
			res.Err, lastLines(res.Stderr, 5))
	}
	return res.Stdout, nil
}

func (p DRMDisplayValidate) runKScreenApply(ctx context.Context, deps *Deps, user, uid string, args []string) (string, error) {
	if p.KScreenApplyFn != nil {
		return p.KScreenApplyFn(ctx, deps, user, uid, args)
	}
	if deps.DryRun {
		return "dry-run kscreen apply", nil
	}
	kscreen, kerr := resolveKScreenDoctor()
	if kerr != nil {
		return "", fmt.Errorf("missing kscreen-doctor: %w", kerr)
	}
	argv := []string{
		"sudo", "-u", user,
		"env",
		"XDG_RUNTIME_DIR=/run/user/" + uid,
		"WAYLAND_DISPLAY=wayland-0",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus",
		"QT_QPA_PLATFORM=wayland",
		"XDG_CURRENT_DESKTOP=KDE",
		"XDG_SESSION_TYPE=wayland",
		"NO_COLOR=1",
		"TERM=dumb",
		"CLICOLOR=0",
		kscreen.Path,
	}
	argv = append(argv, args...)
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    argv,
		Sudo:    true,
		Timeout: 20 * time.Second,
	})
	if res.Err != nil {
		return res.Stdout, fmt.Errorf("kscreen-doctor apply: %w (stderr=%q)", res.Err, lastLines(res.Stderr, 5))
	}
	return res.Stdout, nil
}

func (p DRMDisplayValidate) waylandSocketExists(uid string) bool {
	if p.WaylandSocketFn != nil {
		return p.WaylandSocketFn(uid)
	}
	return waylandSocketExists(uid)
}

func (p DRMDisplayValidate) discoverSysfsConnectors() ([]DRMConnectorState, []string, error) {
	root := p.SysfsRoot
	if root == "" {
		root = "/sys"
	}
	base := filepath.Join(root, "class", "drm")
	entries, err := os.ReadDir(base)
	var out []DRMConnectorState = make([]DRMConnectorState, 0)
	var allNames []string = make([]string, 0)
	if err != nil {
		return out, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		allNames = append(allNames, name)
		if strings.HasPrefix(name, "card") && strings.Contains(name, "-") {
			full := filepath.Join(base, name)
			var cardName, connName string
			parts := strings.SplitN(name, "-", 2)
			if len(parts) != 2 {
				continue
			}
			cardName = parts[0]
			connName = parts[1]
			st := DRMConnectorState{
				Card:          cardName,
				Name:          connName,
				Path:          full,
				SysfsBasename: name,
				Status:        strings.TrimSpace(readFileString(filepath.Join(full, "status"))),
				Enabled:       strings.TrimSpace(readFileString(filepath.Join(full, "enabled"))),
				Modes:         readLines(filepath.Join(full, "modes")),
				NVIDIA:        sysfsConnectorIsNVIDIA(full),
			}
			st.Connected = st.Status == "connected"
			st.IsEnabled = st.Enabled == "enabled"
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NVIDIA != out[j].NVIDIA {
			return out[i].NVIDIA
		}
		if out[i].Connected != out[j].Connected {
			return out[i].Connected
		}
		if out[i].IsEnabled != out[j].IsEnabled {
			return out[i].IsEnabled
		}
		return out[i].Name < out[j].Name
	})
	return out, allNames, nil
}

func sysfsConnectorIsNVIDIA(connectorPath string) bool {
	vendor := readFileString(filepath.Join(connectorPath, "device", "vendor"))
	if vendor == "" {
		// Connector dirs are often card0-DP-1 with device symlink at
		// ../card0/device rather than connector/device.
		cardName := strings.SplitN(filepath.Base(connectorPath), "-", 2)[0]
		vendor = readFileString(filepath.Join(filepath.Dir(connectorPath), cardName, "device", "vendor"))
	}
	return strings.Contains(strings.ToLower(vendor), "0x10de")
}

func readFileString(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (p DRMDisplayValidate) kernelDisplayWarnings(ctx context.Context, deps *Deps) []string {
	var text string
	if p.KernelLogFn != nil {
		text = p.KernelLogFn(ctx, deps)
	} else if deps != nil && deps.Runner != nil && !deps.DryRun {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"bash", "-lc", "dmesg 2>/dev/null | tail -n 300 || journalctl -k -n 300 --no-pager 2>/dev/null || true"},
			LogFile: "-",
			Timeout: 10 * time.Second,
		})
		text = res.Stdout
	}
	var warnings []string
	if strings.Contains(text, "Failed to allocate NvKmsKapiMemory for dumb object") {
		warnings = append(warnings, DRMWarnNvidiaDumbCreateAlloc)
	}
	return warnings
}

func selectDisplayConnector(requested string, parsed KScreenDoctorOutput, sysfs []DRMConnectorState, wantMode string) (string, *KScreenConnector, *DRMConnectorState) {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested, parsed.FindConnector(requested), findSysfsConnector(sysfs, requested)
	}
	names := map[string]bool{}
	for _, s := range sysfs {
		if !s.Connected {
			continue
		}
		names[s.Name] = true
	}
	for _, c := range parsed.Connectors {
		names[c.Name] = true
	}
	var candidates []string
	for name := range names {
		candidates = append(candidates, name)
	}
	sort.Slice(candidates, func(i, j int) bool {
		si := findSysfsConnector(sysfs, candidates[i])
		sj := findSysfsConnector(sysfs, candidates[j])
		score := func(s *DRMConnectorState, c *KScreenConnector) int {
			n := 0
			if s != nil && s.NVIDIA {
				n += 100
			}
			if s != nil && s.Connected {
				n += 50
			}
			if s != nil && s.IsEnabled {
				n += 25
			}
			if c != nil && c.Enabled {
				n += 20
			}
			if wantMode != "" {
				if c != nil && c.HasMode(wantMode) {
					n += 10
				}
				if s != nil && modeListHasKScreenMode(s.Modes, wantMode) {
					n += 10
				}
			}
			return n
		}
		return score(si, parsed.FindConnector(candidates[i])) > score(sj, parsed.FindConnector(candidates[j]))
	})
	if len(candidates) == 0 {
		return "", nil, nil
	}
	name := candidates[0]
	return name, parsed.FindConnector(name), findSysfsConnector(sysfs, name)
}

func findSysfsConnector(list []DRMConnectorState, name string) *DRMConnectorState {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func sysfsConnectorNames(list []DRMConnectorState) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func formatDiscoveredSysfs(list []DRMConnectorState) string {
	if len(list) == 0 {
		return "[]"
	}
	var out []string
	for _, s := range list {
		modes := s.Modes
		if len(modes) > 3 {
			modesCloned := make([]string, 3)
			copy(modesCloned, modes[:3])
			modes = append(modesCloned, "...")
		}
		modesStr := "[" + strings.Join(modes, " ") + "]"
		out = append(out, fmt.Sprintf("%s normalized=%s status=%s enabled=%s modes=%s", s.SysfsBasename, s.Name, s.Status, s.Enabled, modesStr))
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func modeListHasKScreenMode(modes []string, mode string) bool {
	if mode == "" {
		return true
	}
	wantRes := strings.Split(mode, "@")[0]
	for _, m := range modes {
		if m == mode || m == wantRes {
			return true
		}
	}
	return false
}

func displayRepairTargets(conn *KScreenConnector, wantMode string, wantHDR bool) []displayTarget {
	seen := map[string]bool{}
	var out []displayTarget
	add := func(mode, reason string, hdr bool) {
		if mode == "" || seen[mode+"|"+fmt.Sprint(hdr)] {
			return
		}
		if conn != nil && !conn.HasMode(mode) {
			return
		}
		seen[mode+"|"+fmt.Sprint(hdr)] = true
		out = append(out, displayTarget{Mode: mode, HDR: hdr, Reason: reason})
	}
	if conn != nil {
		add(conn.CurrentMode, "current_preferred", wantHDR)
	}
	add("3840x2160@60", "safe_4k60", wantHDR)
	add("3840x2160@120", "target_4k120", wantHDR)
	add(wantMode, "configured_target", wantHDR)
	return out
}

func kscreenApplyArgs(conn KScreenConnector, target displayTarget) ([]string, bool) {
	if conn.ID == "" || target.Mode == "" {
		return nil, false
	}
	modeID := conn.ModeIDs[target.Mode]
	if modeID == "" {
		return nil, false
	}
	args := []string{
		"output." + conn.ID + ".enable",
		"output." + conn.ID + ".mode." + modeID,
		"output." + conn.ID + ".position.0,0",
		"output." + conn.ID + ".scale.1",
	}
	if target.HDR {
		args = append(args, "output."+conn.ID+".hdr.enable", "output."+conn.ID+".wcg.enable")
	}
	return args, true
}

func allowedDisplayFallback(display config.DisplayConfig, modes []string) (displayTarget, bool) {
	if !display.AllowFallback {
		return displayTarget{}, false
	}
	w, h := ParseResolution(display.Resolution)
	if w <= 0 || h <= 0 {
		return displayTarget{}, false
	}
	if display.AllowLowerRefreshFallback {
		mode := FormatMode(w, h, 60)
		if mode != "" && stringListHas(modes, mode) {
			return displayTarget{Mode: mode, HDR: display.HDR && !display.AllowSDRFallback, Reason: "lower_refresh_fallback"}, true
		}
	}
	if display.AllowSDRFallback {
		mode := FormatMode(w, h, display.Refresh)
		if mode != "" && stringListHas(modes, mode) {
			return displayTarget{Mode: mode, HDR: false, Reason: "sdr_fallback"}, true
		}
	}
	return displayTarget{}, false
}

func stringListHas(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func displayTargetName(hdr bool, mode string) string {
	prefix := "sdr"
	if hdr {
		prefix = "hdr"
	}
	return prefix + "_" + strings.ReplaceAll(mode, "@", "_")
}

func (p DRMDisplayValidate) mixedSourceUsabilityOK(details map[string]any, sys *DRMConnectorState, wantMode string) bool {
	if sys == nil || !sys.Connected || !sys.IsEnabled {
		return false
	}
	if wantMode != "" && !modeListHasKScreenMode(sys.Modes, wantMode) {
		return false
	}
	if fmt.Sprint(details["wayland_socket_ok"]) != "true" {
		return false
	}
	if fmt.Sprint(details["dbus_kwin_ok"]) != "true" {
		return false
	}
	return true
}

func waylandSocketExists(uid string) bool {
	if uid == "" {
		return false
	}
	_, err := os.Stat(waylandSocketPath(uid))
	return err == nil
}

func connectorNames(o KScreenDoctorOutput) []string {
	out := make([]string, 0, len(o.Connectors))
	for _, c := range o.Connectors {
		out = append(out, c.Name)
	}
	return out
}
