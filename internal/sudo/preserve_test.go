package sudo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidUser(t *testing.T) {
	cases := map[string]bool{
		"aedyn":      true,
		"clouddep":   true,
		"_systemd":   true,
		"ubuntu":     true,
		"a-good-1":   true,
		"":           false,
		"4starts":    false,
		"$x":         false,
		"with space": false,
		"sudo;rm":    false,
		"sudo|rm":    false,
		"sudo`bad`":  false,
	}
	for u, want := range cases {
		if got := ValidUser(u); got != want {
			t.Errorf("ValidUser(%q): got %v want %v", u, got, want)
		}
	}
}

func TestFilenameForUserUnder(t *testing.T) {
	got, err := FilenameForUserUnder("/etc/sudoers.d", "operator")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// On Linux: /etc/sudoers.d/90-clouddeploy-operator
	// On Windows: \etc\sudoers.d\90-clouddeploy-operator (filepath.Join uses \).
	// Just assert the base name; that's the load-bearing part.
	if filepath.Base(got) != "90-clouddeploy-operator" {
		t.Errorf("base: got %q want 90-clouddeploy-operator (full=%q)", filepath.Base(got), got)
	}
}

func TestFilenameForUserUnder_RefusesUnsafe(t *testing.T) {
	_, err := FilenameForUserUnder("/etc/sudoers.d", "../etc/passwd")
	if err == nil {
		t.Fatalf("expected refusal for unsafe user; got nil")
	}
}

func TestRenderRule_HasUserAndNOPASSWD(t *testing.T) {
	body := RenderRule("aedyn")
	if !strings.Contains(body, "aedyn ALL=(ALL) NOPASSWD:ALL") {
		t.Errorf("rendered rule missing NOPASSWD line: %q", body)
	}
	if !strings.Contains(body, "managed by clouddeployctl") {
		t.Errorf("rendered rule missing CloudDeploy marker: %q", body)
	}
}

func TestPreserve_WritesFile(t *testing.T) {
	dir := t.TempDir()
	res, err := Preserve(PreserveOptions{
		User:        "ubuntu",
		SudoersDir:  dir,
		VisudoCheck: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if !res.Wrote {
		t.Errorf("Wrote: got false want true")
	}
	if res.AlreadyCorrect {
		t.Errorf("AlreadyCorrect: got true want false on first write")
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read sudoers: %v", err)
	}
	if !strings.Contains(string(body), "ubuntu ALL=(ALL) NOPASSWD:ALL") {
		t.Errorf("sudoers body missing the rule: %q", string(body))
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows reports 0444 for any read-only file; the actual
		// 0440 vs 0444 distinction is meaningful only on POSIX.
		if perm := info.Mode().Perm(); perm != 0o440 {
			t.Errorf("sudoers perm: got %o want 0440", perm)
		}
	}
}

func TestPreserve_IdempotentWhenAlreadyCorrect(t *testing.T) {
	dir := t.TempDir()
	first, err := Preserve(PreserveOptions{
		User:        "ubuntu",
		SudoersDir:  dir,
		VisudoCheck: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("first Preserve: %v", err)
	}
	second, err := Preserve(PreserveOptions{
		User:        "ubuntu",
		SudoersDir:  dir,
		VisudoCheck: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("second Preserve: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("path differs across runs: %q vs %q", first.Path, second.Path)
	}
	if second.Wrote {
		t.Errorf("Wrote: got true want false (file already correct)")
	}
	if !second.AlreadyCorrect {
		t.Errorf("AlreadyCorrect: got false want true")
	}
}

func TestPreserve_VisudoRejectionLeavesFileIntact(t *testing.T) {
	dir := t.TempDir()
	// First Preserve writes a valid file.
	_, err := Preserve(PreserveOptions{
		User:        "ubuntu",
		SudoersDir:  dir,
		VisudoCheck: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("first Preserve: %v", err)
	}
	target := filepath.Join(dir, "90-clouddeploy-ubuntu")
	beforeBody, _ := os.ReadFile(target)

	// Second Preserve simulates a "good" content but visudo rejects.
	// (The body produced by RenderRule is the same as before, so to
	// force a rewrite we shorten the destination first.) We have to
	// chmod the existing file back to writable; Preserve installed
	// it 0440 and Windows refuses to overwrite read-only files.
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target writable: %v", err)
	}
	if err := os.WriteFile(target, []byte("stale\n"), 0o440); err != nil {
		t.Fatalf("stale write: %v", err)
	}
	_, err = Preserve(PreserveOptions{
		User:       "ubuntu",
		SudoersDir: dir,
		VisudoCheck: func(string) error {
			return errors.New("simulated visudo: syntax error near line 1")
		},
	})
	if err == nil {
		t.Fatalf("expected visudo rejection to surface")
	}
	if !strings.Contains(err.Error(), "visudo") {
		t.Errorf("error should mention visudo: %v", err)
	}
	// Destination must still contain the stale content (NOT replaced
	// with the invalid candidate).
	got, _ := os.ReadFile(target)
	if string(got) != "stale\n" {
		t.Errorf("destination changed despite visudo rejection: got %q want %q", string(got), "stale\n")
	}
	// And no leftover temp file.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clouddeploy-sudoers-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	_ = beforeBody
}

func TestPreserve_DryRunIsObservableNoop(t *testing.T) {
	dir := t.TempDir()
	res, err := Preserve(PreserveOptions{
		User:       "ubuntu",
		SudoersDir: dir,
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("Preserve: %v", err)
	}
	if res.Wrote {
		t.Errorf("DryRun should not write; got Wrote=true")
	}
	// File must not exist.
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Errorf("file should not exist under DryRun; stat err=%v", err)
	}
}
