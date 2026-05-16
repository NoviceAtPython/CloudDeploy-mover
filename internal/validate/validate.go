// Package validate hosts the post-deploy validators: doctor checks
// (read-only, run any time) and end-to-end checks (require Moonlight
// connect for the HDR stream validator).
//
// Milestone-4 scope. The HDR stream validator mirrors the v2 helper
// /usr/local/sbin/clouddeploy-validate-hdr-stream: grep the Sunshine
// journal for the success markers documented in
// docs/final-hdr-success/KNOWN_GOOD_SUNSHINE_STATE.md.
//
// Stub for Milestone 1.
package validate

// HDRStream greps the Sunshine journal for the HDR success markers.
// NOT YET IMPLEMENTED.
func HDRStream() error { return nil }
