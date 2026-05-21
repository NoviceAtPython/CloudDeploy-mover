package discovery

import (
	"errors"
	"testing"
)

func fakeLookPath(available map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := available[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func TestMustQDBusPrefersQDBus6(t *testing.T) {
	cmd, err := MustQDBus(Resolver{LookPath: fakeLookPath(map[string]string{
		"qdbus6": "/usr/bin/qdbus6",
		"qdbus":  "/usr/bin/qdbus",
	})})
	if err != nil {
		t.Fatalf("MustQDBus: %v", err)
	}
	if cmd.Path != "/usr/bin/qdbus6" {
		t.Fatalf("path: got %q want /usr/bin/qdbus6", cmd.Path)
	}
}

func TestMustQDBusFallsBackToQDBus(t *testing.T) {
	cmd, err := MustQDBus(Resolver{LookPath: fakeLookPath(map[string]string{
		"qdbus": "/usr/bin/qdbus",
	})})
	if err != nil {
		t.Fatalf("MustQDBus: %v", err)
	}
	if cmd.Path != "/usr/bin/qdbus" {
		t.Fatalf("path: got %q want /usr/bin/qdbus", cmd.Path)
	}
}

func TestMustQDBusMissingIsClear(t *testing.T) {
	_, err := MustQDBus(Resolver{LookPath: fakeLookPath(map[string]string{})})
	if err == nil {
		t.Fatalf("expected missing qdbus error")
	}
	if got := err.Error(); got == "" || !containsAll(got, "missing Qt DBus tool", "qdbus6", "qdbus") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
