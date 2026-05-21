// Package discovery contains tiny runtime tool resolvers used by
// phases that must survive distro/package naming drift.
package discovery

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Command is the selected executable and optional version evidence.
type Command struct {
	Name    string
	Path    string
	Version string
}

// Resolver is injectable for tests. The zero value uses the host PATH.
type Resolver struct {
	LookPath  func(string) (string, error)
	VersionFn func(string) string
}

// ResolveCommand returns the first executable in preferred order.
// Entries may be bare command names or absolute paths.
func (r Resolver) ResolveCommand(preferred []string) (Command, error) {
	look := r.LookPath
	if look == nil {
		look = exec.LookPath
	}
	var tried []string
	for _, raw := range preferred {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		tried = append(tried, name)
		path := ""
		var err error
		if filepath.IsAbs(name) {
			if st, statErr := os.Stat(name); statErr == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				path = name
			} else {
				err = statErr
			}
		} else {
			path, err = look(name)
		}
		if path == "" || err != nil {
			continue
		}
		cmd := Command{Name: filepath.Base(name), Path: path}
		if r.VersionFn != nil {
			cmd.Version = strings.TrimSpace(r.VersionFn(path))
		}
		return cmd, nil
	}
	if len(tried) == 0 {
		return Command{}, errors.New("no commands requested")
	}
	return Command{}, fmt.Errorf("none of the requested commands were found: %s", strings.Join(tried, ", "))
}

// MustQDBus returns the Qt DBus command preference used by KDE/KWin
// phases. Plasma 6 images often only ship qdbus6.
func MustQDBus(r Resolver) (Command, error) {
	cmd, err := r.ResolveCommand([]string{"qdbus6", "qdbus", "qdbus-qt5", "/usr/lib/qt6/bin/qdbus", "/usr/lib/qt5/bin/qdbus"})
	if err != nil {
		return Command{}, fmt.Errorf("missing Qt DBus tool; install qdbus6 or qdbus: %w", err)
	}
	return cmd, nil
}
