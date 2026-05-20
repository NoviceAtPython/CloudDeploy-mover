package phase

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoviceAtPython/CloudDeploy-mover/internal/runner"
)

// HeadlessUserName is the canonical state-key.
const HeadlessUserName = "headless_user"

// UserInfo is what HeadlessUser.LookupFn returns. UID=="" means
// the account does not exist yet; the phase will create it.
type UserInfo struct {
	UID    string
	GID    string
	Home   string
	Shell  string
	Groups []string // supplementary group names; sorted
}

// HeadlessUser is the first Milestone 4A phase: create-or-verify the
// dedicated account that owns the headless KDE/KWin session. The
// phase handles:
//
//  1. POSIX login-name validation (Profile.Desktop.User defaults to
//     "cloudgamer"; ValidateProfile already enforces safety).
//  2. Account creation when the user doesn't exist
//     (useradd -m -s <shell>).
//  3. Supplementary group membership (usermod -aG ...).
//  4. loginctl enable-linger so /run/user/<uid> + systemd user bus
//     persist after disconnect.
//  5. Home-dir ownership sanity (chown -R <user>:<gid> /home/<user>).
//
// All side effects are injectable; the production path shells out to
// `id`, `useradd`, `usermod`, `loginctl`, `chown` via the runner.
type HeadlessUser struct {
	// LookupFn returns the current UserInfo for `name`, or
	// UserInfo{UID:""} when the account doesn't exist. nil = use
	// os/user + read /etc/group via the runner.
	LookupFn func(ctx context.Context, deps *Deps, name string) (UserInfo, error)

	// CreateUserFn provisions a missing account. nil = `useradd -m
	// -s <shell> <name>` via the runner.
	CreateUserFn func(ctx context.Context, deps *Deps, name, shell string) error

	// AddGroupsFn adds the named user to each of the named
	// supplementary groups. nil = `usermod -aG <csv> <name>` via the
	// runner.
	AddGroupsFn func(ctx context.Context, deps *Deps, name string, groups []string) error

	// EnableLingerFn is `loginctl enable-linger <name>`. nil =
	// runner call. EnableLinger=false in the profile makes this a
	// no-op regardless.
	EnableLingerFn func(ctx context.Context, deps *Deps, name string, enable bool) error

	// ChownHomeFn is `chown -R <user>:<gid> <home>`. nil = runner.
	ChownHomeFn func(ctx context.Context, deps *Deps, name, gid, home string) error
}

// Name implements Phase.
func (HeadlessUser) Name() string { return HeadlessUserName }

