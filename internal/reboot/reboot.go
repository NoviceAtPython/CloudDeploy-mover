// Package reboot owns the continuation systemd service + reboot
// scheduling. Only this package ever calls `systemctl reboot` so all
// reboot logic is in one place.
//
// Milestone-3 scope. Mirrors v2's install_continuation_service /
// schedule_reboot_for_continuation.
//
// Stub for Milestone 1.
package reboot

// Schedule arranges for the next boot to resume `clouddeployctl
// resume`. NOT YET IMPLEMENTED.
func Schedule() error { return nil }
