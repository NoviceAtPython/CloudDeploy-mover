package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultLockPath is the canonical lock-file location. Sits next to
// state.json so a `find` for "what owns this deploy" stays simple.
const DefaultLockPath = "/var/lib/clouddeploy/state.lock"

// ErrLocked is returned by Acquire when another process holds the lock.
var ErrLocked = errors.New("state: lock is held by another clouddeployctl process")

// Lock represents an acquired exclusive lock. Always Release.
type Lock struct {
	path string
	pid  int
}

// Path returns the lock-file path on disk.
func (l *Lock) Path() string { return l.path }

// PID returns the holder's pid.
func (l *Lock) PID() int { return l.pid }

// Acquire takes an exclusive process-level lock. The implementation
// is intentionally simple: a sentinel file under /var/lib/clouddeploy/
// with the holder's PID. We do NOT use flock(2) because:
//
//   - On Linux flock is cooperative anyway; competing CloudDeploy
//     invocations all run the same binary, so a PID-file race check
//     gives us the same protection with no syscall portability
//     concerns.
//   - We need to read the PID out for `doctor` / error messages even
//     when the lock is held by a crashed prior process; a stale-PID
//     check fits naturally into the PID-file model.
//
// Acquire is therefore: create-with-O_EXCL, write our pid, return
// Lock. If create fails because the file exists, read the existing
// pid; if that process is dead, steal the lock (overwrite the file).
func Acquire(path string) (*Lock, error) {
	if path == "" {
		path = DefaultLockPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("state: mkdir for lock: %w", err)
	}
	myPID := os.Getpid()

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", myPID)
			_ = f.Close()
			return &Lock{path: path, pid: myPID}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("state: create lock %s: %w", path, err)
		}
		// Lock exists. Read pid; if the holder is dead, steal it.
		holder, readErr := readPID(path)
		if readErr != nil || holder == 0 {
			// Stale or unreadable. Force-remove and retry once.
			_ = os.Remove(path)
			continue
		}
		if !processAlive(holder) {
			// Holder is dead. Force-remove and retry.
			_ = os.Remove(path)
			continue
		}
		return nil, fmt.Errorf("%w (held by pid %d, lock=%s). "+
			"Inspect the holder with: ps -fp %d  |  journalctl -u clouddeploy-v3-continue.service -n 200 --no-pager. "+
			"If the holder is gone, remove %s and retry.",
			ErrLocked, holder, path, holder, path)
	}
	return nil, fmt.Errorf("state: could not acquire lock %s after stealing a stale entry", path)
}

// Release removes the lock file. Safe to call multiple times; only
// errors when the file exists AND we cannot remove it.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: release lock: %w", err)
	}
	return nil
}

// LockInfo describes what `doctor lock` sees on disk. Used by the
// `clouddeployctl doctor lock` subcommand to give the operator a
// clear "is the lock real or stale" diagnostic.
type LockInfo struct {
	Path       string
	Exists     bool
	PID        int
	ReadErr    error
	HolderLive bool
}

// Inspect is a read-only probe that does NOT take or release any
// lock. The CLI's `doctor lock` calls this so we never accidentally
// fight with a real holder while diagnosing.
func Inspect(path string) LockInfo {
	if path == "" {
		path = DefaultLockPath
	}
	out := LockInfo{Path: path}
	if _, err := os.Stat(path); err != nil {
		return out
	}
	out.Exists = true
	pid, err := readPID(path)
	if err != nil {
		out.ReadErr = err
		return out
	}
	out.PID = pid
	out.HolderLive = processAlive(pid)
	return out
}

// readPID reads "pid\n" from path and returns the integer.
func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// processAlive returns true if the PID corresponds to a running
// process on this host. Per-OS implementation lives in
// signal_unix.go / signal_windows.go.
func processAlive(pid int) bool {
	return processAliveImpl(pid)
}
