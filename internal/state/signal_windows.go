//go:build windows

package state

import (
	"os"
	"syscall"
	"unsafe"
)

// processAliveImpl on Windows: open the process with limited access
// and ask for its exit code. The Win32 API GetExitCodeProcess
// returns STILL_ACTIVE (259) for a running process and a real exit
// code for a finished one. We deliberately bypass os.Process.Signal
// which on Windows only supports Kill / Interrupt.
//
// We use direct syscall.Open... helpers from the syscall package to
// avoid depending on golang.org/x/sys.
func processAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	const (
		PROCESS_QUERY_LIMITED_INFORMATION uint32 = 0x1000
		STILL_ACTIVE                      uint32 = 259
	)
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return false
	}
	openProcess, err := kernel32.FindProc("OpenProcess")
	if err != nil {
		return false
	}
	getExitCodeProcess, err := kernel32.FindProc("GetExitCodeProcess")
	if err != nil {
		return false
	}
	closeHandle, err := kernel32.FindProc("CloseHandle")
	if err != nil {
		return false
	}

	hRaw, _, _ := openProcess.Call(uintptr(PROCESS_QUERY_LIMITED_INFORMATION), 0, uintptr(pid))
	if hRaw == 0 {
		// OpenProcess returned NULL: either the process doesn't
		// exist, or we lack permission. Treat as dead. CloudDeploy
		// runs as root/admin in practice, so "we lack permission"
		// only happens on a developer host poking at someone else's
		// process - safe to treat as dead.
		return false
	}
	defer closeHandle.Call(hRaw)

	var exitCode uint32
	ret, _, _ := getExitCodeProcess.Call(hRaw, uintptr(unsafe.Pointer(&exitCode)))
	if ret == 0 {
		return false
	}
	return exitCode == STILL_ACTIVE
}

// keep os imported in case future code wants it.
var _ = os.Getpid
