package ubuntu

import (
	"errors"
	"strings"
	"testing"
)

func TestCodenameForVersion(t *testing.T) {
	cases := map[string]string{
		"24.04": "noble",
		"25.10": "questing",
		"22.04": "jammy",
		"99.99": "",
	}
	for in, want := range cases {
		if got := CodenameForVersion(in); got != want {
			t.Errorf("CodenameForVersion(%q): got %q want %q", in, got, want)
		}
	}
}

func TestIsLTS(t *testing.T) {
	cases := map[string]bool{
		"22.04": true,
		"24.04": true,
		"26.04": true,
		"24.10": false,
		"25.04": false, // odd year .04 is NOT LTS by Ubuntu's convention
		"25.10": false,
		"":      false,
	}
	for in, want := range cases {
		if got := IsLTS(in); got != want {
			t.Errorf("IsLTS(%q): got %v want %v", in, got, want)
		}
	}
}

func TestParseUpgradePolicy(t *testing.T) {
	cases := map[string]UpgradePolicy{
		"":      PolicyAuto,
		"auto":  PolicyAuto,
		"force": PolicyForce,
		"true":  PolicyForce,
		"off":   PolicyOff,
		"0":     PolicyOff,
	}
	for in, want := range cases {
		got, err := ParseUpgradePolicy(in)
		if err != nil {
			t.Errorf("ParseUpgradePolicy(%q): err=%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUpgradePolicy(%q): got %q want %q", in, got, want)
		}
	}
	if _, err := ParseUpgradePolicy("maybe"); err == nil {
		t.Error("ParseUpgradePolicy should reject unknown value")
	}
}

func TestPlanUpgrade_AlreadyAtTarget(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion: "25.10",
		TargetVersion:  "25.10",
		AutoUpgrade:    true,
		AcceptNonLTS:   true,
	})
	if plan.Action != UpgradeNoop {
		t.Errorf("Action: got %s want noop (reason=%q)", plan.Action, plan.Reason)
	}
	if plan.Err != nil {
		t.Errorf("Err: got %v want nil", plan.Err)
	}
}

func TestPlanUpgrade_EmptyTarget(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion: "24.04",
		TargetVersion:  "",
		AutoUpgrade:    true,
	})
	if plan.Action != UpgradeNotConfigured {
		t.Errorf("Action: got %s want not-configured", plan.Action)
	}
	if !errors.Is(plan.Err, ErrNoTargetVersion) {
		t.Errorf("Err: got %v want ErrNoTargetVersion", plan.Err)
	}
}

func TestPlanUpgrade_24To25_AutoUpgradeFalse_FailsClearly(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion: "24.04",
		TargetVersion:  "25.10",
		AutoUpgrade:    false,
	})
	if plan.Action != UpgradeNotConfigured {
		t.Errorf("Action: got %s want not-configured", plan.Action)
	}
	if !errors.Is(plan.Err, ErrAutoUpgradeDisabled) {
		t.Errorf("Err: got %v want ErrAutoUpgradeDisabled", plan.Err)
	}
	if !strings.Contains(plan.Reason, "auto_upgrade_ubuntu") {
		t.Errorf("Reason should name the knob: %q", plan.Reason)
	}
}

func TestPlanUpgrade_24To25_AutoUpgradeTrue_NonLTSGuard(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion: "24.04",
		TargetVersion:  "25.10",
		AutoUpgrade:    true,
		AcceptNonLTS:   false,
	})
	if plan.Action != UpgradeRefusedNonLTS {
		t.Errorf("Action: got %s want refused-non-lts", plan.Action)
	}
	if !errors.Is(plan.Err, ErrNonLTSRefused) {
		t.Errorf("Err: got %v want ErrNonLTSRefused", plan.Err)
	}
	if !strings.Contains(plan.Reason, "accept_non_lts") {
		t.Errorf("Reason should name the knob: %q", plan.Reason)
	}
}

