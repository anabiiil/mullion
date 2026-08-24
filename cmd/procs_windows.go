//go:build windows

package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"pm/internal/proc"
)

// portOwner resolves which process listens on the port.
func portOwner(port int) (int, string) {
	script := fmt.Sprintf(
		`$p = (Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess; if ($p) { "$p " + (Get-Process -Id $p -ErrorAction SilentlyContinue).Name }`,
		port)
	out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return 0, ""
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return 0, ""
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, ""
	}
	name := "unknown"
	if len(fields) > 1 {
		name = fields[1] + ".exe"
	}
	return pid, name
}

// killWithParent stops the process, and — for supervisor architectures
// like mysqld's monitor — also its parent when it's the same executable.
func killWithParent(pid int) {
	script := fmt.Sprintf(`
$p = Get-Process -Id %d -ErrorAction SilentlyContinue
if ($p) {
  $parentId = (Get-CimInstance Win32_Process -Filter "ProcessId=%d").ParentProcessId
  Stop-Process -Id %d -Force -ErrorAction SilentlyContinue
  $par = Get-Process -Id $parentId -ErrorAction SilentlyContinue
  if ($par -and $par.Name -eq $p.Name) { Stop-Process -Id $parentId -Force -ErrorAction SilentlyContinue }
}`, pid, pid, pid)
	_ = proc.Quiet("powershell", "-NoProfile", "-Command", script).Run()
}

// processesUnder lists PIDs of running processes whose executable lives
// under dir (optionally restricted to one image name). This is how the
// uninstall distinguishes Mullion's servers from Laragon's.
func processesUnder(dir, image string) []int {
	pattern := strings.ReplaceAll(dir, "'", "''") + `\*`
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -like '%s' } | ForEach-Object { "$($_.ProcessId) $($_.Name)" }`,
		pattern)
	out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if image != "" && !strings.EqualFold(fields[1], image) {
			continue
		}
		var pid int
		if _, err := fmt.Sscan(fields[0], &pid); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killProcess force-stops one process by pid.
func killProcess(pid int) {
	_ = proc.Quiet("taskkill", "/F", "/PID", fmt.Sprint(pid)).Run()
}

// openURL opens a web page in the default browser.
func openURL(url string) {
	_ = proc.Quiet("cmd", "/c", "start", "", url).Start()
}

// scheduleHomeRemoval starts a detached helper that deletes the install
// directory once every Mullion process has exited — this very process
// still runs from bin, so it cannot delete it itself.
func scheduleHomeRemoval(home string) error {
	script := fmt.Sprintf(`
$dir = '%s'
for ($i = 0; $i -lt 120; $i++) {
  $running = Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -like ($dir + '\*') }
  if (-not $running) { break }
  Start-Sleep -Seconds 1
}
Start-Sleep -Seconds 1
Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue`,
		strings.ReplaceAll(home, "'", "''"))
	cmd := proc.Quiet("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", script)
	proc.DetachHiddenConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("scheduling removal of %s: %w", home, err)
	}
	_ = cmd.Process.Release()
	return nil
}

// conflictExample names the usual suspect in the port-conflict warning.
const conflictExample = " (e.g. quit Laragon)"

// printStackHint has nothing extra to add on Windows — killing the
// process is usually enough there.
func printStackHint(conflicts []portConflict) {}
