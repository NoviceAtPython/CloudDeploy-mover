//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecTimeoutKillsProcessGroupChild(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "orphan-smoke.sh")
	body := `#!/bin/sh
( trap '' TERM; while :; do sleep 1; done ) &
echo $! > ` + shellQuote(pidfile) + `
trap '' TERM
wait
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	start := time.Now()
	res := newTestRunner().Exec(context.Background(), CommandSpec{
		Argv:    []string{script},
		Timeout: 200 * time.Millisecond,
		LogFile: "-",
	})
	if !res.TimedOut {
		t.Fatalf("expected timeout, got err=%v exit=%d stdout=%q stderr=%q", res.Err, res.ExitCode, res.Stdout, res.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout cleanup took too long: %s", elapsed)
	}

	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("child pidfile missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", b, err)
	}
	for i := 0; i < 20 && processAlive(pid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("child process %d survived process-group timeout kill", pid)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
