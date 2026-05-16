// Package runner is the only place in clouddeployctl that calls
// os/exec. Every subprocess invocation goes through Exec so we get
// uniform logging, timeouts, and structured error wrapping.
//
// Milestone-1 stub. The full implementation lands in Milestone 2 once
// internal/state is wired into the phase machinery. The current
// surface is just enough to compile cmd/clouddeployctl/main.go.
package runner

import (
	"context"
	"errors"
)

// Exec runs cmd with args, captures stdout/stderr, and returns the
// exit status. NOT YET IMPLEMENTED.
func Exec(ctx context.Context, name string, args ...string) (string, error) {
	return "", errors.New("runner: not implemented yet (Milestone 2)")
}
