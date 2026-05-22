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
	Action UpgradeAction
	// CurrentVersion is the host VERSION_ID before this hop runs.
	CurrentVersion string
	// TargetVersion is the version this single hop will reach. For a
	// multi-step upgrade (e.g. 22.04 -> 25.10 routed via 24.04) this
	// equals the INTERMEDIATE target (24.04), not the final one.
	TargetVersion string
	// FinalTargetVersion is the version the operator ultimately
	// requested (profile.ubuntu_version or the resolver pick). When
	// the planner chose an intermediate hop, the resume after this
	// hop's reboot will re-plan with this same FinalTargetVersion;
	// the planner picks "noop" once Current == Final.
	//
	// In a single-hop case FinalTargetVersion == TargetVersion.
	FinalTargetVersion string
	CurrentCodename    string
	TargetCodename     string
	Reason             string
	Err                error
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
	finalTarget := strings.TrimSpace(in.TargetVersion)

	plan := UpgradePlan{
		CurrentVersion:     current,
		TargetVersion:      finalTarget,
		FinalTargetVersion: finalTarget,
		CurrentCodename:    CodenameForVersion(current),
		TargetCodename:     CodenameForVersion(finalTarget),
	}

	if finalTarget == "" {
		plan.Action = UpgradeNotConfigured
		plan.Reason = "profile.ubuntu_version is empty; ubuntu-upgrade phase is a no-op"
		plan.Err = ErrNoTargetVersion
		return plan
	}

	if current == finalTarget {
		plan.Action = UpgradeNoop
		plan.Reason = fmt.Sprintf("host already at target Ubuntu %s; no upgrade needed", finalTarget)
		return plan
	}

	if !in.AutoUpgrade {
		plan.Action = UpgradeNotConfigured
		plan.Reason = fmt.Sprintf("profile requires Ubuntu %s; host is on %s; enable deploy.auto_upgrade_ubuntu=true or use a %s image", finalTarget, current, finalTarget)
		plan.Err = ErrAutoUpgradeDisabled
		return plan
	}

	if !IsLTS(finalTarget) && !in.AcceptNonLTS {
		plan.Action = UpgradeRefusedNonLTS
		plan.Reason = fmt.Sprintf("target Ubuntu %s is non-LTS; set deploy.accept_non_lts=true to authorize the upgrade", finalTarget)
		plan.Err = ErrNonLTSRefused
		return plan
	}

	if plan.CurrentCodename == "" || plan.TargetCodename == "" {
		plan.Action = UpgradeRefusedUnknownHop
		plan.Reason = fmt.Sprintf("unknown Ubuntu codename for %s (->%s) or %s (->%s); refusing to plan blind hop",
			current, plan.CurrentCodename, finalTarget, plan.TargetCodename)
		plan.Err = ErrUnknownHop
		return plan
	}

	// Intermediate-hop routing. Some upgrades skip too many releases
	// to be safe in a single apt-rewrite (e.g. jammy -> questing
	// crosses noble, and several key packages renamed across that
	// span). NextHopForUpgrade returns either finalTarget (single
	// hop is fine) or the next LTS waypoint (e.g. 24.04 between
	// 22.04 and 25.10). When the planner chose an intermediate, the
	// resume after this hop's reboot re-runs ubuntu-upgrade and the
	// next call lands at the correct second hop.
	hopTarget := NextHopForUpgrade(current, finalTarget)
	if hopTarget == "" {
		hopTarget = finalTarget
	}
	plan.TargetVersion = hopTarget
	plan.TargetCodename = CodenameForVersion(hopTarget)
	intermediate := hopTarget != finalTarget

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
		useDirect = isKnownDirectHop(current, hopTarget)
	}

	if useDirect {
		plan.Action = UpgradeDirectCodenameRewrite
		if intermediate {
			plan.Reason = fmt.Sprintf("direct apt codename rewrite %s (%s) -> %s (%s) [intermediate hop toward final target %s]",
				current, plan.CurrentCodename, hopTarget, plan.TargetCodename, finalTarget)
		} else {
			plan.Reason = fmt.Sprintf("direct apt codename rewrite %s (%s) -> %s (%s); do-release-upgrade refuses this hop",
				current, plan.CurrentCodename, hopTarget, plan.TargetCodename)
		}
		return plan
	}

	plan.Action = UpgradeDoReleaseUpgrade
	if intermediate {
		plan.Reason = fmt.Sprintf("do-release-upgrade %s (%s) -> %s (%s) [intermediate hop toward final target %s]",
			current, plan.CurrentCodename, hopTarget, plan.TargetCodename, finalTarget)
	} else {
		plan.Reason = fmt.Sprintf("do-release-upgrade %s (%s) -> %s (%s)", current, plan.CurrentCodename, hopTarget, plan.TargetCodename)
	}
	return plan
}

