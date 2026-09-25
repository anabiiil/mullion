//go:build !windows && !darwin

package hosts

import "fmt"

func filePath() string { return "/etc/hosts" }

func toNative(s string) string { return s }

func writeElevated(path, content string) error {
	return fmt.Errorf("cannot write %s: permission denied (run with sudo)", path)
}

// RunElevated is unsupported here: Mullion never prompts for root on
// this OS — the user runs the privileged step with sudo themselves.
func RunElevated(reason, script string) error {
	return fmt.Errorf("administrator rights are needed %s — run the step with sudo", reason)
}