// Run implements Phase.
func (p HeadlessUser) Run(ctx context.Context, deps *Deps) error {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if shouldSkip(deps.State, HeadlessUserName) {
		log.Info("phase headless-user: already done; skipping")
		return nil
	}
	deps.State.MarkRunning(HeadlessUserName)
	_ = deps.PersistState()

	desk := deps.Profile.EffectiveDesktop()
	details := map[string]any{
		"user":          desk.User,
		"linger_target": desk.EnableLinger,
		"groups_target": desk.Groups,
	}

	// 1. Look up the user.
	info, err := p.lookup(ctx, deps, desk.User)
	if err != nil {
		return p.failPhase(deps, "lookup user", err, details)
	}
	created := false
	if info.UID == "" {
		// 2. Create.
		log.Info("phase headless-user: creating account",
			"user", desk.User, "shell", desk.Shell)
		if err := p.createUser(ctx, deps, desk.User, desk.Shell); err != nil {
			return p.failPhase(deps, "useradd", err, details)
		}
		created = true
		// Re-lookup.
		info, err = p.lookup(ctx, deps, desk.User)
		if err != nil {
			return p.failPhase(deps, "post-create lookup", err, details)
		}
		if info.UID == "" {
			return p.failPhase(deps, "post-create lookup returned empty UID", fmt.Errorf("useradd appeared to succeed but %q still missing", desk.User), details)
		}
	}
	details["uid"] = info.UID
	details["gid"] = info.GID
	details["home"] = info.Home
	details["shell"] = info.Shell
	details["created_or_existing"] = "existing"
	if created {
		details["created_or_existing"] = "created"
	}

	// 3. Diff supplementary groups + add missing ones.
	needed := missingGroups(info.Groups, desk.Groups)
	if len(needed) > 0 {
		log.Info("phase headless-user: adding supplementary groups",
			"user", desk.User, "missing", needed, "already", info.Groups)
		if err := p.addGroups(ctx, deps, desk.User, needed); err != nil {
			return p.failPhase(deps, "usermod -aG", err, details)
		}
		// Re-lookup so state reflects the post-add membership.
		info, err = p.lookup(ctx, deps, desk.User)
		if err != nil {
			return p.failPhase(deps, "post-usermod lookup", err, details)
		}
	}
	details["groups"] = info.Groups

	// 4. Linger.
	//
	// Live VM regression history:
	//   * `enable_linger` was a plain bool so a missing YAML key
	//     defaulted to false. Headless KWin then died at boot because
	//     /run/user/<uid> never came up.
	//   * Fix is in EffectiveDesktop: missing key -> true. We surface
	//     `linger_requested` (the operator's intent), `linger_enabled`
	//     (what we actually applied via loginctl), and
	//     `linger_defaulted` (whether the YAML had an explicit key).
	lingerRequested := desk.LingerEnabled()
	details["linger_requested"] = lingerRequested
	details["linger_defaulted"] = deps.Profile.DesktopLingerExplicit() == false
	if lingerRequested {
		if err := p.enableLinger(ctx, deps, desk.User, true); err != nil {
			return p.failPhase(deps, "loginctl enable-linger", err, details)
		}
		// Verify via loginctl show-user <user> -p Linger. Best-effort:
		// a missing loginctl on a developer host is just informational.
		if verified, raw := p.verifyLinger(ctx, deps, desk.User); raw != "" {
			details["linger_verified_raw"] = raw
			details["linger_enabled"] = verified
		} else {
			details["linger_enabled"] = true
		}
	} else {
		details["linger_enabled"] = false
	}

	// 5. Home-dir ownership. Best-effort: a failure here is logged
	// but not fatal because most deploys never need a corrective
	// chown.
	if info.Home != "" {
		if err := p.chownHome(ctx, deps, desk.User, info.GID, info.Home); err != nil {
			log.Warn("phase headless-user: chown home failed (non-fatal)",
				"err", err, "home", info.Home)
			details["chown_home_warning"] = err.Error()
		}
	}

	deps.State.MarkDone(HeadlessUserName, details)
	_ = deps.PersistState()
	log.Info("phase headless-user: done",
		"user", desk.User, "uid", info.UID, "groups", info.Groups,
		"linger", details["linger_enabled"], "created", created)
	return nil
}

func (p HeadlessUser) failPhase(deps *Deps, what string, err error, details map[string]any) error {
	details["err"] = err.Error()
	deps.State.MarkFailed(HeadlessUserName, what, err, true)
	deps.State.Get(HeadlessUserName).Details = details
	_ = deps.PersistState()
	return fmt.Errorf("phase headless-user: %s: %w", what, err)
}

// missingGroups returns elements of `want` that are not present in
// `have`. Order matches `want`.
func missingGroups(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, g := range have {
		set[g] = true
	}
	var out []string
	for _, g := range want {
		if !set[g] {
			out = append(out, g)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// production-side wrappers (LookupFn / CreateUserFn / ...).
// -----------------------------------------------------------------------------

func (p HeadlessUser) lookup(ctx context.Context, deps *Deps, name string) (UserInfo, error) {
	if p.LookupFn != nil {
		return p.LookupFn(ctx, deps, name)
	}
	return defaultLookup(ctx, deps, name)
}

// defaultLookup is the production path. Uses os/user for the passwd
// row + a small `id -nG <user>` call for the supplementary-group
// list. Returns UserInfo{UID:""} when the account doesn't exist.
func defaultLookup(ctx context.Context, deps *Deps, name string) (UserInfo, error) {
	u, err := user.Lookup(name)
	if err != nil {
		if _, ok := err.(user.UnknownUserError); ok {
			return UserInfo{}, nil
		}
		return UserInfo{}, err
	}
	out := UserInfo{
		UID:   u.Uid,
		GID:   u.Gid,
		Home:  u.HomeDir,
		Shell: "", // os/user doesn't expose the shell on Linux
	}
	// Read supplementary groups via `id -nG <user>`. cheap + portable
	// compared to parsing /etc/group ourselves.
	if deps != nil && deps.Runner != nil {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"id", "-nG", name},
			LogFile: "-",
			Timeout: 5 * time.Second,
		})
		if res.Err == nil {
			fields := strings.Fields(strings.TrimSpace(res.Stdout))
			sort.Strings(fields)
			out.Groups = fields
		}
	}
	// Try to read /etc/passwd's shell field via getent. Best-effort.
	if deps != nil && deps.Runner != nil {
		res := deps.Runner.Exec(ctx, runner.CommandSpec{
			Argv:    []string{"getent", "passwd", name},
			LogFile: "-",
			Timeout: 5 * time.Second,
		})
		if res.Err == nil {
			cols := strings.Split(strings.TrimRight(res.Stdout, "\n"), ":")
			if len(cols) >= 7 {
				out.Shell = cols[6]
			}
		}
	}
	return out, nil
}

