//go:build !windows

package state

import (
	"os"
	"syscall"
)

// processAliveImpl on Unix: send signal 0 to the PID and look at the
// return. Nil = the process exists. ESRCH = no such process. EPERM
// would also indicate the process is alive (we just can't signal it)
// but in practice CloudDeploy runs as root so EPERM is rare; treat
// it as "alive" anyway.
func processAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM = process exists but we can't signal it. Treat as alive.
	if err == syscall.EPERM {
		return true
	}
	return false
}
