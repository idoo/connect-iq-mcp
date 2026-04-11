//go:build !windows

package state

// isBusy is false off Windows: a failed rename there is not transient.
func isBusy(error) bool { return false }