// isKnownDirectHop returns true for source -> target pairs where the
// safest apt-mechanic is a direct codename rewrite + dist-upgrade.
// We use direct rewrites for:
//
//   - 22.04 -> 24.04: jammy -> noble. do-release-upgrade handles this
//     fine on a real machine, but it shells into a curses UI that
//     does NOT cooperate with an unattended runner. The apt-rewrite
//     mechanic is dependency-equivalent for an LTS-to-LTS hop and
//     finishes cleanly via apt-get dist-upgrade.
//   - 24.04 -> 25.10: noble -> questing. do-release-upgrade -d
//     refuses this hop (noble does not yet know about questing in
//     its meta-release file); direct rewrite is the only path.
//   - 24.04 -> 26.04: noble -> resolute. LTS-to-LTS direct rewrite
//     (do-release-upgrade only ships in a *.04 LTS for the next LTS
//     after it ships, which we cannot rely on inside an unattended
//     deploy).
//
// All other hops fall through to do-release-upgrade.
func isKnownDirectHop(current, target string) bool {
	switch {
	case current == "22.04" && target == "24.04":
		return true
	case current == "24.04" && target == "25.10":
		return true
	case current == "24.04" && target == "26.04":
		return true
	}
	return false
}

// NextHopForUpgrade returns the next Ubuntu version to target when
// the host is on `current` and the operator's FINAL target is
// `finalTarget`. For single-step hops it returns finalTarget; for
// multi-step upgrades it returns the LTS waypoint between them.
//
// Reasoning: jumping more than one major release in a single apt
// codename rewrite is risky -- key packages have been renamed across
// the span, qt5/qt6 transitions land in between, etc. The safer
// pattern is to step through the most recent LTS first. The deploy's
// reboot+resume loop naturally chains these: hop to the LTS, reboot,
// resume re-evaluates, hops to the final target.
//
// Examples:
//
//	NextHopForUpgrade("22.04", "24.04") == "24.04"  // direct
//	NextHopForUpgrade("22.04", "25.10") == "24.04"  // intermediate
//	NextHopForUpgrade("22.04", "26.04") == "24.04"  // intermediate
//	NextHopForUpgrade("24.04", "25.10") == "25.10"  // direct
//	NextHopForUpgrade("24.04", "26.04") == "26.04"  // direct (LTS-to-LTS)
//	NextHopForUpgrade("25.10", "26.04") == "26.04"  // direct
//
// "" is returned for unknown source/target, telling the caller to
// fall back to the existing single-hop logic.
func NextHopForUpgrade(current, finalTarget string) string {
	if current == "" || finalTarget == "" {
		return finalTarget
	}
	if current == finalTarget {
		return finalTarget
	}
	if CodenameForVersion(current) == "" || CodenameForVersion(finalTarget) == "" {
		return finalTarget
	}
	// Jammy starting point: route every non-trivial upgrade through
	// 24.04 LTS first. Even jammy -> 24.10 (hypothetical) would
	// re-evaluate after the reboot; the resolver would then either
	// pick 25.10 or stay at 24.04, both of which are single hops.
	if current == "22.04" && finalTarget != "24.04" {
		return "24.04"
	}
	// Future-proofing: a 20.04 host (currently NOT in supported
	// versions) would also need an intermediate hop, but we leave
	// that to a future patch when we validate that path.
	return finalTarget
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
