//go:build windows

package cmd

// maybeExecFresher is a no-op on Windows: installs there are standalone
// copies updated by setup/selfUpdateIfNeeded, not package managers.
func maybeExecFresher() {}
