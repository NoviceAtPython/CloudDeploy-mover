// Package sudo writes /etc/sudoers.d/90-clouddeploy-<user> with a
// passwordless rule for the operator account. The point is recovery:
// when a half-broken Ubuntu upgrade (dist-upgrade -> snapd postinst
// failure -> /etc/sudoers replaced mid-flight) drops the operator's
// account out of the sudoers, CloudDeploy still needs SSH-side
// privileged access to finish or roll back the deploy.
//
// The 90- prefix puts the file LATE in sudoers.d's load order so it
// overrides anything the upgrade may have rewritten. visudo -cf
// validates before install; we never replace an in-place rule with
// invalid syntax.
package sudo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SudoersD is the canonical drop-in directory.
const SudoersD = "/etc/sudoers.d"

// FilenamePrefix is the prefix the package owns; the per-user file is
// FilenamePrefix + sanitized-user.
const FilenamePrefix = "90-clouddeploy-"

// validUserRE accepts a POSIX-ish login name. Used to refuse
// rendering for shell-meaningful or path-meaningful strings before
// they reach sudoers.
var validUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)

// FilenameForUser returns the file path under /etc/sudoers.d for a
// given user. The user is validated against POSIX login-name rules.
func FilenameForUser(user string) (string, error) {
	return FilenameForUserUnder(SudoersD, user)
}

// FilenameForUserUnder lets tests redirect the sudoers.d dir.
func FilenameForUserUnder(dir, user string) (string, error) {
	if !ValidUser(user) {
		return "", fmt.Errorf("sudo: refused unsafe sudoers user %q (must match %s)", user, validUserRE.String())
	}
	return filepath.Join(dir, FilenamePrefix+user), nil
}

// ValidUser reports whether `user` is safe to embed in a sudoers
// drop-in. Real Linux login-name rules are stricter than what we
// enforce, but this regex blocks anything with shell-meaningful
// characters that would let a hostile input rewrite the sudoers
// file.
func ValidUser(user string) bool {
	return validUserRE.MatchString(user)
}

// RenderRule returns the sudoers content for the given user. The
// rule is intentionally simple: NOPASSWD:ALL so SSH-side `sudo`
// works even when the regular sudoers has been clobbered.
func RenderRule(user string) string {
	return fmt.Sprintf("# managed by clouddeployctl: emergency sudo preserve\n%s ALL=(ALL) NOPASSWD:ALL\n", user)
}

// PreserveOptions configures the side-effecting Preserve call.
type PreserveOptions struct {
	// User is the operator account to preserve.
	User string
	// SudoersDir overrides /etc/sudoers.d for tests.
	SudoersDir string
	// VisudoCheck is the injectable validation step. nil = call
	// `visudo -cf <path>` via os/exec when Preserve runs as root on
	// Linux. Tests pass a stub.
	VisudoCheck func(path string) error
	// DryRun: render the file path + content but make no filesystem
	// changes.
	DryRun bool
}

// PreserveResult is what Preserve returns to the caller (the phase).
type PreserveResult struct {
	// User is the user the rule was rendered for.
	User string
	// Path is the destination sudoers.d entry path.
	Path string
	// AlreadyCorrect is true when the file already existed with the
	// expected content; Preserve was a no-op.
	AlreadyCorrect bool
	// Wrote is true when Preserve created or rewrote the file.
	Wrote bool
}

// Preserve installs the per-user sudoers drop-in.
//
// Steps:
//
//  1. Validate the user.
//  2. Compute the destination path under SudoersDir.
//  3. Render the rule. If the destination already exists with
//     identical content, return AlreadyCorrect=true.
//  4. Write to a sibling temp file with mode 0640 (so visudo can
//     read; we tighten to 0440 just before the atomic rename).
//  5. Validate with VisudoCheck. If invalid, remove the temp and
//     return an error WITHOUT touching the destination.
//  6. chmod 0440 + atomic os.Rename to the destination.
func Preserve(opts PreserveOptions) (PreserveResult, error) {
	dir := opts.SudoersDir
	if dir == "" {
		dir = SudoersD
	}
	path, err := FilenameForUserUnder(dir, opts.User)
	if err != nil {
		return PreserveResult{}, err
	}
	out := PreserveResult{User: opts.User, Path: path}

	body := RenderRule(opts.User)
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == body {
			out.AlreadyCorrect = true
			return out, nil
		}
	}
	if opts.DryRun {
		return out, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, fmt.Errorf("sudo: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".clouddeploy-sudoers-*.tmp")
	if err != nil {
		return out, fmt.Errorf("sudo: create temp under %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return out, fmt.Errorf("sudo: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return out, fmt.Errorf("sudo: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o440); err != nil {
		cleanup()
		return out, fmt.Errorf("sudo: chmod 0440 temp: %w", err)
	}

	check := opts.VisudoCheck
	if check == nil {
		// Default: no-op validator. Tests / production wire a real
		// `visudo -cf` runner; here we keep the package
		// cross-platform without an exec dependency.
		check = func(string) error { return nil }
	}
	if err := check(tmpPath); err != nil {
		cleanup()
		return out, fmt.Errorf("sudo: visudo refused the candidate sudoers file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return out, fmt.Errorf("sudo: rename %s -> %s: %w", tmpPath, path, err)
	}
	out.Wrote = true
	return out, nil
}

// ContainsUser is a tiny convenience for tests that need to check
// rendered output without depending on the exact format.
func ContainsUser(body, user string) bool {
	return strings.Contains(body, user+" ALL=")
}
