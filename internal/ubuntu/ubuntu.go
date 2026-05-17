// Package ubuntu reads /etc/os-release and answers
// "is this Ubuntu version supported by the requested profile?".
//
// v3 is Ubuntu-only (see docs/UBUNTU-ONLY.md). The hdr-4k120 success
// state was reached on Ubuntu 25.10; 24.04 LTS is supported with a
// caveat (Plasma 6 path not validated). Anything older than 24.04 or
// non-Ubuntu hits the gate.
package ubuntu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DefaultOSReleasePath is /etc/os-release on every supported host.
const DefaultOSReleasePath = "/etc/os-release"

// Release is the typed slice of /etc/os-release that the gate cares about.
type Release struct {
	ID         string // "ubuntu"
	IDLike     string // "debian" on Ubuntu derivatives
	VersionID  string // "25.10"
	Codename   string // "questing"
	PrettyName string // "Ubuntu 25.10 (Questing Quokka)"
}

// IsUbuntu reports whether the release reports itself as Ubuntu OR
// an Ubuntu-derived distribution (Pop!_OS etc.) via ID_LIKE.
func (r Release) IsUbuntu() bool {
	if strings.EqualFold(r.ID, "ubuntu") {
		return true
	}
	for _, tok := range strings.Fields(r.IDLike) {
		if strings.EqualFold(tok, "ubuntu") {
			return true
		}
	}
	return false
}

// Read parses the supplied path (default /etc/os-release).
func Read(path string) (Release, error) {
	if path == "" {
		path = DefaultOSReleasePath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Release{}, fmt.Errorf("ubuntu: %s missing; this host is not Ubuntu", path)
		}
		return Release{}, fmt.Errorf("ubuntu: read %s: %w", path, err)
	}
	return Parse(string(b)), nil
}

// Parse handles the os-release key=value (with optional quoting) form.
// Unknown keys are ignored. Exported for testing.
func Parse(text string) Release {
	r := Release{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := line[:eq]
		val := strings.Trim(line[eq+1:], `"'`)
		switch key {
		case "ID":
			r.ID = val
		case "ID_LIKE":
			r.IDLike = val
		case "VERSION_ID":
			r.VersionID = val
		case "VERSION_CODENAME":
			r.Codename = val
		case "PRETTY_NAME":
			r.PrettyName = val
		}
	}
	return r
}

// Supported is the table the gate consults. Bumping this is a
// deliberate act: a release is supported only after CloudDeploy has
// validated the full streaming stack on it.
//
// Order: more-recent-first so iteration emits a stable list for
// error messages. The map form is for quick lookup.
var supportedVersions = map[string]string{
	"25.10": "validated end-to-end for the hdr-4k120 success path",
	"24.04": "supported (LTS); the Plasma 6 / patched-KWin / NVIDIA private HDR path was validated on 25.10",
}

// IsSupportedVersion reports whether the given VERSION_ID is in the
// v3 supported list. Useful for callers that want to short-circuit
// the exact-match gate (e.g. apply defers to the ubuntu-upgrade phase
// when the host is on a supported release but the profile targets a
// different one).
func IsSupportedVersion(versionID string) bool {
	_, ok := supportedVersions[versionID]
	return ok
}

// SupportedVersions returns the version IDs the gate considers OK.
// Sorted for stable printing.
func SupportedVersions() []string {
	out := make([]string, 0, len(supportedVersions))
	for v := range supportedVersions {
		out = append(out, v)
	}
	// Lexicographic sort: 24.04 < 25.10 (works for "YY.MM" since
	// both fields are zero-padded).
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i] > out[j] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// GateResult is what `doctor system` and `apply` get back.
type GateResult struct {
	Release   Release
	Supported bool
	Reason    string
}

// Gate is the verdict the doctor + apply consult. Pure function of
// Release; tests pass synthetic Release records. targetVersion is optional.
func Gate(r Release, targetVersion string) GateResult {
	out := GateResult{Release: r}
	if !r.IsUbuntu() {
		out.Reason = fmt.Sprintf("not Ubuntu (ID=%q, ID_LIKE=%q); v3 is Ubuntu-only", r.ID, r.IDLike)
		return out
	}
	if r.VersionID == "" {
		out.Reason = "Ubuntu reported no VERSION_ID; cannot gate"
		return out
	}

	if targetVersion != "" {
		if r.VersionID == targetVersion {
			out.Supported = true
			out.Reason = fmt.Sprintf("matches profile target Ubuntu %s", targetVersion)
			return out
		}
		out.Reason = fmt.Sprintf("found Ubuntu %s but profile requires Ubuntu %s; use --allow-unsupported to override", r.VersionID, targetVersion)
		return out
	}

	if note, ok := supportedVersions[r.VersionID]; ok {
		out.Supported = true
		out.Reason = note
		return out
	}
	out.Reason = fmt.Sprintf("Ubuntu %s is not in the supported list %v; use --allow-unsupported to override (you accept the deploy may fail)", r.VersionID, SupportedVersions())
	return out
}

// ReadAndGate is the convenience the CLI uses.
func ReadAndGate(path string, targetVersion string) (GateResult, error) {
	r, err := Read(path)
	if err != nil {
		return GateResult{}, err
	}
	return Gate(r, targetVersion), nil
}
