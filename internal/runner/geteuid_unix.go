//go:build !windows

package runner

import "os"

// geteuid returns os.Geteuid on Unix.
func geteuid() int { return os.Geteuid() }
