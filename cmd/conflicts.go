package cmd

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"pm/internal/app"
	"pm/internal/caddy"
	"pm/internal/console"
)

type portConflict struct {
	Port int
	PID  int
	Name string
}

// resolvePortConflicts detects other stacks (Laragon, XAMPP, IIS, ...)
// squatting on Mullion's WEB ports and — with the user's OK — stops them
// so Mullion becomes the main server. Port 3306 is deliberately not
// handled here: the MySQL setup step deals with it, where the other
// server's databases can be imported before it goes down.
func resolvePortConflicts(a *app.App) {
	conflicts := findPortConflicts(a)
	if len(conflicts) == 0 {
		return
	}

	fmt.Println("\nAnother web server is using Mullion's ports:")
	for _, c := range conflicts {
		fmt.Printf("  port %-5d %s (PID %d)\n", c.Port, c.Name, c.PID)
	}

	if !console.Interactive() {
		fmt.Println("warning: Mullion's services cannot bind those ports until you stop the processes above (e.g. quit Laragon).")
		return
	}
	if !askYesNo("Stop those processes now so Mullion becomes the main server?", true) {
		fmt.Println("Leaving them running — Mullion's own services will not be reachable on those ports.")
		return
	}

	for _, c := range conflicts {
		killWithParent(c.PID)
	}
	// Give the OS a moment to release the ports, then report honestly.
	for i := 0; i < 20; i++ {
		if len(findPortConflicts(a)) == 0 {
			fmt.Println("Ports are free — Mullion is the main server now.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("warning: some ports are still busy (the other stack may auto-restart its services — quit it fully).")
}

// findPortConflicts checks the ports Mullion needs, skipping the ones
// served by Mullion itself.
func findPortConflicts(a *app.App) []portConflict {
	var out []portConflict
	webIsOurs := caddy.Running()
	for _, port := range []int{80, 443} {
		if webIsOurs || !portBusy(port) {
			continue
		}
		if pid, name := portOwner(port); pid > 0 {
			out = append(out, portConflict{port, pid, name})
		}
	}
	return out
}

func portBusy(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// portOwner resolves which process listens on the port.
func portOwner(port int) (int, string) {
	script := fmt.Sprintf(
		`$p = (Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess; if ($p) { "$p " + (Get-Process -Id $p -ErrorAction SilentlyContinue).Name }`,
		port)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
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
	_ = exec.Command("powershell", "-NoProfile", "-Command", script).Run()
}
