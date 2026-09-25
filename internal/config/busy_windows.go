//go:build windows

package config

import (
	"errors"
	"syscall"
)

// isBusy reports a transient Windows lock: a sharing violation, or
// access denied while another handle is renaming/replacing the file.
func isBusy(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	const errorSharingViolation, errorLockViolation = 32, 33
	return errno == errorSharingViolation || errno == errorLockViolation || errno == syscall.ERROR_ACCESS_DENIED
}
