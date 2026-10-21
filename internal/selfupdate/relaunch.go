package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pm/internal/agent"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// Role is what a running Mullion process is doing.
type Role string

const (
	RoleNone      Role = ""
	RoleAgent     Role = "agent"      // mullion agent
	RoleTray      Role = "tray"       // mullion tray (Windows)
	RolePanel     Role = "panel"      // mullion ui — a browser tab or app window
	RolePanelHost Role = "panel-host" // mullion ui --window-host, Mullion.app's child
	RoleMacApp    Role = "app"        // Mullion.app itself
)

// Process is one running Mullion piece.
type Process struct {
	PID  int
	Exe  string
	Args []string
	Role Role
}

// macAppSuffix ends the executable path of Mullion.app (not "Mullion
// Terminal.app", whose executable is "Mullion Terminal").
const macAppSuffix = ".app/Contents/MacOS/Mullion"

// classify names a process's role from its executable and arguments.
func classify(exe string, args []string) Role {
	if strings.HasSuffix(filepath.ToSlash(exe), macAppSuffix) {
		return RoleMacApp
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(exe, `\`, "/")))
	if base != "mullion" && base != "mullion.exe" && base != "mullion.old.exe" &&
		!(strings.HasPrefix(base, "mullion.old-") && strings.HasSuffix(base, ".exe")) {
		return RoleNone
	}
	if len(args) == 0 {
		return RoleNone
	}
	switch args[0] {
	case "agent":
		return RoleAgent
	case "tray":
		return RoleTray
	case "ui":
		for _, a := range args[1:] {
			if a == "--window-host" {
				return RolePanelHost
			}
		}
		return RolePanel
	}
	return RoleNone
}

// parsePS joins `ps -axo pid=,comm=` (full executable path, may contain
// spaces) with `ps -axo pid=,args=` into classified processes, skipping
// self and anything that isn't a Mullion piece.
func parsePS(comm, args string, self int) []Process {
	exes := map[int]string{}
	for _, line := range strings.Split(comm, "\n") {
		pid, rest, ok := splitPID(line)
		if ok {
			exes[pid] = rest
		}
	}
	var out []Process
	for _, line := range strings.Split(args, "\n") {
		pid, rest, ok := splitPID(line)
		if !ok || pid == self {
			continue
		}
		exe := exes[pid]
		if exe == "" {
			continue
		}
		tail := strings.TrimPrefix(rest, exe)
		if tail == rest { // argv[0] differs from the path (relative launch)
			if f := strings.Fields(rest); len(f) > 0 {
				tail = strings.TrimPrefix(rest, f[0])
			}
		}
		argv := strings.Fields(tail)
		if r := classify(exe, argv); r != RoleNone {
			out = append(out, Process{PID: pid, Exe: exe, Args: argv, Role: r})
		}
	}
	return out
}

func splitPID(line string) (int, string, bool) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(line[:i])
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	return pid, strings.TrimSpace(line[i+1:]), true
}

// parseWinProcs reads "pid<TAB>exe<TAB>commandline" lines (Win32_Process).
func parseWinProcs(out string, self int) []Process {
	var procs []Process
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(parts) < 3 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		exe := strings.TrimSpace(parts[1])
		argv := winArgs(parts[2])
		if exe == "" && len(argv) > 0 {
			exe = argv[0]
		}
		if len(argv) > 0 {
			argv = argv[1:]
		}
		if r := classify(exe, argv); r != RoleNone {
			procs = append(procs, Process{PID: pid, Exe: exe, Args: argv, Role: r})
		}
	}
	return procs
}

// winArgs splits a Windows command line: the program may be quoted, and
// Mullion's own arguments never contain spaces.
func winArgs(cmdline string) []string {
	s := strings.TrimSpace(cmdline)
	var first string
	if strings.HasPrefix(s, `"`) {
		end := strings.Index(s[1:], `"`)
		if end < 0 {
			return []string{strings.Trim(s, `"`)}
		}
		first, s = s[1:1+end], s[end+2:]
	} else {
		f := strings.Fields(s)
		if len(f) == 0 {
			return nil
		}
		first, s = f[0], strings.TrimPrefix(s, f[0])
	}
	return append([]string{first}, strings.Fields(s)...)
}

// RelaunchOptions steers Relaunch.
type RelaunchOptions struct {
	Paths pmdir.Paths
	// Exe is the new binary (normally os.Executable() of the process
	// running Relaunch).
	Exe string
	// SelfUpdate runs the new binary's install hook (the CLI's
	// selfUpdateIfNeeded: installed copy, autostart, shortcut / Mullion.app,
	// version stamp). Called after old trays and panels are retired.
	SelfUpdate func()
	// PanelPID is the old panel process that asked for the upgrade; it
	// is retired even if process discovery misses it.
	PanelPID int
	// ReopenPanel opens a panel on the new version even when none was
	// found running (the upgrade came from one).
	ReopenPanel bool
	// Log receives progress lines.
	Log func(string)
}

// Relaunch moves every running Mullion piece onto the new binary. It
// must run IN the new binary:
//
//  1. old trays and panels (Windows) are stopped — the tray's single-
//     instance mutex and the scoop/exe files they hold must be free;
//  2. SelfUpdate refreshes the install (Windows: copies the exe into
//     ~/.mullion/bin and starts the tray; macOS: re-extracts Mullion.app);
//  3. the agent is replaced when one is running (agent.Ensure retires an
//     agent that reports an older version and spawns this binary);
//  4. Windows: the tray and the panel window are started again —
//     `mullion ui` closes the old app-mode browser window itself;
//     macOS: Mullion.app is quit and reopened (its `ui --window-host`
//     child exits with it, and the new app starts the new binary), or a
//     browser-tab panel is replaced by a fresh `mullion ui`.
func Relaunch(o RelaunchOptions) error {
	logf := func(format string, args ...any) {
		if o.Log != nil {
			o.Log(fmt.Sprintf(format, args...))
		}
	}
	if o.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		o.Exe = exe
	}
	running := listProcesses(os.Getpid())
	var trays, panels, hosts, apps []Process
	for _, p := range running {
		switch p.Role {
		case RoleTray:
			trays = append(trays, p)
		case RolePanel:
			panels = append(panels, p)
		case RolePanelHost:
			hosts = append(hosts, p)
		case RoleMacApp:
			apps = append(apps, p)
		}
	}
	if o.PanelPID > 0 && o.PanelPID != os.Getpid() && !hasPID(panels, o.PanelPID) && !hasPID(hosts, o.PanelPID) {
		panels = append(panels, Process{PID: o.PanelPID, Role: RolePanel})
	}

	windows := runtime.GOOS == "windows"
	if windows {
		for _, p := range append(trays, panels...) {
			logf("stopping the old %s (PID %d)", p.Role, p.PID)
			stopProcess(p.PID)
		}
	}

	if o.SelfUpdate != nil {
		o.SelfUpdate()
	}

	if agent.Running() {
		logf("restarting the Mullion agent on the new version")
		if err := agent.Ensure(o.Paths); err != nil {
			logf("agent: %v", err)
		}
	}

	if windows {
		CleanupOld(o.Paths.BinDir())
		launcher := o.Exe
		if bin := filepath.Join(o.Paths.BinDir(), "mullion.exe"); fileExists(bin) {
			launcher = bin // where the tray and shortcut normally run from
		}
		if len(trays) > 0 {
			logf("starting the tray")
			spawnDetached(launcher, true, "tray")
		}
		if len(panels) > 0 || o.ReopenPanel {
			logf("reopening the control panel")
			spawnDetached(launcher, true, "ui")
		}
		return nil
	}

	// macOS / Unix.
	reopenedApp := false
	for _, a := range apps {
		bundle := strings.TrimSuffix(filepath.ToSlash(a.Exe), "/Contents/MacOS/Mullion")
		logf("restarting %s", bundle)
		stopProcess(a.PID)
		if !reopenedApp {
			reopenedApp = true
			if err := proc.Quiet("open", bundle).Start(); err != nil {
				logf("could not reopen %s: %v", bundle, err)
			}
		}
	}
	for _, h := range hosts {
		// Normally gone with its app; a leftover old panel must not linger.
		if alive(h.PID) {
			stopProcess(h.PID)
		}
	}
	for _, p := range panels {
		logf("stopping the old control panel (PID %d)", p.PID)
		stopProcess(p.PID)
	}
	if !reopenedApp && (len(panels) > 0 || o.ReopenPanel) {
		logf("reopening the control panel")
		spawnDetached(o.Exe, false, "ui", "--detached")
	}
	return nil
}

func hasPID(ps []Process, pid int) bool {
	for _, p := range ps {
		if p.PID == pid {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// stopProcess asks a process to exit, then forces it after a few seconds.
func stopProcess(pid int) {
	terminate(pid)
	for i := 0; i < 50 && alive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if alive(pid) {
		kill(pid)
		for i := 0; i < 20 && alive(pid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// spawnDetached starts exe with args as its own background process
// (stdin from the null device), not waiting for it.
func spawnDetached(exe string, hiddenConsole bool, args ...string) {
	cmd := proc.Quiet(exe, args...)
	if hiddenConsole {
		proc.DetachHiddenConsole(cmd)
	} else {
		proc.Detach(cmd)
	}
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}
