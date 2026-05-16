// Package apt owns dpkg/apt transaction wrapping for CloudDeploy.
//
// Motivating failure mode (RTX 5090 deploy, v2): during base package
// install,
//
//	power-profiles-daemon.postinst
//	  -> deb-systemd-invoke start power-profiles-daemon.service
//	     -> systemctl --quiet --system start power-profiles-daemon.service
//
// blocked on a systemd job queue that was itself waiting for boot
// jobs that needed the dpkg lock the postinst was holding. Classic
// deadlock.
//
// The standard Debian fix is /usr/sbin/policy-rc.d: a binary that
// returns exit code 101 disables service start during package
// install. We install one for the duration of the apt transaction,
// preserving any pre-existing one, and remove ours (or restore the
// original) when done.
//
// This file implements the policy-rc.d guard, table-tested with the
// three precondition scenarios from the v3 brief:
//
//  1. No existing /usr/sbin/policy-rc.d:
//     - install ours
//     - on cleanup, remove it
//  2. Pre-existing /usr/sbin/policy-rc.d we don't own:
//     - back it up (move to .clouddeploy-backup)
//     - install ours
//     - on cleanup, remove ours, restore backup
//  3. Pre-existing /usr/sbin/policy-rc.d we do own (stale CloudDeploy
//     file from a previous crashed run):
//     - keep it (it's already correct)
//     - on cleanup, remove it
//
// The Guard is created via InstallPolicyRcD(env) and released via
// Guard.Restore(env). Tests inject a fake fs Env so the same code is
// exercised without touching the real filesystem.
package apt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PolicyRcDPath is the canonical path for the Debian policy helper.
const PolicyRcDPath = "/usr/sbin/policy-rc.d"

// policyRcDBackup is where we stash a pre-existing helper while ours
// is in place.
const policyRcDBackup = "/usr/sbin/policy-rc.d.clouddeploy-backup"

// clouddeployMarker is the first line of our policy-rc.d so we can
// detect a stale CloudDeploy file from a crashed prior run.
const clouddeployMarker = "# clouddeploy-policy-rc-d"

// policyRcDBody is the entire content of our policy helper. Exit 101
// is the documented "do not run this action" return code (see
// invoke-rc.d(8) in Debian Policy).
const policyRcDBody = `#!/bin/sh
# clouddeploy-policy-rc-d
# Installed by clouddeployctl for the duration of an apt transaction.
# Exit 101 = "policy says: do not run this action".
# Prevents service-start postinst hooks from deadlocking on boot jobs.
exit 101
`

// FS is the file-system surface Guard uses. Production wires it to
// realFS; tests wire it to an in-memory implementation.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Remove(path string) error
	Rename(oldpath, newpath string) error
	MkdirAll(path string, perm fs.FileMode) error
}

// Env carries dependencies the guard needs. Only FS is required;
// other fields are reserved for the larger apt transaction wrapper
// that lands in Milestone 2.
type Env struct {
	FS FS
}

// GuardKind records what InstallPolicyRcD did so Restore can undo it
// correctly.
type GuardKind int

const (
	// GuardCreated: there was no policy-rc.d on disk; we created ours.
	// Restore will remove ours.
	GuardCreated GuardKind = iota

	// GuardBackedUp: a non-CloudDeploy policy-rc.d existed; we moved
	// it to .clouddeploy-backup, then wrote ours. Restore will remove
	// ours and rename the backup back.
	GuardBackedUp

	// GuardAdoptedStale: a stale CloudDeploy policy-rc.d already
	// existed (e.g. previous run crashed before Restore). We did not
	// touch it because its contents are already correct. Restore will
	// remove it.
	GuardAdoptedStale
)

// Guard is the handle returned by InstallPolicyRcD. Always call
// Restore (typically via defer) so the on-disk state ends up the way
// we found it.
type Guard struct {
	kind GuardKind
	env  *Env
}

// Kind reports what InstallPolicyRcD did. Useful in logs / tests.
func (g *Guard) Kind() GuardKind { return g.kind }

