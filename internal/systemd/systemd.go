// Package systemd renders CloudDeploy's systemd unit files from
// templates under templates/systemd/ and applies them via
// `systemctl daemon-reload`.
//
// Milestone-4 scope. Templates currently shipped:
//
//   - sunshine-headless.service.tmpl
//   - kwin-realvt.service.tmpl
//   - plasma-shell-realvt.service.tmpl
//   - clouddeploy-continue.service.tmpl
//
// Stub for Milestone 1.
package systemd

// Apply renders and writes all unit files for the given profile.
// NOT YET IMPLEMENTED.
func Apply() error { return nil }
