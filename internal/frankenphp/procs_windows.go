//go:build windows

package frankenphp

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
