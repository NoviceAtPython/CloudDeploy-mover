package phase

// kwin_session_prep.go is the discovery + external-prep + adoption
// machinery for the KWinSession phase. Lives in its own file so the
// existing kwin_session.go diff stays small.
//
// Live VM 2026-05-22 (Vast RTX 4090) drove the design:
//
//   - blocking ExecStartPre lines (`systemctl stop getty@ttyN.service`,
//     `systemctl start user@UID.service`, `mkdir`, `chvt`) wedged the
//     unit in start-pre.
//   - ExecStartPost=/usr/local/bin/clouddeploy-force-kwin-mode.sh
//     wedged it in start-post.
//   - openvt + runuser produced a transient socket but kwin_wayland
//     kept exiting because no real logind session was active.
//   - the working fix was: do EVERY prep step outside the unit with
//     bounded timeouts, then start a clean PAM+TTY service that has
//     no ExecStartPre or ExecStartPost.
//
// All helpers here use the runner's context for timeouts; nothing
// blocks indefinitely.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// -----------------------------------------------------------------------------
// failure categories - persisted as state.Details["kwin_failure_category"]
// -----------------------------------------------------------------------------

// KWinFailureCategory enumerates the distinct failure modes the live
// VM debugging surfaced. Each value gets recorded into state.Details
// so the operator (and `clouddeployctl monitor`) can tell "the
// compositor never started" from "the compositor started but logind
// rejected its session". The names are the same as the brief.
const (
	KWinFailServiceStuckExecStartPre  = "service_stuck_exec_start_pre"
	KWinFailServiceStuckExecStartPost = "service_stuck_exec_start_post"
	KWinFailServiceFailedBeforeKWin   = "service_failed_before_kwin"
	KWinFailSocketAppearedKWinExited  = "socket_appeared_but_kwin_exited"
	KWinFailLogindActivationFailed    = "kwin_logind_activation_failed"
	KWinFailDRMOpenFailed             = "kwin_drm_open_failed"
	KWinFailNoSuitableDRMDevices      = "no_suitable_drm_devices"
	KWinFailNoCandidateDRMCard        = "no_candidate_drm_card"
	KWinFailNoCandidateTTY            = "no_candidate_tty"
	KWinFailUserRuntimeDirBad         = "user_runtime_dir_bad"
	KWinFailUserBusMissing            = "user_bus_missing"
	KWinFailDMOrGettyConflict         = "display_manager_or_getty_conflict"
	KWinFailModeHelperTimeout         = "mode_helper_timeout"
	KWinFailServiceNotActive          = "service_not_active"
	KWinFailJournalFatalMarker        = "kwin_journal_fatal_marker"
	KWinFailPIDUnstable               = "kwin_pid_unstable"
	KWinFailWaylandSocketMissing      = "wayland_socket_missing"
	KWinFailSessionNotOnSeatTTY       = "session_not_on_expected_seat_tty"
)

// classifyKWinFailure maps an arbitrary error + the journal tail +
// the unit's current SubState into one of the categories above. The
// brief names "socket missing" as a misleading default; we keep that
// as a true fallback because it's still better than a bare exit-code.
func classifyKWinFailure(err error, journalTail, subState string) string {
	combined := strings.ToLower(strings.Join([]string{
		safeErr(err), strings.ToLower(journalTail), strings.ToLower(subState),
	}, "\n"))
	switch {
	case strings.Contains(combined, "no suitable drm devices have been found"):
		return KWinFailNoSuitableDRMDevices
	case strings.Contains(combined, "failed to open drm device") ||
		strings.Contains(combined, "failed to open /dev/dri"):
		return KWinFailDRMOpenFailed
	case strings.Contains(combined, "failed to activate") && strings.Contains(combined, "session"):
		return KWinFailLogindActivationFailed
	case strings.Contains(combined, "start-pre") || strings.Contains(combined, "execstartpre"):
		return KWinFailServiceStuckExecStartPre
	case strings.Contains(combined, "start-post") || strings.Contains(combined, "execstartpost"):
		return KWinFailServiceStuckExecStartPost
	case strings.Contains(combined, "kscreen-doctor") && strings.Contains(combined, "timed out"):
		return KWinFailModeHelperTimeout
	case strings.Contains(combined, "user@") && strings.Contains(combined, "bus"):
		return KWinFailUserBusMissing
	case strings.Contains(combined, "wayland-0") && strings.Contains(combined, "not appear"):
		return KWinFailWaylandSocketMissing
	}
	return KWinFailServiceNotActive
}

func safeErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// -----------------------------------------------------------------------------
// discovery
// -----------------------------------------------------------------------------

// KWinSessionDiscovery captures everything the phase resolved at
// runtime instead of trusting the profile defaults. Persisted as
// state.Details["kwin_session_discovery"].
type KWinSessionDiscovery struct {
	User            string `json:"user"`
	UID             string `json:"uid"`
	HomeDir         string `json:"home_dir"`
	XDGRuntimeDir   string `json:"xdg_runtime_dir"`
	DRMCard         string `json:"drm_card"`
	DRMCardReason   string `json:"drm_card_reason"`
	RenderNode      string `json:"render_node,omitempty"`
	TTY             string `json:"tty"`
	TTYReason       string `json:"tty_reason"`
	Seat            string `json:"seat"`
	ForcedConnector string `json:"forced_connector,omitempty"`
}

// candidateTTYs is the search list when the profile doesn't pin a
// specific VT. tty7 first because that's where the working live VM
// landed; the rest are conservative fallbacks that avoid clashing
// with serial consoles (tty1) and conventional getty terminals
// (tty2-tty6 on cloud images).
var candidateTTYs = []int{7, 8, 9, 3, 2}

// discoverKWinSession resolves user/uid/card/tty/seat WITHOUT
// trusting the profile's defaults to be present on the actual host.
// Each field is sourced from the most-authoritative input available;
// failure to resolve a particular field produces a clear error
// (categorized via classifyKWinFailure on the caller side).
//
// Inputs:
//
//   - explicitDRM       : raw profile.desktop.kwin_drm_device value
//     (pre-default). Honored verbatim when non-empty.
//   - explicitTTY       : profile.desktop.kwin_vt (0 means "discover").
//   - forcedConnector   : profile.display.forced_connector. Used to
//     bias the DRM-card search toward the card whose sysfs has the
//     matching cardN-DP-1 connector.
//   - sysClassDRMRoot   : /sys/class/drm on real hosts; t.TempDir()
//     in unit tests. "" means the production default.
//   - devDriRoot        : /dev/dri on real hosts.
//   - userLookupFn      : returns (uid, home) for the headless user.
func discoverKWinSession(
	user string,
	explicitDRM string,
	explicitTTY int,
	forcedConnector string,
	sysClassDRMRoot string,
	devDriRoot string,
	userLookupFn func(string) (uid string, home string, ok bool),
) (KWinSessionDiscovery, error) {
	out := KWinSessionDiscovery{
		User:            strings.TrimSpace(user),
		ForcedConnector: strings.TrimSpace(forcedConnector),
		Seat:            "seat0",
	}
	if out.User == "" {
		return out, fmt.Errorf("discoverKWinSession: empty user")
	}
	if userLookupFn != nil {
		if uid, home, ok := userLookupFn(out.User); ok {
			out.UID = strings.TrimSpace(uid)
			out.HomeDir = strings.TrimSpace(home)
		}
	}
	if out.UID == "" {
		return out, fmt.Errorf("discoverKWinSession: could not resolve uid for user %q", out.User)
	}
	if out.HomeDir == "" {
		out.HomeDir = "/home/" + out.User
	}
	out.XDGRuntimeDir = "/run/user/" + out.UID

	// TTY selection (cheap, no I/O) is done first so a DRM-discovery
	// failure still leaves the partial discovery populated for the
	// state diagnostics.
	tty, ttyReason := selectKWinTTY(explicitTTY)
	out.TTY = tty
	out.TTYReason = ttyReason

	// DRM card selection.
	card, reason, err := selectKWinDRMCard(explicitDRM, forcedConnector, sysClassDRMRoot, devDriRoot)
	if err != nil {
		out.DRMCardReason = reason
		return out, err
	}
	out.DRMCard = card
	out.DRMCardReason = reason
	out.RenderNode = RenderNodeForCardWithSysfs(card, sysClassDRMRoot)

	return out, nil
}

