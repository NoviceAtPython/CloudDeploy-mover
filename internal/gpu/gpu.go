// Package gpu detects GPU(s) on the host via lspci -nn and exposes
// the typed result the rest of CloudDeploy consults.
//
// Milestone-2 scope: full lspci parsing, multi-GPU handling, and
// (eventually) per-GPU profile selection. The skeleton here is just
// enough to surface the API.
package gpu

// GPU is one detected graphics device.
type GPU struct {
	PCIID  string
	Name   string
	Vendor string
}

// Detect returns the GPUs lspci reports on this host. NOT YET
// IMPLEMENTED in v1; internal/nvidia.GatherEvidenceFromHost contains
// the lspci shell-out for the nvidia-driver phase.
func Detect() ([]GPU, error) {
	return nil, nil
}
