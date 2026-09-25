//go:build windows

package devserver

import (
	"fmt"
	"strconv"
	"strings"

	"pm/internal/proc"
)

func processAlive(pid int) bool {
	out, err := proc.Quiet("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	return err == nil && strings.Contains(string(out), strconv.Itoa(pid))
}

// killTree terminates the process and its whole child tree.
func killTree(pid int) {
	_ = proc.Quiet("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// listeningPorts lists the TCP ports the process tree is listening on.
func listeningPorts(pid int) []int {
	script := fmt.Sprintf(`
$root = %d
$pids = @($root)
$all = Get-CimInstance Win32_Process
do {
  $new = @($all | Where-Object { $pids -contains $_.ParentProcessId -and $pids -notcontains $_.ProcessId } | ForEach-Object { $_.ProcessId })
  $pids += $new
} while ($new.Count -gt 0)
Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
  Where-Object { $pids -contains $_.OwningProcess } |
  ForEach-Object { $_.LocalPort } | Sort-Object -Unique`, pid)
	out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil
	}
	var ports []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if port, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && port > 0 {
			ports = append(ports, port)
		}
	}
	return ports
}
