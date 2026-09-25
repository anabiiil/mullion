// This file gives the control panel (internal/ui) the same actions the
// CLI has — the core of each command lives here as a UI-agnostic method
// on App, returning values/errors instead of printing results. Progress
// output via fmt is fine (it mirrors what the CLI already prints), but
// callers should not have to scrape stdout for the outcome.
package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"pm/internal/agent"
	"pm/internal/caddy"
	"pm/internal/composer"
	"pm/internal/devserver"
	"pm/internal/dnsd"
	"pm/internal/fcgi"
	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/nodever"
	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/postgres"
	"pm/internal/sysproc"
	"pm/internal/version"
)

// UninstallPhp removes an installed PHP version, refusing when it is
// the active global version or a site is isolated to it (mirrors
// `mullion php uninstall`'s checks). Its php-fpm/php-cgi worker is
// stopped first so the files can always be removed cleanly.
func (a *App) UninstallPhp(version string) error {
	sel, err := phpver.ParseSelector(version)
	if err != nil {
		return err
	}
	full, err := phpver.FindInstalled(a.Paths, sel)
	if err != nil {
		return err
	}
	if full == a.State.Config.GlobalPHP {
		return fmt.Errorf("PHP %s is the active global version; switch first with `mullion use <other>`", full)
	}
	for _, s := range a.State.Sites {
		if s.PHP == full {
			return fmt.Errorf("site %q is isolated to PHP %s; run `mullion unisolate` there first", s.Name, full)
		}
	}
	if err := fcgi.StopVersion(a.Paths, full); err != nil {
		return err
	}
	if err := os.RemoveAll(a.Paths.PhpVersionDir(full)); err != nil {
		return err
	}
	fmt.Println("Removed PHP", full)
	return nil
}

// UninstallNode removes an installed Node version, refusing when it is
// the global default or a site is pinned to it (mirrors
// `mullion node uninstall`'s checks).
func (a *App) UninstallNode(version string) error {
	full, err := nodever.FindInstalled(a.Paths, version)
	if err != nil {
		return err
	}
	if full == a.State.Config.GlobalNode {
		return fmt.Errorf("Node %s is the default; switch first with `mullion node use <other>`", full)
	}
	for _, s := range a.State.Sites {
		if s.Node == full {
			return fmt.Errorf("site %q is pinned to Node %s; run `mullion node isolate <other>` there first", s.Name, full)
		}
	}
	if err := os.RemoveAll(a.Paths.NodeVersionDir(full)); err != nil {
		return err
	}
	fmt.Println("Removed Node", full)
	return nil
}

// SetNpmVersion installs a specific npm release into an installed Node
// version (the global default when nodeVersion is ""), mirroring
// `mullion node npm`.
func (a *App) SetNpmVersion(ctx context.Context, npmVersion, nodeVersion string) error {
	target := nodeVersion
	if target == "" {
		if a.State.Config.GlobalNode == "" {
			return fmt.Errorf("no Node installed yet (run: mullion node install lts)")
		}
		target = a.State.Config.GlobalNode
	}
	full, err := nodever.FindInstalled(a.Paths, target)
	if err != nil {
		return err
	}
	dir := a.Paths.NodeVersionDir(full)
	spec := "npm@" + strings.TrimPrefix(npmVersion, "npm@")
	fmt.Printf("Installing %s into Node %s...\n", spec, full)
	c := exec.CommandContext(ctx, nodever.Tool(dir, "npm"), "install", "-g", spec)
	c.Env = append(os.Environ(), "PATH="+nodever.BinDir(dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return err
	}
	fmt.Printf("Done — Node %s now ships that npm.\n", full)
	return nil
}

// RestartDevServer restarts one site's managed dev server (unpausing it
// if needed), mirroring `mullion dev restart`.
func (a *App) RestartDevServer(name string) error {
	site := a.State.FindSite(name)
	if site == nil {
		return fmt.Errorf("no site named %q", name)
	}
	if site.Kind != "node" {
		return fmt.Errorf("%s is not a frontend site", site.Name)
	}
	site.DevPaused = false
	devserver.Stop(a.Paths, site.Name)
	if err := a.Apply(); err != nil {
		return err
	}
	fmt.Printf("%s's dev server restarted.\n", a.State.Host(*site))
	return nil
}

// RestartStack stops and brings the whole managed stack back up: Caddy,
// the PHP FastCGI workers, dev servers, the wake agent, and the
// configured databases — the same operations `mullion stop` followed by
// `mullion start` performs. Like that CLI pair, the stop phase does NOT
// persist "stopped by the user" for the databases, so they self-heal
// back up in the start phase below.
//
// Every one of these stops targets a specific pid/port it owns (not a
// kill-by-process-name sweep), so a control panel server calling this
// from inside its own long-running `mullion ui` process is never at
// risk of killing itself.
func (a *App) RestartStack(ctx context.Context) error {
	if err := caddy.Stop(a.Paths); err != nil {
		return err
	}
	if err := fcgi.StopAll(a.Paths); err != nil {
		return err
	}
	devserver.StopAll(a.Paths)
	agent.Stop(a.Paths)
	if v := a.State.Config.MySQL; v != "" {
		if err := mysql.Stop(a.Paths, v); err != nil {
			return err
		}
	}
	if v := a.State.Config.Postgres; v != "" {
		if err := postgres.Stop(a.Paths, v); err != nil {
			return err
		}
	}
	if v := a.State.Config.Mongo; v != "" {
		if err := mongodb.Stop(a.Paths, v); err != nil {
			return err
		}
	}
	fmt.Println("Stopped.")

	if err := caddy.EnsureInstalled(ctx, a.Paths); err != nil {
		return err
	}
	if err := a.Apply(); err != nil {
		return err
	}
	return caddy.Start(a.Paths)
}

// composerVersionRe extracts the version number from `composer
// --version`'s output ("Composer version 2.7.6 2024-06-10 22:11:12").
var composerVersionRe = regexp.MustCompile(`Composer version (\S+)`)

// parseComposerVersion pulls the version number out of `composer
// --version`'s output ("" when the text doesn't match).
func parseComposerVersion(output string) string {
	m := composerVersionRe.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// composerShimPath is where `mullion composer install` writes the
// `composer` entry point (composer.Install's writeShim).
func composerShimPath(paths pmdir.Paths) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(paths.BinDir(), "composer.bat")
	}
	return filepath.Join(paths.BinDir(), "composer")
}

