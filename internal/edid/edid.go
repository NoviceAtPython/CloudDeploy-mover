// Package edid generates and installs forced-mode EDID binaries for
// CloudDeploy's headless / forced-connector path.
//
// Milestone-4 scope: invoke helpers/write-edids.py, write to
// /lib/firmware/edid/, wire into /etc/default/grub via
// drm.edid_firmware=...
//
// A v3.1+ candidate is to port helpers/write-edids.py to Rust using
// edid-rs (see docs/ARCHITECTURE.md). Not now.
//
// Stub for Milestone 1.
package edid

// Write generates the requested EDID + drops it under /lib/firmware/edid/.
// NOT YET IMPLEMENTED.
func Write() error { return nil }
