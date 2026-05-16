//go:build windows

package runner

// geteuid on Windows returns -1 to signal "not applicable". The
// Sudo precheck treats any non-zero euid as "not root" so the
// runner refuses Sudo=true commands on a Windows developer host -
// which is what we want; Sudo=true is meaningful only on Linux.
func geteuid() int { return -1 }