// ComposerVersion reports the installed Composer's version ("" when
// Composer isn't installed, or its version can't be determined — e.g.
// no PHP installed yet to run it with).
func (a *App) ComposerVersion() string {
	if _, err := os.Stat(composer.PharPath(a.Paths)); err != nil {
		return ""
	}
	out, err := exec.Command(composerShimPath(a.Paths), "--version").Output()
	if err != nil {
		return ""
	}
	return parseComposerVersion(string(out))
}

// InstallComposer installs Composer ("" version = latest stable),
// mirroring `mullion composer install`, and returns the version that
// ended up installed (best-effort: falls back to the requested version,
// or "latest stable", when it can't be detected — e.g. no PHP yet).
func (a *App) InstallComposer(ctx context.Context, version string) (string, error) {
	if err := composer.Install(ctx, a.Paths, version); err != nil {
		return "", err
	}
	if v := a.ComposerVersion(); v != "" {
		return v, nil
	}
	if version != "" {
		return version, nil
	}
	return "latest stable", nil
}

// RestoreMySQL imports a backup (a single .sql file, or a backup folder)
// into the running MySQL server, mirroring `mullion mysql restore`.
func (a *App) RestoreMySQL(ctx context.Context, path string) error {
	v := a.State.Config.MySQL
	if v == "" {
		return fmt.Errorf("MySQL is not installed (run: mullion mysql install)")
	}
	if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
		return err
	}
	if err := mysql.Start(a.Paths, v); err != nil {
		return err
	}

	path = strings.Trim(strings.TrimSpace(path), `"`)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s does not exist", path)
	}

	var files []string
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(path, "all-databases.sql")); err == nil {
			files = []string{filepath.Join(path, "all-databases.sql")}
		} else {
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
					files = append(files, filepath.Join(path, e.Name()))
				}
			}
			if len(files) == 0 {
				return fmt.Errorf("no .sql files in %s", path)
			}
		}
	} else {
		files = []string{path}
	}

	for _, f := range files {
		fmt.Println("Importing", filepath.Base(f), "...")
		if err := mysql.RestoreFile(a.Paths, v, f); err != nil {
			return err
		}
	}
	fmt.Printf("Done — %d file(s) imported into MySQL %s.\n", len(files), v)
	return nil
}

// DoctorCheck is one line of `mullion doctor`'s diagnosis. Status is
// "ok", "warn", or "fail" — or "" for a plain informational line (no
// pass/fail verdict, e.g. the version banner). Detail is what follows
// the status marker on the same line; Fix is an additional block
// printed after it (e.g. a log tail), already formatted with its own
// leading indentation/newlines.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

const (
	DoctorOK   = "ok"
	DoctorWarn = "warn"
	DoctorFail = "fail"
)

func doctorStatus(ok bool) string {
	if ok {
		return DoctorOK
	}
	return DoctorFail
}

// caddyfileMissingHosts returns which of the given hosts are absent
// from the Caddyfile's contents.
func caddyfileMissingHosts(content string, hosts []string) []string {
	var missing []string
	for _, h := range hosts {
		if !strings.Contains(content, h) {
			missing = append(missing, h)
		}
	}
	return missing
}