func TestPlanUpgrade_24To25_PlansDirectRewrite(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "24.04",
		TargetVersion:            "25.10",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyAuto,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Errorf("Action: got %s want direct-codename-rewrite (reason=%q, err=%v)", plan.Action, plan.Reason, plan.Err)
	}
	if plan.Err != nil {
		t.Errorf("Err should be nil for direct-rewrite plan; got %v", plan.Err)
	}
	if plan.CurrentCodename != "noble" || plan.TargetCodename != "questing" {
		t.Errorf("codenames: got %s -> %s; want noble -> questing", plan.CurrentCodename, plan.TargetCodename)
	}
}

func TestPlanUpgrade_UnknownHop(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion: "20.04",
		TargetVersion:  "99.99",
		AutoUpgrade:    true,
		AcceptNonLTS:   true,
	})
	if plan.Action != UpgradeRefusedUnknownHop {
		t.Errorf("Action: got %s want refused-unknown-hop", plan.Action)
	}
	if !errors.Is(plan.Err, ErrUnknownHop) {
		t.Errorf("Err: got %v want ErrUnknownHop", plan.Err)
	}
}

func TestPlanUpgrade_ForceDirectOverridesPolicy(t *testing.T) {
	// 22.04 -> 24.04 is normally a do-release-upgrade hop; force flips it.
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "22.04",
		TargetVersion:            "24.04",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyForce,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Errorf("PolicyForce should pick direct-rewrite; got %s", plan.Action)
	}
}

func TestPlanUpgrade_OffPolicyUsesDoReleaseUpgrade(t *testing.T) {
	// 24.04 -> 25.10 with off-policy: try do-release-upgrade even
	// though it will fail. The plan is "do-release-upgrade"; the
	// phase's job is to surface the error if it dies.
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "24.04",
		TargetVersion:            "25.10",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyOff,
	})
	if plan.Action != UpgradeDoReleaseUpgrade {
		t.Errorf("PolicyOff should pick do-release-upgrade; got %s", plan.Action)
	}
}

// -----------------------------------------------------------------------------
// Jammy (22.04) upgrade hops
// -----------------------------------------------------------------------------

func TestNextHopForUpgrade_JammyToNobleIsDirect(t *testing.T) {
	if got := NextHopForUpgrade("22.04", "24.04"); got != "24.04" {
		t.Fatalf("22.04 -> 24.04: got %q want 24.04 (single hop)", got)
	}
}

func TestNextHopForUpgrade_JammyToQuestingRoutesThroughNoble(t *testing.T) {
	if got := NextHopForUpgrade("22.04", "25.10"); got != "24.04" {
		t.Fatalf("22.04 -> 25.10: got %q want 24.04 (intermediate LTS waypoint)", got)
	}
}

func TestNextHopForUpgrade_JammyToResoluteRoutesThroughNoble(t *testing.T) {
	if got := NextHopForUpgrade("22.04", "26.04"); got != "24.04" {
		t.Fatalf("22.04 -> 26.04: got %q want 24.04 (LTS waypoint, even when final is LTS too)", got)
	}
}

func TestNextHopForUpgrade_NobleToQuestingIsDirect(t *testing.T) {
	if got := NextHopForUpgrade("24.04", "25.10"); got != "25.10" {
		t.Fatalf("24.04 -> 25.10: got %q want 25.10", got)
	}
}

func TestNextHopForUpgrade_UnknownCodenameFallsBack(t *testing.T) {
	if got := NextHopForUpgrade("17.10", "25.10"); got != "25.10" {
		t.Fatalf("unknown source codename should fall back to single-hop logic; got %q", got)
	}
	if got := NextHopForUpgrade("22.04", "99.99"); got != "99.99" {
		t.Fatalf("unknown target codename should fall back to single-hop logic; got %q", got)
	}
}