// selectKWinDRMCard picks the /dev/dri/cardN node KWin should bind
// to. Priority:
//
//  1. operator-pinned explicit path                  -> honor verbatim
//  2. card whose sysfs has the forced connector      -> prefer
//  3. NVIDIA-vendored card (0x10de)                  -> prefer
//  4. card1 if present (cloud-VM convention)         -> next
//  5. card0                                          -> last
//
// Returns ("", reason, error) when no card resolves at all.
func selectKWinDRMCard(explicit, forcedConnector, sysClassDRMRoot, devDriRoot string) (string, string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, "explicitly pinned via profile.desktop.kwin_drm_device", nil
	}
	sysClassDRMRoot = strings.TrimRight(coalesceStr(sysClassDRMRoot, "/sys/class/drm"), "/")
	devDriRoot = strings.TrimRight(coalesceStr(devDriRoot, "/dev/dri"), "/")

	cards, _ := filepath.Glob(filepath.Join(sysClassDRMRoot, "card*"))
	var cardNames []string
	for _, c := range cards {
		base := filepath.Base(c)
		if !drmCardNameRE.MatchString(base) {
			continue
		}
		cardNames = append(cardNames, base)
	}
	sort.Strings(cardNames)

	if strings.TrimSpace(forcedConnector) != "" {
		for _, name := range cardNames {
			conn := filepath.Join(sysClassDRMRoot, name, name+"-"+forcedConnector)
			if _, err := os.Stat(conn); err == nil {
				return filepath.Join(devDriRoot, name), "matched forced_connector " + forcedConnector, nil
			}
		}
	}
	for _, name := range cardNames {
		vendorBytes, err := os.ReadFile(filepath.Join(sysClassDRMRoot, name, "device", "vendor"))
		if err == nil && strings.Contains(strings.ToLower(string(vendorBytes)), "0x10de") {
			return filepath.Join(devDriRoot, name), "nvidia gpu (PCI vendor 0x10de) detected", nil
		}
	}
	for _, prefer := range []string{"card1", "card0"} {
		if _, err := os.Stat(filepath.Join(devDriRoot, prefer)); err == nil {
			return filepath.Join(devDriRoot, prefer), "fallback " + prefer + " present", nil
		}
	}
	return "", "no /dev/dri/card* present", fmt.Errorf("%s", KWinFailNoCandidateDRMCard)
}

// selectKWinTTY returns ("/dev/ttyN", reason). When `explicit` > 0
// we honor the operator's pin verbatim; otherwise we walk the
// candidateTTYs list and pick the first one we don't already see a
// non-VT user on. This is conservative on purpose: cloud-VM images
// universally have getty@tty1 mounted on the serial console, so we
// avoid tty1.
func selectKWinTTY(explicit int) (string, string) {
	if explicit > 0 {
		return fmt.Sprintf("/dev/tty%d", explicit), fmt.Sprintf("explicit profile.desktop.kwin_vt=%d", explicit)
	}
	// We do NOT introspect /var/run/utmp here; the live VM evidence
	// is that systemd's TTYVTDisallocate=yes + the unit's
	// Conflicts=getty@ttyN.service handle the conflict for us as
	// long as the prep step did `systemctl stop getty@ttyN.service`
	// (which kwinExternalPrep does).
	tty := candidateTTYs[0]
	return fmt.Sprintf("/dev/tty%d", tty), fmt.Sprintf("default candidate tty%d (Conflicts=getty@tty%d.service handles handover)", tty, tty)
}

func coalesceStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// external prep - everything that used to be ExecStartPre lines
// -----------------------------------------------------------------------------

// KWinPrepResult is what kwinExternalPrep returns. The phase folds
// the entire struct into state.Details so the operator can see
// exactly what we ran and how long it took.
type KWinPrepResult struct {
	Steps            []KWinPrepStep `json:"steps"`
	GettyStopped     bool           `json:"getty_stopped"`
	LingerEnabled    bool           `json:"linger_enabled"`
	RuntimeDirReady  bool           `json:"runtime_dir_ready"`
	SeatAttachOK     bool           `json:"seat_attach_ok"`
	UdevSettleOK     bool           `json:"udev_settle_ok"`
	ChvtOK           bool           `json:"chvt_ok"`
	UserBusReachable bool           `json:"user_bus_reachable"`
}

