//go:build !windows

package sysproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// portOwner resolves which process listens on the port.
func PortOwner(port int) (int, string) {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return 0, ""
	}
	pid, name := 0, "unknown"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == 0:
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "c") && pid != 0:
			name = line[1:]
			return pid, name
		}
	}
	return pid, name
}

// killWithParent stops the process: TERM first, KILL if it lingers.
// (The Windows implementation also stops a same-name parent — the
// mysqld monitor pattern — which doesn't exist on Unix.)
func KillWithParent(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// processesUnder lists PIDs of running processes whose executable lives
// under dir (optionally restricted to one image name). This is how the
// uninstall distinguishes Mullion's servers from someone else's.
func ProcessesUnder(dir, image string) []int {
	// On macOS `ps -o comm` prints the full executable path.
	out, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		exe := strings.Join(fields[1:], " ")
		if !strings.HasPrefix(exe, prefix) {
			continue
		}
		if image != "" && !strings.EqualFold(filepath.Base(exe), image) {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killProcess force-stops one process by pid.
func KillProcess(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// openURL opens a web page in the default browser.
func OpenURL(url string) {
	opener := "open" // macOS
	if _, err := exec.LookPath(opener); err != nil {
		opener = "xdg-open"
	}
	_ = exec.Command(opener, url).Start()
}

// scheduleHomeRemoval starts a detached helper that deletes the install
// directory once every Mullion process has exited — this very process
// still runs from bin, so it cannot delete it itself.
func ScheduleHomeRemoval(home string) error {
	// The [f]irst-char class keeps pgrep -f from matching the helper's
	// own command line (which contains the pattern itself).
	pattern := "[" + home[:1] + "]" + home[1:] + "/"
	script := fmt.Sprintf(`for i in $(seq 1 120); do pgrep -f %q >/dev/null || break; sleep 1; done; sleep 1; rm -rf %q`,
		pattern, home)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("scheduling removal of %s: %w", home, err)
	}
	_ = cmd.Process.Release()
	return nil
}

// conflictExample names the usual suspects in the port-conflict warning.
const ConflictExample = " (e.g. Valet's nginx, Homebrew services)"

// printStackHint explains how to stop stacks that launchd keeps
// resurrecting: killing Valet's nginx or a `brew services` mysql just
// brings it back, so the managing service must be stopped instead.
func PrintStackHint(conflicts []Conflict) {
	names := map[string]bool{}
	for _, c := range conflicts {
		names[strings.ToLower(filepath.Base(c.Name))] = true
	}
	var hints []string
	if names["nginx"] || names["httpd"] {
		hints = append(hints,
			"  valet stop                       # if the server belongs to Laravel Valet",
			"  brew services stop nginx         # if it runs via Homebrew (try with sudo too)")
	}
	if names["mysqld"] || names["mariadbd"] {
		hints = append(hints, "  brew services stop mysql         # Homebrew's MySQL (or mysql@<version>)")
	}
	if len(hints) == 0 && len(conflicts) > 0 {
		hints = append(hints, "  brew services list               # find and stop whatever manages it")
	}
	if len(hints) > 0 {
		fmt.Println("hint: on macOS these servers are often kept alive by launchd — stopping the manager is what sticks:")
		for _, h := range hints {
			fmt.Println(h)
		}
	}
}

// stopConflict stops a conflicting listener FOR GOOD: launchd- and
// brew-managed servers resurrect after a bare kill, so their manager
// (brew services, Valet) is stopped first, then the process itself.
func StopConflict(c Conflict) {
	stopManagedService(c.Name)
	KillWithParent(c.PID)
}

// stopManagedService stops whatever keeps the named server alive.
func stopManagedService(procName string) {
	base := strings.ToLower(filepath.Base(procName))
	var prefixes []string
	switch {
	case strings.Contains(base, "nginx"):
		prefixes = []string{"nginx"}
		// Valet manages its nginx (and dnsmasq) itself.
		if _, err := exec.LookPath("valet"); err == nil {
			fmt.Println("Stopping Laravel Valet (it manages that nginx)...")
			cmd := exec.Command("valet", "stop")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			_ = cmd.Run()
		}
	case strings.Contains(base, "mysqld"):
		prefixes = []string{"mysql", "percona-server"}
	case strings.Contains(base, "mariadbd"):
		prefixes = []string{"mariadb"}
	case strings.Contains(base, "httpd"):
		prefixes = []string{"httpd"}
	default:
		return
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return
	}
	out, err := exec.Command(brew, "services", "list").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "started" {
			continue
		}
		name := fields[0]
		match := false
		for _, p := range prefixes {
			if name == p || strings.HasPrefix(name, p+"@") {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		asRoot := len(fields) >= 3 && fields[2] == "root"
		fmt.Printf("Stopping Homebrew service %s (so it stays down)...\n", name)
		var cmd *exec.Cmd
		if asRoot {
			if !stdinIsTerminal() {
				fmt.Printf("note: %s runs as root — stop it yourself with: sudo brew services stop %s\n", name, name)
				continue
			}
			cmd = exec.Command("sudo", brew, "services", "stop", name)
		} else {
			cmd = exec.Command(brew, "services", "stop", name)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		_ = cmd.Run()
	}
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// PortActive reports whether any ESTABLISHED TCP connection exists to
// the local port — a browser tab keeps its HMR websocket open through
// Caddy, so this distinguishes "someone is looking at the site" from
// idle. The listener itself doesn't count.
func PortActive(port int) bool {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:ESTABLISHED").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "ESTABLISHED")
}
