package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// filepathBase is a 1-line shim so main.go can call filepathBase
// instead of importing path/filepath directly.
func filepathBase(p string) string { return filepath.Base(p) }

// execLookPath wraps os/exec.LookPath.
func execLookPath(name string) (string, error) { return exec.LookPath(name) }

// readSmall reads a small file (up to ~4 KiB) and returns its
// trimmed contents. Used by doctor system to read /proc files.
// Empty string on any error so the report stays readable.
func readSmall(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(missing)"
	}
	if len(b) > 4096 {
		b = b[:4096]
	}
	return strings.TrimSpace(string(b))
}

// lsbRelease reads /etc/os-release and returns the requested field
// (e.g. "ID" or "VERSION_ID"). Returns "(missing)" on any error.
func lsbRelease(field string) string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "(missing)"
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		if line[:eq] == field {
			v := line[eq+1:]
			// Strip surrounding quotes if any.
			v = strings.Trim(v, `"'`)
			return v
		}
	}
	return "(missing)"
}