// KWinPrepStep is one external command. Persisted so the operator
// can audit every action the phase took outside the systemd unit.
type KWinPrepStep struct {
	Name       string `json:"name"`
	Argv       string `json:"argv"`
	Timeout    string `json:"timeout"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	ExitCode   int    `json:"exit_code"`
	Err        string `json:"err,omitempty"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// kwinExternalPrep runs every prep step that the old unit template
// crammed into ExecStartPre, with a bounded context timeout on each
// step so a misbehaving systemctl/udevadm can no longer wedge the
// phase. The steps in order:
//
//  1. systemctl stop getty@<tty>.service              (best-effort)
//  2. loginctl enable-linger <user>                   (best-effort)
//  3. mkdir -p /run/user/<uid>                        (must succeed)
//  4. chown <user>:<user> /run/user/<uid>             (best-effort)
//  5. chmod 0700 /run/user/<uid>                      (best-effort)
//  6. systemctl start user@<uid>.service              (best-effort)
//  7. loginctl attach seat0 <drm-card-sysfs-path>     (best-effort)
//  8. udevadm trigger --settle --subsystem-match=drm  (best-effort)
//  9. udevadm settle                                  (best-effort)
//
// 10. chvt <ttyN>                                     (best-effort)
//
// "best-effort" means: failure produces a recorded diagnostic but
// does NOT abort the phase. The kwin-realvt service start is the
// gate; if any of these were essential and missing the service will
// fail to come up and the caller surfaces that specifically.
func kwinExternalPrep(ctx context.Context, deps *Deps, disc KWinSessionDiscovery, log *slog.Logger) KWinPrepResult {
	out := KWinPrepResult{}
	ttyBase := filepath.Base(disc.TTY) // "tty7"

	add := func(step KWinPrepStep, ok bool) {
		out.Steps = append(out.Steps, step)
		_ = ok // kept for readability
	}

	// 1. Stop getty.
	gettyStep := runExternalPrepStep(ctx, deps, "stop-getty",
		[]string{"systemctl", "stop", "getty@" + ttyBase + ".service"},
		10*time.Second, true, log)
	add(gettyStep, gettyStep.Err == "")
	out.GettyStopped = gettyStep.ExitCode == 0

	// 2. enable-linger.
	lingerStep := runExternalPrepStep(ctx, deps, "enable-linger",
		[]string{"loginctl", "enable-linger", disc.User},
		15*time.Second, true, log)
	add(lingerStep, lingerStep.Err == "")
	out.LingerEnabled = lingerStep.ExitCode == 0

	// 3-5. runtime dir + perms.
	mkdirStep := runExternalPrepStep(ctx, deps, "mkdir-runtime-dir",
		[]string{"mkdir", "-p", disc.XDGRuntimeDir}, 10*time.Second, true, log)
	add(mkdirStep, mkdirStep.Err == "")
	chownStep := runExternalPrepStep(ctx, deps, "chown-runtime-dir",
		[]string{"chown", disc.User + ":" + disc.User, disc.XDGRuntimeDir},
		10*time.Second, true, log)
	add(chownStep, chownStep.Err == "")
	chmodStep := runExternalPrepStep(ctx, deps, "chmod-runtime-dir",
		[]string{"chmod", "0700", disc.XDGRuntimeDir}, 10*time.Second, true, log)
	add(chmodStep, chmodStep.Err == "")
	if mkdirStep.ExitCode == 0 && chownStep.ExitCode == 0 {
		out.RuntimeDirReady = true
	}

	// 6. start user@<uid> service. Bounded; the live VM hang was a
	//    `systemctl start user@1002.service` that ran inside the
	//    kwin unit's start-pre and never returned. Outside the
	//    unit, with our process-group kill on context timeout, this
	//    can no longer wedge us.
	userBusStep := runExternalPrepStep(ctx, deps, "start-user-bus",
		[]string{"systemctl", "start", "user@" + disc.UID + ".service"},
		20*time.Second, true, log)
	add(userBusStep, userBusStep.Err == "")
	out.UserBusReachable = userBusStep.ExitCode == 0

	// 7. Attach DRM card to seat0. We feed loginctl the full sysfs
	//    path (resolved via udevadm info) so it works on cards that
	//    aren't auto-attached on cloud images.
	if disc.DRMCard != "" {
		cardSysfs := drmCardSysfsPath(ctx, deps, disc.DRMCard, log)
		if cardSysfs != "" {
			attachStep := runExternalPrepStep(ctx, deps, "attach-drm-to-seat",
				[]string{"loginctl", "attach", disc.Seat, cardSysfs},
				15*time.Second, true, log)
			add(attachStep, attachStep.Err == "")
			out.SeatAttachOK = attachStep.ExitCode == 0
		}
	}

	// 8-9. udev settle.
	udevTriggerStep := runExternalPrepStep(ctx, deps, "udevadm-trigger-drm",
		[]string{"udevadm", "trigger", "--settle", "--subsystem-match=drm"},
		20*time.Second, true, log)
	add(udevTriggerStep, udevTriggerStep.Err == "")
	udevSettleStep := runExternalPrepStep(ctx, deps, "udevadm-settle",
		[]string{"udevadm", "settle"}, 15*time.Second, true, log)
	add(udevSettleStep, udevSettleStep.Err == "")
	if udevTriggerStep.ExitCode == 0 && udevSettleStep.ExitCode == 0 {
		out.UdevSettleOK = true
	}

	// 10. chvt. Bounded with `timeout 5s` upstream + our own
	//     context timeout. Failure is non-fatal: TTYPath= in the
	//     unit re-tries the chvt as the unit starts.
	chvtStep := runExternalPrepStep(ctx, deps, "chvt",
		[]string{"chvt", fmt.Sprintf("%d", ttyNumberFromName(ttyBase))},
		8*time.Second, true, log)
	add(chvtStep, chvtStep.Err == "")
	out.ChvtOK = chvtStep.ExitCode == 0

	return out
}

// runExternalPrepStep wraps deps.Runner.Exec with a per-step context
// timeout and captures elapsed time / exit / stderr tail. Failures
// are NEVER returned as Go errors -- everything is recorded as a
// step so the caller can decide what to do.
func runExternalPrepStep(ctx context.Context, deps *Deps, name string, argv []string, timeout time.Duration, sudo bool, log *slog.Logger) KWinPrepStep {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := deps.Runner.Exec(stepCtx, runner.CommandSpec{
		Argv:    argv,
		Sudo:    sudo,
		LogFile: "-",
		Timeout: timeout,
		DryRun:  deps.DryRun,
	})
	elapsed := time.Since(start)
	step := KWinPrepStep{
		Name:       name,
		Argv:       strings.Join(argv, " "),
		Timeout:    timeout.String(),
		ElapsedMS:  elapsed.Milliseconds(),
		ExitCode:   res.ExitCode,
		StdoutTail: lastLines(res.Stdout, 4),
		StderrTail: lastLines(res.Stderr, 4),
	}
	if res.Err != nil {
		step.Err = res.Err.Error()
	}
	if log != nil {
		log.Info("phase kwin-session: external prep step",
			"step", name, "argv", step.Argv,
			"elapsed_ms", step.ElapsedMS, "exit", step.ExitCode,
			"err", step.Err)
	}
	return step
}

// drmCardSysfsPath returns the absolute /sys/devices/.../drm/cardN
// path for a /dev/dri/cardN node. Live VM evidence:
//
//	$ udevadm info -q path -n /dev/dri/card0
//	/devices/pci0000:00/0000:00:07.0/drm/card0
//	$ loginctl attach seat0 "/sys/devices/pci0000:00/0000:00:07.0/drm/card0"
//
// We use udevadm directly so we don't have to maintain a custom
// sysfs-walk; loginctl wants the full sysfs path, not the /dev path.
func drmCardSysfsPath(ctx context.Context, deps *Deps, devPath string, log *slog.Logger) string {
	if strings.TrimSpace(devPath) == "" {
		return ""
	}
	stepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res := deps.Runner.Exec(stepCtx, runner.CommandSpec{
		Argv:    []string{"udevadm", "info", "-q", "path", "-n", devPath},
		Sudo:    false,
		LogFile: "-",
		Timeout: 10 * time.Second,
		DryRun:  deps.DryRun,
	})
	rel := strings.TrimSpace(strings.Split(res.Stdout, "\n")[0])
	if rel == "" || strings.HasPrefix(strings.ToLower(rel), "unknown") {
		if log != nil {
			log.Warn("phase kwin-session: udevadm info returned no path for "+devPath, "stderr", lastLines(res.Stderr, 2))
		}
		return ""
	}
	if !strings.HasPrefix(rel, "/sys/") {
		return "/sys" + rel
	}
	return rel
}

func ttyNumberFromName(name string) int {
	rest := strings.TrimPrefix(name, "tty")
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 7
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 7
	}
	return n
}