func (p HeadlessUser) createUser(ctx context.Context, deps *Deps, name, shell string) error {
	if p.CreateUserFn != nil {
		return p.CreateUserFn(ctx, deps, name, shell)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"useradd", "-m", "-s", shell, name},
		Sudo:    true,
		Timeout: 60 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("useradd -m -s %s %s: %w (stderr=%q)", shell, name, res.Err, lastLines(res.Stderr, 3))
	}
	return nil
}

func (p HeadlessUser) addGroups(ctx context.Context, deps *Deps, name string, groups []string) error {
	if p.AddGroupsFn != nil {
		return p.AddGroupsFn(ctx, deps, name, groups)
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"usermod", "-aG", strings.Join(groups, ","), name},
		Sudo:    true,
		Timeout: 30 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("usermod -aG %s %s: %w (stderr=%q)", strings.Join(groups, ","), name, res.Err, lastLines(res.Stderr, 3))
	}
	return nil
}

func (p HeadlessUser) enableLinger(ctx context.Context, deps *Deps, name string, enable bool) error {
	if p.EnableLingerFn != nil {
		return p.EnableLingerFn(ctx, deps, name, enable)
	}
	verb := "enable-linger"
	if !enable {
		verb = "disable-linger"
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"loginctl", verb, name},
		Sudo:    true,
		Timeout: 15 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("loginctl %s %s: %w (stderr=%q)", verb, name, res.Err, lastLines(res.Stderr, 3))
	}
	return nil
}

// verifyLinger reads `loginctl show-user <name> -p Linger`. Returns
// (verified, rawOutput). Best-effort: a missing loginctl / unparseable
// output returns (false, "") and the phase falls back to trusting
// the enableLinger return value.
func (p HeadlessUser) verifyLinger(ctx context.Context, deps *Deps, name string) (bool, string) {
	if deps == nil || deps.Runner == nil {
		return false, ""
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"loginctl", "show-user", name, "-p", "Linger"},
		LogFile: "-",
		Timeout: 10 * time.Second,
	})
	if res.Err != nil {
		return false, ""
	}
	out := strings.TrimSpace(res.Stdout)
	// loginctl emits "Linger=yes" / "Linger=no".
	switch out {
	case "Linger=yes":
		return true, out
	case "Linger=no":
		return false, out
	}
	return false, out
}

func (p HeadlessUser) chownHome(ctx context.Context, deps *Deps, name, gid, home string) error {
	if p.ChownHomeFn != nil {
		return p.ChownHomeFn(ctx, deps, name, gid, home)
	}
	target := name
	if gid != "" {
		target = name + ":" + gid
	}
	res := deps.Runner.Exec(ctx, runner.CommandSpec{
		Argv:    []string{"chown", "-R", target, home},
		Sudo:    true,
		Timeout: 60 * time.Second,
		DryRun:  deps.DryRun,
	})
	if res.Err != nil {
		return fmt.Errorf("chown -R %s %s: %w (stderr=%q)", target, home, res.Err, lastLines(res.Stderr, 3))
	}
	return nil
}

// ParseUIDForState surfaces the uid as an int when callers want it
// (the kwin-session phase needs it to build /run/user/<uid>). Returns
// 0 on parse failure; the caller treats 0 as "unknown" since real
// user UIDs start at 1000.
func ParseUIDForState(uid string) int {
	n, err := strconv.Atoi(strings.TrimSpace(uid))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
