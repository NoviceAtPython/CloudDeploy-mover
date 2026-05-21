package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// keysOnly returns a slice of the secret key names for use in test
// failure messages. Critical: NEVER include the raw value here so a
// test failure dump doesn't leak the operator's auth key into the CI
// log.
func keysOnly(kv map[string]string) []string {
	out := []string{}
	for k := range kv {
		out = append(out, k)
	}
	return out
}

func TestRender_TailscaleAuthKey_SingleQuoteEscapes(t *testing.T) {
	body, err := Render(map[string]string{
		"TAILSCALE_AUTHKEY": "fixture-authkey-AbC's-and-spaces 42",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The single quote in the value must become '\''  so the line is
	// shell-safe.
	want := `TAILSCALE_AUTHKEY='fixture-authkey-AbC'\''s-and-spaces 42'` + "\n"
	if !strings.Contains(body, want) {
		t.Errorf("Render did not single-quote-escape the value (looking at the key-only diff): keys=%v", keysOnly(map[string]string{"TAILSCALE_AUTHKEY": ""}))
	}
}

func TestRender_SortedKeysAndManagedHeader(t *testing.T) {
	body, err := Render(map[string]string{
		"ZED":   "last",
		"ALPHA": "first",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(body, "# managed by clouddeployctl:") {
		t.Errorf("body should start with managed-by header; got first 80 chars: %q", body[:min(len(body), 80)])
	}
	// ALPHA must come before ZED.
	ai := strings.Index(body, "ALPHA=")
	zi := strings.Index(body, "ZED=")
	if ai < 0 || zi < 0 || ai > zi {
		t.Errorf("keys not sorted: ALPHA index=%d ZED index=%d", ai, zi)
	}
}

func TestRender_RejectsInvalidKey(t *testing.T) {
	for _, bad := range []string{
		"lower",
		"with-dash",
		"with.dot",
		"123_LEADING_DIGIT",
		"",
	} {
		_, err := Render(map[string]string{bad: "x"})
		if err == nil {
			t.Errorf("Render must reject key %q", bad)
			continue
		}
		if !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Render(%q): err=%v want ErrInvalidKey", bad, err)
		}
	}
}

func TestRender_RejectsNewlineValue(t *testing.T) {
	_, err := Render(map[string]string{"K": "ok\nbad"})
	if err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("multiline value: err=%v want ErrInvalidValue", err)
	}
}

func TestRender_RejectsNULValue(t *testing.T) {
	_, err := Render(map[string]string{"K": "ok\x00bad"})
	if err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("NUL value: err=%v want ErrInvalidValue", err)
	}
}

func TestWrite_AtomicAnd0600OnUnix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	err := Write(path, map[string]string{
		"TAILSCALE_AUTHKEY": "fixture-authkey-fixture",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows reports 0444 for any read-only file; the precise
		// 0600 vs 0644 distinction is meaningful only on POSIX.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode: got %o want 0600", perm)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "TAILSCALE_AUTHKEY=") {
		t.Errorf("written body missing the configured key (keys-only check)")
	}
	// Sanity: no `.secrets-*.env` leftover from the temp file.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".secrets-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWrite_EmptyMapRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := Write(path, map[string]string{"TAILSCALE_AUTHKEY": "x"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Write(path, map[string]string{}); err != nil {
		t.Fatalf("Write(empty): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file removed; stat err=%v", err)
	}
}

func TestLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	want := map[string]string{
		"TAILSCALE_AUTHKEY": "fixture-authkey-AbC's-42",
		"EXTRA_TOKEN":       "value with spaces",
	}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["TAILSCALE_AUTHKEY"] != want["TAILSCALE_AUTHKEY"] {
		t.Errorf("round-trip TAILSCALE_AUTHKEY changed; key-only diff")
	}
	if got["EXTRA_TOKEN"] != want["EXTRA_TOKEN"] {
		t.Errorf("round-trip EXTRA_TOKEN changed; key-only diff")
	}
}

func TestLoad_MissingFileReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(filepath.Join(dir, "absent.env"))
	if err != nil {
		t.Errorf("Load missing: err=%v want nil", err)
	}
	if got != nil {
		t.Errorf("Load missing: got %v want nil", keysOnly(got))
	}
}

func TestMerge_AddsKeyWithoutTouchingOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := Write(path, map[string]string{
		"TAILSCALE_AUTHKEY": "fixture",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Merge(path, map[string]string{
		"EXTRA_TOKEN": "new",
	}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got["TAILSCALE_AUTHKEY"]; !ok {
		t.Errorf("Merge dropped existing key (key-only diff: %v)", keysOnly(got))
	}
	if got["EXTRA_TOKEN"] != "new" {
		t.Errorf("Merge did not add new key (key-only diff: %v)", keysOnly(got))
	}
}

func TestPathFromEnv_RespectsOverride(t *testing.T) {
	t.Setenv("CLOUDDEPLOY_SECRETS_ENV", "/run/secrets-custom.env")
	if got := PathFromEnv(); got != "/run/secrets-custom.env" {
		t.Errorf("PathFromEnv: got %q want /run/secrets-custom.env", got)
	}
}

func TestPathFromEnv_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("CLOUDDEPLOY_SECRETS_ENV", "")
	if got := PathFromEnv(); got != DefaultPath {
		t.Errorf("PathFromEnv: got %q want %q", got, DefaultPath)
	}
}

// Loud-and-clear smoke test: the test binary itself must never embed
// or print the raw key value. (If `go test -v` shows the fixture key
// below, it's because some other test broke its key-only contract.)
func TestRender_NoValueAppearsInKeyList(t *testing.T) {
	kv := map[string]string{"TAILSCALE_AUTHKEY": "should-never-appear-in-test-output"}
	keys := KeyList(kv)
	for _, k := range keys {
		if strings.Contains(k, "should-never-appear") {
			t.Fatalf("KeyList leaked a value: %v", keys)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