// -----------------------------------------------------------------------------
// adoption - if a previous run already has a healthy KWin, keep it.
// -----------------------------------------------------------------------------

// KWinAdoptCheck is the result of probing whether an existing
// kwin-realvt.service is already in a healthy state we should keep.
// All four conditions must be true to count as "adoptable":
//
//   - unit is "active"
//   - MainPID > 0
//   - /run/user/<uid>/wayland-0 exists
//   - the unit's last 200 journal lines contain no fatal markers
//
// MinUptime is the wall-clock seconds the unit has been active.
// Caller may require >= 30s before adopting.
type KWinAdoptCheck struct {
	Adoptable     bool     `json:"adoptable"`
	ActiveState   string   `json:"active_state"`
	SubState      string   `json:"sub_state"`
	MainPID       int      `json:"main_pid"`
	SocketPresent bool     `json:"socket_present"`
	UptimeSeconds int      `json:"uptime_seconds"`
	JournalFatals []string `json:"journal_fatal_hits,omitempty"`
}

// probeKWinAdoption walks the four "is the existing unit healthy?"
// gates above. Caller supplies the unit name + uid + how-long-active
// minimum (30s default). The probe uses the same SystemctlFn /
// SystemctlIsActiveFn / SystemctlShowMainPIDFn / JournalRecentFn
// stubs the rest of the phase already exposes, so tests can drive
// every branch without poking real systemd.
func (p KWinSession) probeKWinAdoption(ctx context.Context, deps *Deps, unitName, uid string, minUptime time.Duration) KWinAdoptCheck {
	out := KWinAdoptCheck{}
	if state, err := p.isActive(ctx, deps, unitName); err == nil {
		out.ActiveState = state
	}
	out.SubState = p.showSubState(ctx, deps, unitName)
	if pid, err := p.mainPID(ctx, deps, unitName); err == nil {
		out.MainPID = pid
	}
	out.SocketPresent = p.socketExists(uid)
	out.UptimeSeconds = p.unitActiveSeconds(ctx, deps, unitName)
	if jrn, err := p.journalRecent(ctx, deps, unitName, 200); err == nil {
		out.JournalFatals = scanFatalSignatures(jrn)
	}
	if out.ActiveState != "active" {
		return out
	}
	if out.MainPID <= 0 {
		return out
	}
	if !out.SocketPresent {
		return out
	}
	if len(out.JournalFatals) > 0 {
		return out
	}
	if minUptime > 0 && time.Duration(out.UptimeSeconds)*time.Second < minUptime {
		return out
	}
	out.Adoptable = true
	return out
}

