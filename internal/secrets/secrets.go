// Package secrets owns the on-disk secrets env file
// (/etc/clouddeploy/secrets.env by default).
//
// The file is consumed by systemd's EnvironmentFile= directive on
// the clouddeploy-v3-continue.service unit, which is how operator-
// supplied credentials (Tailscale auth key, future API tokens)
// survive the reboot that happens between apply and resume. v2
// relied on /root/clouddeploy-v2.env for the same purpose; v3
// uses /etc/clouddeploy/secrets.env so the path doesn't conflict
// with /root being unmounted/encrypted on some cloud images.
//
// Security contract:
//   - the file is written 0600 on Unix (Windows / Plan 9 silently
//     fall through to whatever the platform allows, since CI runs
//     these tests on the Linux build).
//   - the rename-from-temp is atomic so a crash mid-write can't
//     leave a half-written secrets file.
//   - keys MUST match the systemd EnvironmentFile= grammar:
//     `KEY=value` with key in [A-Z_][A-Z0-9_]*. We validate this
//     before writing so callers can't introduce a key that systemd
//     would silently ignore.
//   - values are single-quote escaped so embedded quotes / spaces /
//     special characters don't break the file. Newlines are
//     rejected at validation time (systemd EnvironmentFile=
//     doesn't support multiline values).
//   - no API on this package logs or returns the value. Callers
//     that want diagnostics print only the key list.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DefaultPath mirrors reboot.DefaultSecretsEnvFile. Duplicated here
// to avoid an import cycle between reboot <-> secrets <-> phase.
const DefaultPath = "/etc/clouddeploy/secrets.env"

// PathFromEnv honors CLOUDDEPLOY_SECRETS_ENV; falls back to
// DefaultPath.
func PathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("CLOUDDEPLOY_SECRETS_ENV")); v != "" {
		return v
	}
	return DefaultPath
}

// keyRE matches the systemd EnvironmentFile= key grammar.
var keyRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ErrInvalidKey is returned when a caller passes a key systemd
// would refuse to parse.
var ErrInvalidKey = errors.New("secrets: invalid env key (must match [A-Z_][A-Z0-9_]*)")

// ErrInvalidValue is returned when a value contains a newline or NUL.
// Both are illegal in systemd's EnvironmentFile= grammar.
var ErrInvalidValue = errors.New("secrets: value must not contain newline or NUL")

// Render returns the file body for the given key/value map. Sorted
// by key for stable output; tests / collect-logs / state-show can
// assert on the key list without depending on map iteration order.
//
// Values are emitted as single-quoted shell strings with embedded
// single quotes turned into '\”  — the bash-safe encoding systemd
// recognises. The body always ends with a trailing newline.
func Render(kv map[string]string) (string, error) {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		if !keyRE.MatchString(k) {
			return "", fmt.Errorf("%w: %q", ErrInvalidKey, k)
		}
		v := kv[k]
		if strings.ContainsAny(v, "\n\x00") {
			return "", fmt.Errorf("%w: key=%s", ErrInvalidValue, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# managed by clouddeployctl: operator secrets for the resume continuation service.\n")
	b.WriteString("# DO NOT EDIT BY HAND. mode 0600 root:root. systemd reads this via EnvironmentFile=.\n")
	for _, k := range keys {
		v := kv[k]
		// Single-quote shell escape: 'foo' bar's "baz" -> 'foo bar'\''s "baz"'
		esc := strings.ReplaceAll(v, "'", `'\''`)
		fmt.Fprintf(&b, "%s='%s'\n", k, esc)
	}
	return b.String(), nil
}

// KeyList returns the sorted key list from a map. Safe to log
// (never includes values).
func KeyList(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Write renders + atomically installs the secrets env file at
// `path` with mode 0600. The parent directory is created with mode
// 0755 if missing.
//
// An empty kv map deletes any existing file at `path` so an operator
// removing a knob can ensure the secret is gone from disk on the
// next apply. (The reboot unit references this file with
// EnvironmentFile=-, so an absent file is fine.)
func Write(path string, kv map[string]string) error {
	if path == "" {
		path = DefaultPath
	}
	if len(kv) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("secrets: remove %s: %w", path, err)
		}
		return nil
	}
	body, err := Render(kv)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("secrets: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*.env")
	if err != nil {
		return fmt.Errorf("secrets: create temp under %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secrets: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("secrets: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("secrets: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("secrets: chmod 0600 temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("secrets: rename %s -> %s: %w", tmpPath, path, err)
	}
	// Re-chmod the destination too: some filesystems / atomic-rename
	// kernels reset the mode through rename.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secrets: chmod 0600 %s: %w", path, err)
	}
	return nil
}

// Merge loads an existing secrets file (if any), overlays the
// supplied keys, and writes the result. Useful when a phase wants
// to add ONE key without disturbing whatever else is already on
// disk.
//
// Missing destination file is treated as empty.
func Merge(path string, kv map[string]string) error {
	if path == "" {
		path = DefaultPath
	}
	existing, _ := Load(path) // ignore "not exist" + parse errors here; Render validates again
	if existing == nil {
		existing = map[string]string{}
	}
	for k, v := range kv {
		existing[k] = v
	}
	return Write(path, existing)
}

// Load is a tolerant parser for previously-written secrets files.
// It is NOT a full bash-quoting parser; it only handles the format
// `KEY='value'` (the single-quoted form Render emits). Anything
// else is ignored. This is intentional: an operator who hand-edited
// the file in some other format is asking us to leave their lines
// alone; we just won't surface them through Load.
//
// Returns nil + nil when the file doesn't exist.
func Load(path string) (map[string]string, error) {
	if path == "" {
		path = DefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			continue
		}
		k := s[:eq]
		if !keyRE.MatchString(k) {
			continue
		}
		v := s[eq+1:]
		// Strip the surrounding single quotes Render wrote, if any.
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
			// Reverse the '\'' escape Render uses.
			v = strings.ReplaceAll(v, `'\''`, `'`)
		}
		out[k] = v
	}
	return out, nil
}
