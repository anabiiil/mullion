// Package devserver keeps one managed `npm run dev` per linked frontend
// site: started on demand, detached, logged, its actual listening port
// detected and handed to Caddy's reverse proxy — so opening
// https://myapp.test just works without the user thinking about it.
package devserver

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

func pidFile(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.PidsDir(), "dev-"+site+".pid")
}
func portFile(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.PidsDir(), "dev-"+site+".port")
}
func logFilePath(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.LogsDir(), "dev-"+site+".log")
}

// Running reports the dev server's live port (0 when not running).
func Running(paths pmdir.Paths, site string) int {
	pid := readInt(pidFile(paths, site))
	port := readInt(portFile(paths, site))
	if pid > 0 && port > 0 && processAlive(pid) && portListening(port) {
		return port
	}
	return 0
}

// Ensure starts the site's dev server if needed and returns the port
// Caddy should proxy to.
func Ensure(paths pmdir.Paths, site config.Site, host, nodeDir string) (int, error) {
	if port := Running(paths, site.Name); port > 0 {
		return port, nil
	}
	Stop(paths, site.Name) // clear any half-dead leftover

	pm, script, err := detectRunner(site.Path)
	if err != nil {
		return 0, err
	}
	// pnpm/yarn ship as corepack shims that only exist after
	// `corepack enable` — run it once per Node version when needed.
	if pm != "npm" {
		if _, statErr := os.Stat(nodever.Tool(nodeDir, pm)); statErr != nil {
			enable := proc.Quiet(nodever.Tool(nodeDir, "corepack"), "enable")
			enable.Env = append(os.Environ(),
				"PATH="+nodever.BinDir(nodeDir)+string(os.PathListSeparator)+os.Getenv("PATH"))
			if out, err := enable.CombinedOutput(); err != nil {
				return 0, fmt.Errorf("this project uses %s, and `corepack enable` failed: %v: %s", pm, err, out)
			}
		}
	}

	logFile, err := os.OpenFile(logFilePath(paths, site.Name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n---- %s: mullion starting `%s run %s` (node %s) ----\n",
		time.Now().Format(time.RFC3339), pm, script, filepath.Base(nodeDir))

	env := append(os.Environ(),
		"PATH="+nodever.BinDir(nodeDir)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PORT="+strconv.Itoa(site.DevPort),
		"BROWSER=none",
		// Dev servers block unknown Host headers (DNS-rebinding
		// protection); let our .test host through.
		"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS="+host,
		"DANGEROUSLY_DISABLE_HOST_CHECK=true",
	)

	// A fresh clone has no node_modules — installing for the user is
	// what "the link just opens" means.
	if _, statErr := os.Stat(filepath.Join(site.Path, "node_modules")); statErr != nil {
		fmt.Printf("Installing %s dependencies (%s install)...\n", site.Name, pm)
		install := proc.Quiet(nodever.Tool(nodeDir, pm), "install")
		install.Dir = site.Path
		install.Env = env
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr
		if err := install.Run(); err != nil {
			return 0, fmt.Errorf("%s install failed in %s: %w", pm, site.Path, err)
		}
	}

	cmd := proc.Quiet(nodever.Tool(nodeDir, pm), "run", script)
	cmd.Dir = site.Path
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting `%s run %s` for %s: %w", pm, script, site.Name, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if err := os.WriteFile(pidFile(paths, site.Name), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return 0, err
	}

	fmt.Printf("Starting %s's dev server (`%s run %s`)...\n", site.Name, pm, script)
	// Wait for it to open a port — first boots (bundling) can be slow.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return 0, fmt.Errorf("the dev server for %s exited — last log lines:\n%s(full log: %s)",
				site.Name, logTail(logFilePath(paths, site.Name)), logFilePath(paths, site.Name))
		}
		if port := pickPort(listeningPorts(pid), site.DevPort); port > 0 {
			_ = os.WriteFile(portFile(paths, site.Name), []byte(strconv.Itoa(port)), 0o644)
			fmt.Printf("%s's dev server is up on localhost:%d\n", site.Name, port)
			return port, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return 0, fmt.Errorf("the dev server for %s did not open a port within 2 minutes (see %s)",
		site.Name, logFilePath(paths, site.Name))
}

// pickPort chooses the port to proxy to among a process's listeners:
// the assigned one when honored, otherwise the lowest real one
// (skipping the debug inspector).
func pickPort(ports []int, assigned int) int {
	best := 0
	for _, p := range ports {
		if p == assigned {
			return p
		}
		if p == 9229 || p == 9230 { // node inspector
			continue
		}
		if best == 0 || p < best {
			best = p
		}
	}
	return best
}

// Stop terminates one site's dev server (no-op when not running).
func Stop(paths pmdir.Paths, site string) {
	if pid := readInt(pidFile(paths, site)); pid > 0 {
		killTree(pid)
	}
	_ = os.Remove(pidFile(paths, site))
	_ = os.Remove(portFile(paths, site))
}

// StopAll terminates every managed dev server.
func StopAll(paths pmdir.Paths) {
	entries, err := os.ReadDir(paths.PidsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "dev-") && strings.HasSuffix(name, ".pid") {
			Stop(paths, strings.TrimSuffix(strings.TrimPrefix(name, "dev-"), ".pid"))
		}
	}
}

// detectRunner picks the package manager (from the lockfile) and the
// script to run (dev, serve, or start).
func detectRunner(dir string) (pm, script string, err error) {
	pm = "npm"
	if _, e := os.Stat(filepath.Join(dir, "pnpm-lock.yaml")); e == nil {
		pm = "pnpm"
	} else if _, e := os.Stat(filepath.Join(dir, "yarn.lock")); e == nil {
		pm = "yarn"
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", "", fmt.Errorf("%s has no package.json", dir)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", "", fmt.Errorf("parsing package.json in %s: %w", dir, err)
	}
	for _, s := range []string{"dev", "serve", "start"} {
		if _, ok := pkg.Scripts[s]; ok {
			return pm, s, nil
		}
	}
	return "", "", fmt.Errorf("package.json in %s has no dev, serve, or start script", dir)
}

// AssignPort returns a stable free dev port not used by other sites.
func AssignPort(sites []config.Site) int {
	used := map[int]bool{}
	for _, s := range sites {
		if s.DevPort > 0 {
			used[s.DevPort] = true
		}
	}
	for p := 42001; ; p++ {
		if !used[p] {
			return p
		}
	}
}

func readInt(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

// portListening probes both loopback families — dev servers often bind
// only ::1 (Node resolves "localhost" to IPv6 first on macOS).
func portListening(port int) bool {
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}
