// Package kwin owns the patched-KWin NVIDIA private HDR pipeline.
//
// Milestone-4 scope:
//   - Apply patches/kwin-clouddeploy-nvidia-private-hdr.patch.
//   - Build + install Plasma 6 KWin from source on Ubuntu 25.10.
//   - Apt-pin the patched packages so unattended-upgrades does not
//     replace them.
//   - Drop the patched-KWin marker under /var/lib/clouddeploy/.
//   - Wire KWIN_CLOUDDEPLOY_NVIDIA_PRIVATE_HDR env into the compositor
//     unit.
//
// See docs/HDR-NVIDIA-PRIVATE.md for the design and the matrix of DRM
// properties we expect (and intentionally do NOT touch).
//
// Stub for Milestone 1.
package kwin

// Install patches, builds, and installs KWin with the CloudDeploy
// NVIDIA private HDR path enabled. NOT YET IMPLEMENTED.
func Install() error { return nil }