// InstallPolicyRcD installs the policy helper, capturing what was on
// disk so Restore can undo the change cleanly.
func InstallPolicyRcD(env *Env) (*Guard, error) {
	if env == nil || env.FS == nil {
		return nil, errors.New("apt: InstallPolicyRcD requires a non-nil Env.FS")
	}
	if err := env.FS.MkdirAll(filepath.Dir(PolicyRcDPath), 0o755); err != nil {
		return nil, fmt.Errorf("apt: mkdir %s: %w", filepath.Dir(PolicyRcDPath), err)
	}

	_, statErr := env.FS.Stat(PolicyRcDPath)
	switch {
	case statErr == nil:
		// File exists.
		body, readErr := env.FS.ReadFile(PolicyRcDPath)
		if readErr != nil {
			return nil, fmt.Errorf("apt: read existing %s: %w", PolicyRcDPath, readErr)
		}
		if isClouddeployBody(body) {
			// Stale CloudDeploy file from a crashed prior run. Adopt
			// it - the content is correct - and remove on Restore.
			return &Guard{kind: GuardAdoptedStale, env: env}, nil
		}
		// Real pre-existing file. Back it up and overwrite.
		if err := env.FS.Rename(PolicyRcDPath, policyRcDBackup); err != nil {
			return nil, fmt.Errorf("apt: rename %s -> %s: %w", PolicyRcDPath, policyRcDBackup, err)
		}
		if err := env.FS.WriteFile(PolicyRcDPath, []byte(policyRcDBody), 0o755); err != nil {
			// Best-effort restore of the backup so we don't leave the
			// system without any policy-rc.d at all.
			_ = env.FS.Rename(policyRcDBackup, PolicyRcDPath)
			return nil, fmt.Errorf("apt: write %s: %w", PolicyRcDPath, err)
		}
		return &Guard{kind: GuardBackedUp, env: env}, nil

	case errors.Is(statErr, fs.ErrNotExist):
		// No file. Write ours.
		if err := env.FS.WriteFile(PolicyRcDPath, []byte(policyRcDBody), 0o755); err != nil {
			return nil, fmt.Errorf("apt: write %s: %w", PolicyRcDPath, err)
		}
		return &Guard{kind: GuardCreated, env: env}, nil

	default:
		// Something else - permission denied, etc. Surface the error.
		return nil, fmt.Errorf("apt: stat %s: %w", PolicyRcDPath, statErr)
	}
}

// Restore reverses InstallPolicyRcD. Safe to call multiple times
// (idempotent within a single guard instance).
func (g *Guard) Restore() error {
	if g == nil || g.env == nil {
		return nil
	}
	switch g.kind {
	case GuardCreated, GuardAdoptedStale:
		// Remove ours. If it isn't there anymore (someone else
		// removed it), that's fine.
		if err := g.env.FS.Remove(PolicyRcDPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("apt: remove %s: %w", PolicyRcDPath, err)
		}
		return nil
	case GuardBackedUp:
		// Remove ours, then rename the backup back. If our file is
		// already gone, still try to restore the backup.
		_ = g.env.FS.Remove(PolicyRcDPath)
		if err := g.env.FS.Rename(policyRcDBackup, PolicyRcDPath); err != nil {
			return fmt.Errorf("apt: rename backup %s -> %s: %w", policyRcDBackup, PolicyRcDPath, err)
		}
		return nil
	}
	return fmt.Errorf("apt: Restore: unknown GuardKind %d", g.kind)
}

func isClouddeployBody(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	// Look for our marker line near the top.
	for i := 0; i < len(b) && i < 256; i++ {
		if b[i] == '\n' {
			line := string(b[:i])
			if line == "#!/bin/sh" {
				// Could be ours - keep scanning for the marker.
				continue
			}
			if line == clouddeployMarker {
				return true
			}
			continue
		}
	}
	// Fall back to substring search; cheaper than parsing line by line.
	if has(b, []byte(clouddeployMarker)) {
		return true
	}
	return false
}

func has(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
outer:
	for i := 0; i <= len(haystack)-len(needle); i++ {
		for j := range needle {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// realFS - production FS implementation. Tests substitute an in-memory FS.
// -----------------------------------------------------------------------------

// RealFS is the production filesystem.
type RealFS struct{}

func (RealFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (RealFS) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (RealFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (RealFS) Remove(path string) error                       { return os.Remove(path) }
func (RealFS) Rename(oldpath, newpath string) error           { return os.Rename(oldpath, newpath) }
func (RealFS) MkdirAll(path string, perm fs.FileMode) error   { return os.MkdirAll(path, perm) }
