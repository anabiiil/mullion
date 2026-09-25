//go:build !windows

package junction

import "os"

// Non-Windows fallback used for development and tests: a plain symlink.
func Set(linkPath, targetDir string) error {
	if _, err := os.Lstat(linkPath); err == nil {
		if err := os.Remove(linkPath); err != nil {
			return err
		}
	}
	return os.Symlink(targetDir, linkPath)
}

func Target(linkPath string) string {
	t, err := os.Readlink(linkPath)
	if err != nil {
		return ""
	}
	return t
}
