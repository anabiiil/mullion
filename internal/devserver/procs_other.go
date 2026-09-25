//go:build !windows

package devserver

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// killTree terminates the process group (proc.Detach uses Setsid, so
// the group leader's pid doubles as the pgid — this takes the whole
// npm → framework → workers tree down).
func killTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// listeningPorts lists the TCP ports the process group is listening on.
func listeningPorts(pid int) []int {
	// -g selects by process group id — the whole npm/node tree at once.
	out, err := exec.Command("lsof", "-g", strconv.Itoa(pid),
		"-aPn", "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var ports []int
	for _, line := range strings.Split(string(out), "\n") {
		// lines look like: n*:5173 or n127.0.0.1:5173
		if !strings.HasPrefix(line, "n") {
			continue
		}
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(line[i+1:]))
		if err != nil || port <= 0 || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports
}