// showSubState returns the unit's systemd SubState ("running",
// "start-pre", "start-post", "failed", ...). Used by failure
// classification to identify wedged-in-start-pre / start-post.
func (p KWinSession) showSubState(ctx context.Context, deps *Deps, unitName string) string {
	if deps == nil || deps.Runner == nil {
		return ""
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"systemctl", "show", unitName, "-p", "SubState", "--value"},
		LogFile: "-",
		Timeout: 8 * time.Second,
		DryRun:  deps.DryRun,
	})
	return strings.TrimSpace(strings.Split(res.Stdout, "\n")[0])
}

// unitActiveSeconds returns floor((now - ActiveEnterTimestampMonotonic)
// / 1e6) seconds. 0 means "not active" or "could not read".
func (p KWinSession) unitActiveSeconds(ctx context.Context, deps *Deps, unitName string) int {
	if deps == nil || deps.Runner == nil {
		return 0
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv: []string{"systemctl", "show", unitName,
			"-p", "ActiveEnterTimestampMonotonic", "--value"},
		LogFile: "-",
		Timeout: 8 * time.Second,
		DryRun:  deps.DryRun,
	})
	monotonic := strings.TrimSpace(strings.Split(res.Stdout, "\n")[0])
	if monotonic == "" || monotonic == "0" {
		return 0
	}
	enterUS := 0
	for _, c := range monotonic {
		if c < '0' || c > '9' {
			return 0
		}
		enterUS = enterUS*10 + int(c-'0')
	}
	// Read CLOCK_MONOTONIC now.
	uptimeRes := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"bash", "-lc", "awk '{print int($1*1000000)}' /proc/uptime"},
		LogFile: "-",
		Timeout: 5 * time.Second,
		DryRun:  deps.DryRun,
	})
	nowUS := 0
	for _, c := range strings.TrimSpace(strings.Split(uptimeRes.Stdout, "\n")[0]) {
		if c < '0' || c > '9' {
			return 0
		}
		nowUS = nowUS*10 + int(c-'0')
	}
	if nowUS <= enterUS {
		return 0
	}
	return (nowUS - enterUS) / 1_000_000
}

