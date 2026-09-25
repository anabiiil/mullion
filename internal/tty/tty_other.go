//go:build !darwin && !linux && !windows

package tty

import "os"

// IsTerminal falls back to the ModeCharDevice heuristic on platforms
// without a dedicated ioctl/console-mode implementation above. It is
// less accurate (e.g. /dev/null also reports as a character device),
// but keeps the module building everywhere.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