// Doctor runs the same diagnosis as `mullion doctor` and returns it as
// structured checks; the CLI renders them in the same shape it always
// has (see cmd/doctor.go).
func (a *App) Doctor(ctx context.Context) []DoctorCheck {
	var checks []DoctorCheck

	checks = append(checks, DoctorCheck{Name: "mullion " + version.Number})
	if exe, err := os.Executable(); err == nil {
		checks = append(checks, DoctorCheck{Name: "running from: " + exe})
	}
	if found, err := exec.LookPath("mullion"); err == nil {
		checks = append(checks, DoctorCheck{Name: "PATH resolves to: " + found})
	}
	checks = append(checks, DoctorCheck{Name: "home: " + a.Paths.Home})

	// Caddy identity — the #1 cause of "my changes don't show up".
	running := caddy.Running()
	ours := running && caddy.ServingOurs(a.Paths)
	switch {
	case !running:
		checks = append(checks, DoctorCheck{Name: "caddy", Status: DoctorFail, Detail: "not running"})
	case !ours:
		pid, name := sysproc.PortOwner(2019)
		checks = append(checks, DoctorCheck{Name: "caddy", Status: DoctorFail,
			Detail: fmt.Sprintf("a FOREIGN caddy answers the admin port (%s, PID %d) — `mullion start` will replace it", name, pid)})
	default:
		checks = append(checks, DoctorCheck{Name: "caddy", Status: DoctorOK, Detail: "running with Mullion's config"})
	}

	for _, port := range []int{80, 443} {
		name := fmt.Sprintf("port %d", port)
		if pid, procName := sysproc.PortOwner(port); pid > 0 {
			owned := strings.Contains(strings.ToLower(procName), "caddy") && ours
			checks = append(checks, DoctorCheck{Name: name, Status: doctorStatus(owned),
				Detail: fmt.Sprintf("held by %s (PID %d)", procName, pid)})
		} else {
			checks = append(checks, DoctorCheck{Name: name, Status: DoctorFail, Detail: "nothing listening"})
		}
	}

	if v := a.State.Config.GlobalPHP; v != "" {
		runningFcgi := len(fcgi.RunningVersions([]string{v})) == 1
		checks = append(checks, DoctorCheck{Name: "php " + v, Status: doctorStatus(runningFcgi)})
	}
	if v := a.State.Config.MySQL; v != "" {
		foreign := mysql.Running() && len(sysproc.ProcessesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) == 0
		switch {
		case foreign:
			checks = append(checks, DoctorCheck{Name: "mysql " + v, Status: DoctorFail,
				Detail: fmt.Sprintf("a foreign server holds port %d — `mullion mysql start` reclaims it", mysql.Port)})
		case mysql.Running():
			checks = append(checks, DoctorCheck{Name: "mysql " + v, Status: DoctorOK})
		default:
			checks = append(checks, DoctorCheck{Name: "mysql " + v, Status: DoctorFail, Detail: "not running"})
		}
	}
	if v := a.State.Config.GlobalNode; v != "" {
		checks = append(checks, DoctorCheck{Name: "node default: " + v})
	}

	hasNode := false
	for _, s := range a.State.Sites {
		if s.Kind == "node" {
			hasNode = true
			break
		}
	}
	if hasNode {
		check := DoctorCheck{Name: "wake agent", Status: doctorStatus(agent.Running())}
		if data, err := os.ReadFile(filepath.Join(a.Paths.LogsDir(), "agent.log")); err == nil && len(data) > 0 {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			var b strings.Builder
			b.WriteString("  agent log tail:\n")
			for _, l := range lines {
				b.WriteString("    " + l + "\n")
			}
			check.Fix = b.String()
		}
		checks = append(checks, check)
	}

	if a.State.Config.WildcardDNS {
		_, resolverOK, serverOK, note := a.WildcardDNSStatus()
		check := DoctorCheck{Name: "wildcard dns", Status: doctorStatus(resolverOK && serverOK),
			Detail: fmt.Sprintf("*.%s via %s", a.State.Config.TLD, dnsd.ResolverDescription(a.State.Config.TLD))}
		if note != "" {
			check.Detail = note
		}
		checks = append(checks, check)
	}

	for _, site := range a.State.Sites {
		host := a.State.Host(site)
		switch {
		case site.Kind == "node" && site.Mode == "build":
			_, err := os.Stat(filepath.Join(site.Path, site.BuildDir, "index.html"))
			checks = append(checks, DoctorCheck{
				Name:   fmt.Sprintf("site %-24s build mode (%s/)", host, site.BuildDir),
				Status: doctorStatus(err == nil),
			})
		case site.Kind == "node":
			port := devserver.Running(a.Paths, site.Name)
			state := fmt.Sprintf("dev on port %d", port)
			if port == 0 {
				if site.DevPaused {
					state = "dev PAUSED by you"
				} else {
					state = "dev NOT RUNNING"
				}
			}
			nodeV := "?"
			if dir, err := a.NodeVersionDirFor(site); err == nil {
				nodeV = filepath.Base(dir)
			}
			check := DoctorCheck{
				Name:   fmt.Sprintf("site %-24s %s  (node %s)", host, state, nodeV),
				Status: doctorStatus(port > 0 || site.DevPaused),
			}
			if port == 0 && !site.DevPaused {
				check.Fix = devserver.LogTail(a.Paths, site.Name)
			}
			checks = append(checks, check)
		case site.Kind == "static":
			_, err := os.Stat(filepath.Join(site.Path, site.BuildDir, "index.html"))
			checks = append(checks, DoctorCheck{
				Name:   fmt.Sprintf("site %-24s static (%s/)", host, site.BuildDir),
				Status: doctorStatus(err == nil),
			})
		default:
			checks = append(checks, DoctorCheck{Name: fmt.Sprintf("site %-24s php %s", host, a.SiteVersion(site))})
		}
	}

	// Does the Caddyfile on disk match what this state would generate?
	if data, err := os.ReadFile(a.Paths.Caddyfile()); err == nil {
		for _, host := range caddyfileMissingHosts(string(data), a.Hostnames()) {
			checks = append(checks, DoctorCheck{Name: "caddyfile", Status: DoctorFail,
				Detail: fmt.Sprintf("%s is missing — run `mullion start`", host)})
		}
	}

	return checks
}

