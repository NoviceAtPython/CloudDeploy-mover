// Package sunshine owns Sunshine fork pin + build + dual-install +
// setcap + sunshine.conf generation + HDR env wiring.
//
// Milestone-4 scope. The state v3 must preserve verbatim:
//
//   - Pin to fork commit 464bccf1b6e33bf35138136c6138fd9851e6d906
//     (or the profile-supplied override).
//   - Source-build via the same cmake/ninja flow as v2.
//   - Install to BOTH /usr/local/bin/sunshine-clouddeploy and
//     /usr/local/bin/sunshine.
//   - setcap cap_sys_admin,cap_net_bind_service,cap_sys_nice+ep on
//     both binaries.
//   - Generate sunshine-headless.service with HDR env vars when the
//     profile sets ForceAV1HDR10 / SynthesizeHDR10Metadata.
//   - Do NOT write hdr=, fps=, or resolutions= to sunshine.conf.
//   - CSRF allowlist: localhost + 127.0.0.1 + Tailscale IP.
//
// See docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md for the
// authoritative success-state spec.
//
// Stub for Milestone 1.
package sunshine

// Install does the pin + build + dual-install + setcap + config
// write. NOT YET IMPLEMENTED.
func Install() error { return nil }