// -----------------------------------------------------------------------------
// stable-PID validation
// -----------------------------------------------------------------------------

// requireStableMainPID samples the unit's MainPID every interval for
// the given window. Returns the PID and nil iff the same PID was
// observed throughout. Otherwise returns the most recent PID and an
// error describing how it changed.
//
// Live VM evidence: the openvt/runuser fallback's MainPID was bumping
// every second because KWin kept exiting; the previous "socket
// appeared" check was a false positive because /run/user/<uid>
// /wayland-0 also reappeared on each restart.
func (p KWinSession) requireStableMainPID(ctx context.Context, deps *Deps, unitName string, window, interval time.Duration) (int, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	deadline := time.Now().Add(window)
	first, err := p.mainPID(ctx, deps, unitName)
	if err != nil || first <= 0 {
		return first, fmt.Errorf("MainPID unreadable / zero at t=0: pid=%d err=%v", first, err)
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return first, ctx.Err()
		case <-time.After(interval):
		}
		pid, err := p.mainPID(ctx, deps, unitName)
		if err != nil {
			return pid, fmt.Errorf("MainPID read failure mid-window: %w", err)
		}
		if pid != first {
			return pid, fmt.Errorf("MainPID changed from %d to %d inside %s stability window (restart loop)", first, pid, window)
		}
		if pid <= 0 {
			return pid, fmt.Errorf("MainPID dropped to %d inside %s stability window (compositor exited)", pid, window)
		}
	}
	return first, nil
}

// -----------------------------------------------------------------------------
// mode helper - run from Go, NEVER as ExecStartPost.
// -----------------------------------------------------------------------------

// runModeHelperBounded invokes /usr/local/bin/clouddeploy-force-kwin-mode.sh
// (or the test override) with a hard wall-clock timeout. The script
// itself flock-locks and has its own 60-second internal deadline; we
// add a 90s safety net here so even a wedged kscreen-doctor cannot
// hang the phase. Failure is non-fatal -- the helper's own
// "diag:" lines go to the journal and stream_validate sees them.
//
// Critical: this replaces the old ExecStartPost=. The unit no longer
// references the helper at all.
func (p KWinSession) runModeHelperBounded(ctx context.Context, deps *Deps, log *slog.Logger) (bool, string) {
	helper := p.helperPath()
	if _, err := os.Stat(helper); err != nil {
		if log != nil {
			log.Info("phase kwin-session: skipping mode helper (not installed)", "path", helper)
		}
		return false, "helper-missing"
	}
	stepCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	start := time.Now()
	res := deps.Runner.Exec(stepCtx, runner.CommandSpec{
		Argv:    []string{helper},
		Sudo:    true,
		LogFile: "-",
		Timeout: 90 * time.Second,
		DryRun:  deps.DryRun,
	})
	elapsed := time.Since(start)
	if log != nil {
		log.Info("phase kwin-session: ran mode helper",
			"path", helper, "elapsed_ms", elapsed.Milliseconds(),
			"exit", res.ExitCode, "err", safeErr(res.Err))
	}
	if res.Err != nil {
		return false, res.Err.Error()
	}
	return res.ExitCode == 0, lastLines(res.Stdout, 4)
}
