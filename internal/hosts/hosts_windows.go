//go:build windows

package hosts

import (
	"fmt"
	"os"
	"pm/internal/proc"
	"path/filepath"
	"strings"
)

func filePath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}

func toNative(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// writeElevated stages the desired content in a temp file, then asks
// Windows (via a UAC prompt) to copy it over the hosts file and flush DNS.
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

	fmt.Println("Updating the hosts file needs administrator rights — please accept the UAC prompt.")
	inner := fmt.Sprintf("Copy-Item -Force '%s' '%s'; ipconfig /flushdns | Out-Null", tmpPath, path)
	if err := RunElevated("to update the hosts file", inner); err != nil {
		return fmt.Errorf("elevated hosts update failed (UAC declined?): %w", err)
	}

	// Verify the copy actually happened.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != content {
		return fmt.Errorf("hosts file was not updated; run your terminal as administrator and retry")
	}
	return nil
}

// RunElevated runs a PowerShell command as administrator through a UAC
// prompt and waits for it. reason is informational (shown by callers).
func RunElevated(reason, command string) error {
	ps := fmt.Sprintf(
		`Start-Process -Verb RunAs -Wait -WindowStyle Hidden powershell -ArgumentList '-NoProfile','-Command',%s`,
		psQuote(command),
	)
	cmd := proc.Quiet("powershell", "-NoProfile", "-Command", ps)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// PSQuote wraps s in PowerShell single quotes, escaping embedded quotes.
func PSQuote(s string) string { return psQuote(s) }

// psQuote wraps s in PowerShell single quotes, escaping embedded quotes.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