func TestPlanUpgrade_JammyToNoble_AutoPolicyPicksDirectRewrite(t *testing.T) {
	// 22.04 -> 24.04 is now a known direct hop in auto mode (no
	// PolicyForce needed). Live VM 2026-05-22: a Vast 22.04 host
	// targeting the auto-unattended profile needs this path.
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "22.04",
		TargetVersion:            "24.04",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyAuto,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Fatalf("22.04 -> 24.04 in auto mode should pick direct-codename-rewrite; got %s (reason=%q)",
			plan.Action, plan.Reason)
	}
	if plan.CurrentCodename != "jammy" || plan.TargetCodename != "noble" {
		t.Fatalf("codenames: got %s -> %s; want jammy -> noble", plan.CurrentCodename, plan.TargetCodename)
	}
	if plan.FinalTargetVersion != "24.04" {
		t.Fatalf("FinalTargetVersion: got %q want 24.04 (single hop)", plan.FinalTargetVersion)
	}
}

func TestPlanUpgrade_JammyToQuesting_RoutesThroughNobleIntermediate(t *testing.T) {
	// 22.04 -> 25.10 should NOT try a single jammy -> questing
	// rewrite. The planner picks 24.04 as the intermediate; the
	// deploy hops there, reboots, and resume re-plans the second
	// hop (24.04 -> 25.10) on next invocation.
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "22.04",
		TargetVersion:            "25.10",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyAuto,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Fatalf("22.04 -> 25.10 should be planned as a direct-rewrite hop to 24.04; got %s (reason=%q)",
			plan.Action, plan.Reason)
	}
	if plan.TargetVersion != "24.04" {
		t.Fatalf("TargetVersion: got %q want 24.04 (intermediate)", plan.TargetVersion)
	}
	if plan.FinalTargetVersion != "25.10" {
		t.Fatalf("FinalTargetVersion: got %q want 25.10 (the user's actual ask)", plan.FinalTargetVersion)
	}
	if plan.TargetCodename != "noble" {
		t.Fatalf("TargetCodename: got %q want noble", plan.TargetCodename)
	}
	if !strings.Contains(plan.Reason, "intermediate") {
		t.Errorf("Reason should mention intermediate hop: %q", plan.Reason)
	}
	if !strings.Contains(plan.Reason, "25.10") {
		t.Errorf("Reason should mention the final target so monitor users can see where we're headed: %q", plan.Reason)
	}
}

func TestPlanUpgrade_JammyToResolute_RoutesThroughNoble(t *testing.T) {
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "22.04",
		TargetVersion:            "26.04",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyAuto,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Fatalf("22.04 -> 26.04: got %s want direct-codename-rewrite (intermediate hop)", plan.Action)
	}
	if plan.TargetVersion != "24.04" {
		t.Fatalf("TargetVersion: got %q want 24.04 (intermediate)", plan.TargetVersion)
	}
	if plan.FinalTargetVersion != "26.04" {
		t.Fatalf("FinalTargetVersion: got %q want 26.04", plan.FinalTargetVersion)
	}
}

func TestPlanUpgrade_NobleToResolute_DirectLTSToLTS(t *testing.T) {
	// 24.04 -> 26.04 is an LTS-to-LTS hop; do-release-upgrade may
	// not yet know about resolute by the time clouddeployctl runs,
	// so the planner picks direct rewrite.
	plan := PlanUpgrade(PlanInputs{
		CurrentVersion:           "24.04",
		TargetVersion:            "26.04",
		AutoUpgrade:              true,
		AcceptNonLTS:             true,
		DirectAptCodenameUpgrade: PolicyAuto,
	})
	if plan.Action != UpgradeDirectCodenameRewrite {
		t.Fatalf("24.04 -> 26.04 should be direct-codename-rewrite (LTS-to-LTS, single hop); got %s (reason=%q)",
			plan.Action, plan.Reason)
	}
	if plan.TargetVersion != "26.04" {
		t.Fatalf("TargetVersion: got %q want 26.04 (single hop)", plan.TargetVersion)
	}
}
