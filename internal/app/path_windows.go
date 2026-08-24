//go:build windows

package app

import (
	"fmt"
	"os/exec"
	"strings"
)

// EnsureUserPath appends dirs to the user's PATH environment variable
// (persistently, via the registry) when they're not already present.
// Uses PowerShell's SetEnvironmentVariable, which is safe for long PATHs
// (unlike `setx`, which truncates at 1024 characters).
func EnsureUserPath(dirs ...string) error {
	var adds []string
	for _, d := range dirs {
		adds = append(adds, psQuote(d))
	}
	script := fmt.Sprintf(`
$dirs = @(%s)
$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $path) { $path = '' }
$parts = $path -split ';' | Where-Object { $_ -ne '' }
$changed = $false
foreach ($d in $dirs) {
  if ($parts -notcontains $d) { $parts += $d; $changed = $true }
}
if ($changed) {
  [Environment]::SetEnvironmentVariable('Path', ($parts -join ';'), 'User')
  Write-Output 'updated'
}`, strings.Join(adds, ", "))

	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("updating user PATH: %v: %s", err, out)
	}
	if strings.Contains(string(out), "updated") {
		fmt.Println("Added to your PATH — open a NEW terminal for it to take effect.")
	}
	return nil
}

// EnsureMachinePathPriority puts dirs at the FRONT of the system PATH,
// ahead of other stacks' entries (Laragon, XAMPP, ...): Windows searches
// the system PATH before the user PATH, so this is the only way
// Mullion's php wins when another stack registered itself system-wide.
// Requires administrator rights.
func EnsureMachinePathPriority(dirs ...string) error {
	var adds []string
	for _, d := range dirs {
		adds = append(adds, psQuote(d))
	}
	script := fmt.Sprintf(`
$dirs = @(%s)
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if ($null -eq $path) { $path = '' }
$parts = $path -split ';' | Where-Object { $_ -ne '' -and $dirs -notcontains $_ }
$new = ($dirs + $parts) -join ';'
if ($new -ne $path) {
  [Environment]::SetEnvironmentVariable('Path', $new, 'Machine')
  Write-Output 'updated'
}`, strings.Join(adds, ", "))

	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("updating system PATH: %v: %s", err, out)
	}
	if strings.Contains(string(out), "updated") {
		fmt.Println("Mullion now has priority on the system PATH (ahead of Laragon/XAMPP).")
	}
	return nil
}

// RemovePathEntries strips dirs from the given scope's PATH
// ("User" or "Machine" — the latter needs administrator rights).
func RemovePathEntries(scope string, dirs ...string) error {
	var removes []string
	for _, d := range dirs {
		removes = append(removes, psQuote(d))
	}
	script := fmt.Sprintf(`
$dirs = @(%s)
$path = [Environment]::GetEnvironmentVariable('Path', '%s')
if ($null -eq $path) { exit 0 }
$parts = $path -split ';' | Where-Object { $_ -ne '' -and $dirs -notcontains $_ }
[Environment]::SetEnvironmentVariable('Path', ($parts -join ';'), '%s')`,
		strings.Join(removes, ", "), scope, scope)

	if out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput(); err != nil {
		return fmt.Errorf("cleaning %s PATH: %v: %s", scope, err, out)
	}
	return nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// DisableShadowEntries is a no-op on Windows: PATH priority is fixed via
// the machine PATH during setup instead of profile-file edits.
func DisableShadowEntries(shadowExe string) ([]string, error) { return nil, nil }
