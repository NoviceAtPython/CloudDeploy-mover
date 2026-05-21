package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// DRMDisplayValidateName is the canonical state-key.
const DRMDisplayValidateName = "drm_display_validate"

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
	// Name is the connector name (e.g. "DP-1", "HDMI-A-1").
	Name string
	// Enabled is the boolean kscreen-doctor reports.
	Enabled bool
	// CurrentMode is the active mode, "" when disabled.
	CurrentMode string
	// Modes is every mode kscreen-doctor enumerated under this
	// connector, in the same order it printed them.
	Modes []string
	HDR   bool
	WCG   bool
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
var kscreenOutputRE = regexp.MustCompile(`^Output:\s+\S+\s+(\S+)`)

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
			cur = &KScreenConnector{Name: m[1]}
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
			cur.Modes, cur.CurrentMode = parseKScreenModesField(m[1])
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
func parseKScreenModesField(field string) (modes []string, current string) {
	liveRE := regexp.MustCompile(`\b[0-9]+:([0-9]+x[0-9]+@[0-9.]+)([!*]?)`)
	for _, m := range liveRE.FindAllStringSubmatch(field, -1) {
		mode := m[1]
		modes = append(modes, mode)
		if strings.Contains(m[2], "*") {
			current = mode
		}
	}
	if len(modes) > 0 {
		return modes, current
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
	}
	return modes, current
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
		"wayland_socket_ok": uid != "" && waylandSocketExists(uid),
		"kscreen_doctor_ok": false,
		"enabled":           false,
		"selected_mode":     "",
		"modes":             []string{},
	}
	unitName := UnitNameForSession(desk.SessionBackend, desk.CompositorMode)
	details["service_name"] = unitName

	if uid == "" || connector == "" {
		err := fmt.Errorf("drm-display-validate needs uid (from headless_user) and display.forced_connector to be set; uid=%q connector=%q", uid, connector)
		details["err"] = err.Error()
		deps.State.MarkFailed(DRMDisplayValidateName, "missing inputs", err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}

	// Wait briefly for the wayland socket if kwin-session just
	// started.
	if !waylandSocketExists(uid) && !deps.DryRun {
		log.Info("phase drm-display-validate: waiting for wayland socket",
			"socket", waylandSocketPath(uid))
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && !waylandSocketExists(uid) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	details["wayland_socket_ok"] = waylandSocketExists(uid)

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
	conn := parsed.FindConnector(connector)
	if conn == nil {
		// Live VM (2026-05-21): the parser would report
		// `saw []` even when the raw kscreen output clearly
		// contained DP-1 + HDR + WCG -- the culprit was ANSI
		// color escapes. The parser now strips them, but if
		// we ever land in a new parser-mismatch regression
		// (newer kscreen output format), call it out as a
		// PARSER bug rather than an absent-connector bug so
		// the operator doesn't go looking for a missing
		// monitor that's actually present.
		stripped := StripANSI(rawOut)
		mentionsConnector := strings.Contains(stripped, connector)
		details["kscreen_connector_names"] = connectorNames(parsed)
		details["kscreen_raw_excerpt"] = lastLines(rawOut, 20)
		details["kscreen_stripped_excerpt"] = lastLines(stripped, 20)
		details["kscreen_raw_mentions_connector"] = mentionsConnector
		var err error
		if mentionsConnector {
			err = fmt.Errorf(
				"kscreen-doctor output contained %q but parser failed; possible format/color issue. "+
					"Raw bytes=%d. supportInformation already confirmed DRM + connector earlier, "+
					"so this is a parser regression, not an absent-monitor regression. "+
					"Capture: sudo -u %s env XDG_RUNTIME_DIR=/run/user/%s WAYLAND_DISPLAY=wayland-0 NO_COLOR=1 TERM=dumb kscreen-doctor -o",
				connector, len(rawOut), desk.User, uid)
			deps.State.MarkFailed(DRMDisplayValidateName, "kscreen-doctor parser failed despite connector in raw output", err, true)
		} else {
			err = fmt.Errorf("kscreen-doctor reports no connector named %q (saw %v)", connector, connectorNames(parsed))
			deps.State.MarkFailed(DRMDisplayValidateName, "connector missing in kscreen-doctor output", err, true)
		}
		details["err"] = err.Error()
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}
	details["enabled"] = conn.Enabled
	details["selected_mode"] = conn.CurrentMode
	details["modes"] = conn.Modes
	details["hdr_enabled"] = conn.HDR
	details["wcg_enabled"] = conn.WCG
	if !conn.Enabled {
		err := fmt.Errorf("connector %q exists but is disabled in kscreen-doctor output", connector)
		details["err"] = err.Error()
		deps.State.MarkFailed(DRMDisplayValidateName, "connector disabled", err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}
	if wantMode != "" && !conn.HasMode(wantMode) {
		err := fmt.Errorf("connector %q does not advertise expected mode %q (saw modes %v)", connector, wantMode, conn.Modes)
		details["err"] = err.Error()
		deps.State.MarkFailed(DRMDisplayValidateName, "expected mode missing", err, true)
		deps.State.Get(DRMDisplayValidateName).Details = details
		_ = deps.PersistState()
		return fmt.Errorf("phase drm-display-validate: %w", err)
	}
	if deps.Profile != nil && deps.Profile.Display.HDR {
		if !conn.HDR || !conn.WCG {
			err := fmt.Errorf("HDR profile requires kscreen-doctor to report HDR enabled and Wide Color Gamut enabled on %q (hdr=%v wcg=%v)",
				connector, conn.HDR, conn.WCG)
			details["err"] = err.Error()
			deps.State.MarkFailed(DRMDisplayValidateName, "HDR/WCG not enabled", err, true)
			deps.State.Get(DRMDisplayValidateName).Details = details
			_ = deps.PersistState()
			return fmt.Errorf("phase drm-display-validate: %w", err)
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
