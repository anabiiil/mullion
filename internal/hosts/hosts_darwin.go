//go:build darwin

package hosts

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"pm/internal/tty"
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
	if err := RunElevated("to update /etc/hosts", script); err != nil {
		return fmt.Errorf("elevated hosts update failed: %w", err)
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

// RunElevated runs a /bin/sh script as root: through sudo when a
// terminal can take the password, otherwise through the macOS
// administrator dialog (e.g. launched from the control panel). reason
// completes "Password (...)", e.g. "to update /etc/hosts".
func RunElevated(reason, script string) error {
	if stdinIsTerminal() {
		fmt.Printf("This needs administrator rights (%s) — you may be asked for your password.\n", reason)
		cmd := exec.Command("sudo", "-p", "Password ("+reason+"): ", "/bin/sh", "-c", script)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	// No terminal to type a sudo password into: use the system
	// authorization dialog instead.
	osa := fmt.Sprintf("do shell script %q with administrator privileges", script)
	if out, err := exec.Command("osascript", "-e", osa).CombinedOutput(); err != nil {
		return fmt.Errorf("%v (dialog dismissed?): %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stdinIsTerminal() bool {
	return tty.IsTerminal(os.Stdin)
}
