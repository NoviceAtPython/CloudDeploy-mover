//go:build windows

package runner

import "os/exec"

func prepareCommandForTreeKill(cmd *exec.Cmd) {
	_ = cmd
}

func terminateProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func forceKillProcessTree(cmd *exec.Cmd) {
	terminateProcessTree(cmd)
}
