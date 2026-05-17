package ubuntu

import (
	"errors"
	"fmt"
	"strings"
)

// VersionID -> codename mapping. Only the releases CloudDeploy
// actively supports for upgrade-source / upgrade-target appear here;
// anything else falls through to "unknown" and the upgrade phase
// refuses to hop blindly.
//
// Source: https://wiki.ubuntu.com/Releases.
var codenameByVersion = map[string]string{
	"22.04": "jammy",
	"22.10": "kinetic",
	"23.04": "lunar",
	"23.10": "mantic",
	"24.04": "noble",
	"24.10": "oracular",
	"25.04": "plucky",
	"25.10": "questing",
	"26.04": "resolute", // placeholder; not yet released - guard with care
}

// CodenameForVersion returns the apt suite name for a Ubuntu version
// (e.g. "24.04" -> "noble"). Returns "" if unknown.
func CodenameForVersion(version string) string {
	return codenameByVersion[strings.TrimSpace(version)]
}

// IsLTS reports whether the Ubuntu VERSION_ID is an LTS release.
// Convention: even-year `.04` releases are LTS (22.04, 24.04, 26.04);
// `.10` releases and odd-year `.04` releases (e.g. 23.04) are not.
func IsLTS(version string) bool {
	v := strings.TrimSpace(version)
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return false
	}
	if parts[1] != "04" {
		return false
	}
	// Even-year .04 = LTS.
	if len(parts[0]) != 2 {
		return false
	}
	year := parts[0]
	if year[len(year)-1] == '2' || year[len(year)-1] == '4' || year[len(year)-1] == '6' || year[len(year)-1] == '8' || year[len(year)-1] == '0' {
		return true
	}
	return false
}

// UpgradePolicy mirrors the YAML knob
// `profile.deploy.direct_apt_codename_upgrade`.
type UpgradePolicy string

const (
	PolicyAuto  UpgradePolicy = "auto"
	PolicyForce UpgradePolicy = "force"
	PolicyOff   UpgradePolicy = "off"
)

// ParseUpgradePolicy turns the profile string into a typed value.
// Empty / unrecognised inputs map to PolicyAuto. Returns an error for
// values that are clearly typos so the operator gets a clean message.
func ParseUpgradePolicy(s string) (UpgradePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return PolicyAuto, nil
	case "force", "1", "true", "yes", "on":
		return PolicyForce, nil
	case "off", "0", "false", "no":
		return PolicyOff, nil
	}
	return "", fmt.Errorf("ubuntu: unknown direct_apt_codename_upgrade value %q (want auto/force/off)", s)
}

// UpgradeAction is the verdict of PlanUpgrade.
type UpgradeAction int

const (
	// UpgradeNoop: the host is already at the target version.
	UpgradeNoop UpgradeAction = iota
	// UpgradeNotConfigured: the profile has no target version OR
	// deploy.auto_upgrade_ubuntu=false. The phase will return a
	// clear error explaining what to set.
	UpgradeNotConfigured
	// UpgradeRefusedNonLTS: target is non-LTS (e.g. 25.10) and
	// deploy.accept_non_lts=false. Refuse with a clear error.
	UpgradeRefusedNonLTS
	// UpgradeRefusedUnknownHop: codename mapping doesn't know one
	// or both sides. Refuse rather than guess.
	UpgradeRefusedUnknownHop
	// UpgradeDirectCodenameRewrite: rewrite apt sources `from` ->
	// `to` and dist-upgrade in place.
	UpgradeDirectCodenameRewrite
	// UpgradeDoReleaseUpgrade: invoke `do-release-upgrade`.
	UpgradeDoReleaseUpgrade
)

// UpgradePlan is the input + chosen action + reason.
type UpgradePlan struct {
	Action          UpgradeAction
	CurrentVersion  string
	TargetVersion   string
	CurrentCodename string
	TargetCodename  string
	Reason          string
	Err             error
}

// PlanInputs is the data PlanUpgrade consumes.
type PlanInputs struct {
	CurrentVersion           string
	TargetVersion            string
	AutoUpgrade              bool
	AcceptNonLTS             bool
	DirectAptCodenameUpgrade UpgradePolicy
}

// Common errors PlanUpgrade returns. The phase wraps these as
// failed_fatal with the structured Reason.
var (
	ErrNoTargetVersion     = errors.New("ubuntu: profile has no ubuntu_version target")
	ErrAutoUpgradeDisabled = errors.New("ubuntu: profile requires release upgrade but deploy.auto_upgrade_ubuntu=false")
	ErrNonLTSRefused       = errors.New("ubuntu: target is non-LTS and deploy.accept_non_lts=false")
	ErrUnknownHop          = errors.New("ubuntu: unknown source or target Ubuntu version; cannot plan upgrade")
)

