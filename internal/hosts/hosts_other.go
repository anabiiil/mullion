//go:build !windows && !darwin

package hosts

import "fmt"

func filePath() string { return "/etc/hosts" }

func toNative(s string) string { return s }

func writeElevated(path, content string) error {
	return fmt.Errorf("cannot write %s: permission denied (run with sudo)", path)
}
