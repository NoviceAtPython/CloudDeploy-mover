package ubuntu

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `# /etc/os-release
NAME="Ubuntu"
VERSION="25.10 (Questing Quokka)"
ID=ubuntu
ID_LIKE="debian"
PRETTY_NAME="Ubuntu 25.10 (Questing Quokka)"
VERSION_ID="25.10"
VERSION_CODENAME=questing
`
	r := Parse(in)
	if r.ID != "ubuntu" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.VersionID != "25.10" {
		t.Errorf("VERSION_ID: got %q", r.VersionID)
	}
	if r.Codename != "questing" {
		t.Errorf("VERSION_CODENAME: got %q", r.Codename)
	}
	if !r.IsUbuntu() {
		t.Errorf("IsUbuntu should be true")
	}
}

func TestParse_DerivativeViaIDLike(t *testing.T) {
	in := `ID=pop
ID_LIKE="ubuntu debian"
VERSION_ID="22.04"
`
	r := Parse(in)
	if r.ID != "pop" {
		t.Errorf("ID: got %q", r.ID)
	}
	if !r.IsUbuntu() {
		t.Errorf("Pop!_OS should classify as Ubuntu-like")
	}
}

func TestGate_SupportedVersions(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"25.10", true},
		{"24.04", true},
		{"22.04", false}, // unsupported
		{"26.04", false}, // future
		{"", false},
	}
	for _, c := range cases {
		r := Release{ID: "ubuntu", VersionID: c.version}
		g := Gate(r, "")
		if g.Supported != c.want {
			t.Errorf("version %q: Supported got %v want %v (reason=%q)", c.version, g.Supported, c.want, g.Reason)
		}
	}
}

func TestGate_NonUbuntu(t *testing.T) {
	r := Release{ID: "fedora", VersionID: "40"}
	g := Gate(r, "")
	if g.Supported {
		t.Errorf("Fedora should not be supported")
	}
	if !strings.Contains(g.Reason, "Ubuntu-only") {
		t.Errorf("reason should mention Ubuntu-only: %q", g.Reason)
	}
}

func TestSupportedVersionsSorted(t *testing.T) {
	v := SupportedVersions()
	if len(v) < 2 {
		t.Fatalf("expected at least 2 supported versions; got %v", v)
	}
	// Stable order: lexicographic, oldest first.
	for i := 0; i < len(v)-1; i++ {
		if v[i] > v[i+1] {
			t.Errorf("SupportedVersions not sorted: %v", v)
			break
		}
	}
}