// PlanUpgrade decides what the ubuntu-upgrade phase should do given
// the profile config + current host state. Pure function; the phase
// applies the result via apt.Transaction / runner.
//
// Rules:
//
//  1. Target empty -> UpgradeNotConfigured. Phase logs skipped and
//     returns nil.
//  2. Current == target -> UpgradeNoop. Mark done.
//  3. AutoUpgrade=false -> UpgradeNotConfigured + ErrAutoUpgradeDisabled.
//  4. Target non-LTS + AcceptNonLTS=false -> UpgradeRefusedNonLTS +
//     ErrNonLTSRefused.
//  5. Either codename unknown -> UpgradeRefusedUnknownHop +
//     ErrUnknownHop.
//  6. DirectAptCodenameUpgrade=force, or =auto and the hop is one
//     of the known-failing hops (currently 24.04 -> 25.10) ->
//     UpgradeDirectCodenameRewrite.
//  7. Otherwise -> UpgradeDoReleaseUpgrade.
func PlanUpgrade(in PlanInputs) UpgradePlan {
	current := strings.TrimSpace(in.CurrentVersion)
	target := strings.TrimSpace(in.TargetVersion)

	plan := UpgradePlan{
		CurrentVersion:  current,
		TargetVersion:   target,
		CurrentCodename: CodenameForVersion(current),
		TargetCodename:  CodenameForVersion(target),
	}

	if target == "" {
		plan.Action = UpgradeNotConfigured
		plan.Reason = "profile.ubuntu_version is empty; ubuntu-upgrade phase is a no-op"
		plan.Err = ErrNoTargetVersion
		return plan
	}

	if current == target {
		plan.Action = UpgradeNoop
		plan.Reason = fmt.Sprintf("host already at target Ubuntu %s; no upgrade needed", target)
		return plan
	}

	if !in.AutoUpgrade {
		plan.Action = UpgradeNotConfigured
		plan.Reason = fmt.Sprintf("profile requires Ubuntu %s; host is on %s; enable deploy.auto_upgrade_ubuntu=true or use a %s image", target, current, target)
		plan.Err = ErrAutoUpgradeDisabled
		return plan
	}

	if !IsLTS(target) && !in.AcceptNonLTS {
		plan.Action = UpgradeRefusedNonLTS
		plan.Reason = fmt.Sprintf("target Ubuntu %s is non-LTS; set deploy.accept_non_lts=true to authorize the upgrade", target)
		plan.Err = ErrNonLTSRefused
		return plan
	}

	if plan.CurrentCodename == "" || plan.TargetCodename == "" {
		plan.Action = UpgradeRefusedUnknownHop
		plan.Reason = fmt.Sprintf("unknown Ubuntu codename for %s (->%s) or %s (->%s); refusing to plan blind hop",
			current, plan.CurrentCodename, target, plan.TargetCodename)
		plan.Err = ErrUnknownHop
		return plan
	}

	policy := in.DirectAptCodenameUpgrade
	if policy == "" {
		policy = PolicyAuto
	}
	useDirect := false
	switch policy {
	case PolicyForce:
		useDirect = true
	case PolicyOff:
		useDirect = false
	case PolicyAuto:
		useDirect = isKnownDirectHop(current, target)
	}

	if useDirect {
		plan.Action = UpgradeDirectCodenameRewrite
		plan.Reason = fmt.Sprintf("direct apt codename rewrite %s (%s) -> %s (%s); do-release-upgrade refuses this hop",
			current, plan.CurrentCodename, target, plan.TargetCodename)
		return plan
	}

	plan.Action = UpgradeDoReleaseUpgrade
	plan.Reason = fmt.Sprintf("do-release-upgrade %s (%s) -> %s (%s)", current, plan.CurrentCodename, target, plan.TargetCodename)
	return plan
}

// isKnownDirectHop returns true for source -> target pairs where
// `do-release-upgrade -d` is known to refuse and the operator MUST
// rewrite apt sources directly. Currently only 24.04 -> 25.10. Add
// more as new hops are validated.
func isKnownDirectHop(current, target string) bool {
	return current == "24.04" && target == "25.10"
}

// String makes UpgradeAction self-describing in logs / tests.
func (a UpgradeAction) String() string {
	switch a {
	case UpgradeNoop:
		return "noop"
	case UpgradeNotConfigured:
		return "not-configured"
	case UpgradeRefusedNonLTS:
		return "refused-non-lts"
	case UpgradeRefusedUnknownHop:
		return "refused-unknown-hop"
	case UpgradeDirectCodenameRewrite:
		return "direct-codename-rewrite"
	case UpgradeDoReleaseUpgrade:
		return "do-release-upgrade"
	}
	return "unknown"
}
