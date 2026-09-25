//go:build !windows

package elevate

import "fmt"

// IsElevated always reports true off Windows: nothing in the setup flow
// needs privilege escalation there.
func IsElevated() bool { return true }

func Relaunch(exe string, args ...string) error {
	return fmt.Errorf("elevation is only supported on Windows")
}

func RelaunchAsync(exe string, args ...string) error {
	return fmt.Errorf("elevation is only supported on Windows")
}