// ResolveNodeForCwd resolves the Node version directory the CURRENT
// DIRECTORY should use: a linked site's pin, a .nvmrc walking up
// (stopping at the home directory), then the global default. The
// reason explains the choice to a human — used by `mullion node which`
// / `mullion node bin` and NodeInfo.
func (a *App) ResolveNodeForCwd() (dir, reason string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	home, _ := os.UserHomeDir()
	for d := cwd; ; d = filepath.Dir(d) {
		if site := a.State.FindSiteByPath(d); site != nil {
			verDir, err := a.NodeVersionDirFor(*site)
			why := fmt.Sprintf("linked site %q", site.Name)
			if site.Node == "" {
				why += " (.nvmrc / global)"
			} else {
				why += " (pinned)"
			}
			return verDir, why, err
		}
		if data, err := os.ReadFile(filepath.Join(d, ".nvmrc")); err == nil {
			full, err := nodever.FindInstalled(a.Paths, strings.TrimSpace(string(data)))
			if err != nil {
				return "", "", err
			}
			return a.Paths.NodeVersionDir(full), filepath.Join(d, ".nvmrc"), nil
		}
		if d == home || filepath.Dir(d) == d {
			break
		}
	}
	full, err := nodever.FindInstalled(a.Paths, a.State.Config.GlobalNode)
	if err != nil {
		return "", "", err
	}
	return a.Paths.NodeVersionDir(full), "the global default", nil
}

// NodeInfo describes which Node version the current directory resolves
// to and why, plus the tool paths and PATH-shadowing state that
// `mullion node which` reports — for a diagnostics display. Error is
// set (and the rest left zero) when resolution fails, e.g. no Node
// installed yet.
type NodeInfo struct {
	Version       string `json:"version,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Dir           string `json:"dir,omitempty"`
	GlobalDefault string `json:"globalDefault,omitempty"`
	NodePath      string `json:"nodePath,omitempty"`
	NpmPath       string `json:"npmPath,omitempty"`
	NpxPath       string `json:"npxPath,omitempty"`
	ShimPath      string `json:"shimPath,omitempty"`
	ResolvedPath  string `json:"resolvedPath,omitempty"`
	Shadowed      bool   `json:"shadowed,omitempty"`
	Warning       string `json:"warning,omitempty"`
	Error         string `json:"error,omitempty"`
}

// NodeInfo reports what `mullion node which` shows, structured for the
// panel.
func (a *App) NodeInfo() NodeInfo {
	info := NodeInfo{GlobalDefault: a.State.Config.GlobalNode}
	dir, reason, err := a.ResolveNodeForCwd()
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Dir = dir
	info.Version = filepath.Base(dir)
	info.Reason = reason
	info.NodePath = nodever.Tool(dir, "node")
	info.NpmPath = nodever.Tool(dir, "npm")
	info.NpxPath = nodever.Tool(dir, "npx")
	info.ShimPath = filepath.Join(a.Paths.BinDir(), "node")

	found, lookErr := exec.LookPath("node")
	if lookErr != nil {
		info.Warning = "no `node` on this terminal's PATH — open a NEW terminal."
		return info
	}
	info.ResolvedPath = found
	info.Shadowed = filepath.Clean(found) != filepath.Clean(info.ShimPath)
	return info
}
