//go:build !windows

package config

// isBusy is Windows-only: POSIX rename never blocks readers.
func isBusy(error) bool { return false }
