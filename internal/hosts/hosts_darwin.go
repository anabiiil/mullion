//go:build darwin

package hosts

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func filePath() string { return "/etc/hosts" }

func toNative(s string) string { return s }

// writeElevated stages the desired content in a temp file, then copies it
// over /etc/hosts with sudo (terminal) or a macOS admin prompt (no
// terminal, e.g. launched from the control panel), and flushes the DNS
// caches so the new names resolve immediately.
func writeElevated(path, content string) error {
	tmp, err := os.CreateTemp("", "mullion-hosts-*.txt")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	script := fmt.Sprintf("/bin/cp %q %q && /usr/bin/dscacheutil -flushcache && /usr/bin/killall -HUP mDNSResponder", tmpPath, path)
	if stdinIsTerminal() {
		fmt.Println("Updating /etc/hosts needs administrator rights — you may be asked for your password.")
		cmd := exec.Command("sudo", "-p", "Password (to update /etc/hosts): ", "/bin/sh", "-c", script)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("elevated hosts update failed: %w", err)
		}
	} else {
		// No terminal to type a sudo password into: use the system
		// authorization dialog instead.
		osa := fmt.Sprintf("do shell script %q with administrator privileges", script)
		if out, err := exec.Command("osascript", "-e", osa).CombinedOutput(); err != nil {
			return fmt.Errorf("elevated hosts update failed (dialog dismissed?): %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	// Verify the copy actually happened.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != content {
		return fmt.Errorf("/etc/hosts was not updated; retry, or edit it manually")
	}
	return nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
