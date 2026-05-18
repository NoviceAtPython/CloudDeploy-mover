package cuda

import (
	"fmt"
	"strings"
)

// Method controls *how* the toolkit is installed. Distinct from Mode,
// which controls *whether* failure stops the deploy.
type Method string

const (
	// MethodAuto: try apt first (when a CUDA apt repo is available
	// for the host Ubuntu), runfile otherwise.
	MethodAuto Method = "auto"
	// MethodApt: force the NVIDIA CUDA apt repo path. If no official
	// repo is available for the host Ubuntu, Plan + phase fail with
	// a clear error (mode-aware: required = fatal).
	MethodApt Method = "apt"
	// MethodRunfile: force the toolkit-only NVIDIA runfile path.
	MethodRunfile Method = "runfile"
	// MethodNone: do not install. Equivalent to mode=none from the
	// install side. State still reflects the configured mode + method
	// so doctor can explain what happened.
	MethodNone Method = "none"
)

// ParseMethod normalizes the YAML knob. Empty defaults to MethodAuto.
func ParseMethod(s string) (Method, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return MethodAuto, nil
	case "apt":
		return MethodApt, nil
	case "runfile":
		return MethodRunfile, nil
	case "none":
		return MethodNone, nil
	}
	return "", fmt.Errorf("cuda: unknown method %q (want auto|apt|runfile|none)", s)
}
