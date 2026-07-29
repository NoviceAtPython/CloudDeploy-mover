package ubuntu

import (
	"fmt"
	"strings"
)

// OSSelectionPolicy is the typed value behind
// `profile.deploy.ubuntu_selection_policy`.
type OSSelectionPolicy string

const (
	// OSPolicyLatestCompatible (default): walk Candidates newest-
	// first; pick the first one that passes the gate set
	// (codename-known + v3-supported + non-LTS gate).
	OSPolicyLatestCompatible OSSelectionPolicy = "latest-compatible"
	// OSPolicyExact: require a candidate list of length 1; that
	// candidate must pass all gates.
	OSPolicyExact OSSelectionPolicy = "exact"
	// OSPolicyMinVersion: filter out candidates below MinVersion.
	OSPolicyMinVersion OSSelectionPolicy = "min-version"
	// OSPolicyAny: accept any known candidate, even non-LTS, even
	// non-validated. Intended for diagnostic runs.
	OSPolicyAny OSSelectionPolicy = "any"
)

// ParseOSSelectionPolicy normalizes the YAML knob. Empty defaults to
// OSPolicyLatestCompatible.
func ParseOSSelectionPolicy(s string) (OSSelectionPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "latest-compatible":
		return OSPolicyLatestCompatible, nil
	case "exact":
		return OSPolicyExact, nil
	case "min-version":
		return OSPolicyMinVersion, nil
	case "any":
		return OSPolicyAny, nil
	}
	return "", fmt.Errorf("ubuntu: unknown ubuntu_selection_policy %q (want latest-compatible|exact|min-version|any)", s)
}

// OSCandidate is one row of the resolver's decision table. The phase
// records the full slice in state.Details["ubuntu_candidates_tried"]
// so the operator can see WHY each version was accepted or rejected.
type OSCandidate struct {
	Version       string
	Codename      string
	KnownCodename bool
	IsLTS         bool
	Supported     bool   // appears in ubuntu.SupportedVersions()
	Ok            bool   // all gates passed
	Reason        string // populated when Ok=false
}

// PackageProbeFn answers "would the profile-required packages probably
// install on this Ubuntu version?". Today the phase wires nil ("trust
// the codename + v3-supported gates"); future commits can plumb a real
// apt-cache probe. Returning false rejects the candidate; the resolver
// will move on to the next one.
type PackageProbeFn func(version string) (ok bool, reason string)

// OSResolverInputs bundles everything ResolveOSTarget needs.
type OSResolverInputs struct {
	// CurrentVersion is the host's VERSION_ID (e.g. "24.04"). Used
	// only for diagnostics today, but the resolver may grow a
	// "stay-on-current" branch in the future.
	CurrentVersion string

	// Candidates is the operator's ordered list (newest first). The
	// resolver does NOT sort; the list order is authoritative.
	Candidates []string

	// MinVersion is the floor under OSPolicyMinVersion. Ignored
	// otherwise.
	MinVersion string

	// Policy controls which candidates pass.
	Policy OSSelectionPolicy

	// AcceptNonLTS unlocks non-LTS targets (.10 releases). When
	// false, non-LTS candidates are rejected with a clear reason.
	AcceptNonLTS bool

	// PreferLTS is recorded only; the resolver does NOT reorder.
	PreferLTS bool

	// Probe is the optional per-candidate package-availability hook.
	// nil = no probe.
	Probe PackageProbeFn


	// SkipSupportedCheck disables the "must be in
	// ubuntu.SupportedVersions()" gate. Used by tests / by
	// OSPolicyAny which intentionally accepts experimental versions.
	SkipSupportedCheck bool
}

// OSResolverResult is what ResolveOSTarget returns. Selected="" means
// no candidate passed.
type OSResolverResult struct {
	Selected         string
	SelectedCodename string
	SelectedReason   string
	Tried            []OSCandidate
}

// RejectedReasons extracts the version->reason map for state.Details.
// Only includes candidates the resolver actually rejected (so a
// successful selection's prior-rejected candidates show up; the
// selected one does not).
func (r OSResolverResult) RejectedReasons() map[string]string {
	out := map[string]string{}
	for _, c := range r.Tried {
		if !c.Ok {
			out[c.Version] = c.Reason
		}
	}
	return out
}

// ResolveOSTarget walks Candidates newest-first and returns the first
// one that passes every applicable gate. When Policy is "exact", the
// resolver requires Candidates to contain exactly one entry and that
// single entry must pass.
func ResolveOSTarget(in OSResolverInputs) OSResolverResult {
	policy := in.Policy
	if policy == "" {
		policy = OSPolicyLatestCompatible
	}
	if policy == OSPolicyExact {
		if len(in.Candidates) != 1 {
			return OSResolverResult{
				Tried: []OSCandidate{{
					Reason: fmt.Sprintf("policy=exact requires exactly one candidate; got %d (%v)", len(in.Candidates), in.Candidates),
				}},
			}
		}
	}

	var tried []OSCandidate
	for _, v := range in.Candidates {
		c := evaluateOSCandidate(v, policy, in)
		tried = append(tried, c)
		if c.Ok {
			return OSResolverResult{
				Selected:         c.Version,
				SelectedCodename: c.Codename,
				SelectedReason:   fmt.Sprintf("policy=%s; first candidate passing gates", policy),
				Tried:            tried,
			}
		}
	}
	return OSResolverResult{Tried: tried}
}

func evaluateOSCandidate(v string, policy OSSelectionPolicy, in OSResolverInputs) OSCandidate {
	c := OSCandidate{Version: strings.TrimSpace(v)}
	code := CodenameForVersion(c.Version)
	c.Codename = code
	c.KnownCodename = code != ""
	c.IsLTS = IsLTS(c.Version)
	c.Supported = IsSupportedVersion(c.Version)

	if !c.KnownCodename {
		c.Reason = fmt.Sprintf("unknown Ubuntu codename for %q (CloudDeploy's codename map needs an entry)", c.Version)
		return c
	}
	if policy == OSPolicyMinVersion && in.MinVersion != "" {
		if compareUbuntuVersion(c.Version, in.MinVersion) < 0 {
			c.Reason = fmt.Sprintf("below min_version %s", in.MinVersion)
			return c
		}
	}
	if !c.IsLTS && !in.AcceptNonLTS {
		c.Reason = "non-LTS and deploy.accept_non_lts=false"
		return c
	}
	if !in.SkipSupportedCheck && policy != OSPolicyAny && !c.Supported {
		c.Reason = fmt.Sprintf("not in v3 supported list %v (no validated streaming-stack run yet)", SupportedVersions())
		return c
	}
	if in.Probe != nil {
		ok, why := in.Probe(c.Version)
		if !ok {
			if why == "" {
				why = "package-probe rejected without reason"
			}
			c.Reason = "package probe rejected: " + why
			return c
		}
	}
	c.Ok = true
	return c
}

// compareUbuntuVersion does a YY.MM compare. Returns -1 / 0 / 1 by
// converting "24.04" -> 2404 etc., so "24.04" < "25.10" < "26.04".
// Falls back to a string compare on unparseable input.
func compareUbuntuVersion(a, b string) int {
	ai, aok := versionInt(a)
	bi, bok := versionInt(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	switch {
	case ai < bi:
		return -1
	case ai > bi:
		return 1
	}
	return 0
}

func versionInt(v string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	n *= 100
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
