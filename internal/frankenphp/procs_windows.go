//go:build windows

package frankenphp

import (
	"fmt"
	"strconv"

	"pm/internal/proc"
)

func processAlive(pid int) bool {
	out, err := proc.Quiet("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	return err == nil && proc.TasklistHasPID(out, pid)
}

// killTree terminates the process and its whole child tree.
func killTree(pid int) {
	_ = proc.Quiet("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// forceKillTree retries killTree: taskkill /F is already forceful.
func forceKillTree(pid int) { killTree(pid) }
