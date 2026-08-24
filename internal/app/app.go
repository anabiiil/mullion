// Package app wires the pieces together: it loads state and knows how to
// converge the machine (Caddyfile, hosts file, php-cgi processes, junction)
// to match that state.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/hosts"
	"pm/internal/junction"
	"pm/internal/mysql"
	"pm/internal/nodever"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

type App struct {
	Paths pmdir.Paths
	State *config.State
}

func New() (*App, error) {
	paths, err := pmdir.New()
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureLayout(); err != nil {
		return nil, err
	}
	state, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	mysql.RootPassword = state.Config.MySQLPassword
	return &App{Paths: paths, State: state}, nil
}

// SiteVersion returns the full PHP version a site should run on.
func (a *App) SiteVersion(s config.Site) string {
	if s.PHP != "" {
		return s.PHP
	}
	return a.State.Config.GlobalPHP
}

// NeededVersions is the set of PHP versions that must have a running
// php-cgi process: the global one plus every isolated site's.
func (a *App) NeededVersions() []string {
	set := map[string]bool{}
	if v := a.State.Config.GlobalPHP; v != "" {
		set[v] = true
	}
	for _, s := range a.State.Sites {
		if s.IsPHP() && s.PHP != "" {
			set[s.PHP] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Hostnames of all linked sites.
func (a *App) Hostnames() []string {
	out := make([]string, 0, len(a.State.Sites))
	for _, s := range a.State.Sites {
		out = append(out, a.State.Host(s))
	}
	return out
}

// WriteCaddyfile regenerates ~/.mullion/Caddyfile from the current
// state. devPorts maps node site names to their running dev-server port
// (0 = down, which renders a friendly 503).
func (a *App) WriteCaddyfile(devPorts map[string]int) error {
	var confs []caddy.SiteConf
	for _, s := range a.State.Sites {
		conf := caddy.SiteConf{
			Host:   a.State.Host(s),
			Kind:   s.Kind,
			Secure: s.Secure,
		}
		switch s.Kind {
		case "node":
			if s.Mode == "build" {
				conf.Kind = "static"
				conf.Root = filepath.Join(s.Path, s.BuildDir)
				break
			}
			conf.ProxyPort = devPorts[s.Name]
			conf.RewriteHost = usesVite(s.Path)
			conf.Paused = s.DevPaused
		case "static":
			conf.Root = filepath.Join(s.Path, s.BuildDir)
		default: // php
			version := a.SiteVersion(s)
			if version == "" {
				return fmt.Errorf("no PHP version set: run `mullion use <version>` first")
			}
			port, err := phpver.FcgiPort(version)
			if err != nil {
				return err
			}
			conf.Root = caddy.DocRoot(s.Path)
			conf.FcgiPort = port
		}
		confs = append(confs, conf)
	}
	content := caddy.Generate(confs, a.Paths.LogsDir())
	return writeFile(a.Paths.Caddyfile(), content)
}

// NodeVersionDirFor resolves which installed Node a site should run on:
// its pinned version, the project's .nvmrc, or the global default.
func (a *App) NodeVersionDirFor(s config.Site) (string, error) {
	arg := s.Node
	if arg == "" {
		if data, err := os.ReadFile(filepath.Join(s.Path, ".nvmrc")); err == nil {
			arg = strings.TrimSpace(string(data))
		}
	}
	if arg == "" {
		arg = a.State.Config.GlobalNode
	}
	full, err := nodever.FindInstalled(a.Paths, arg)
	if err != nil {
		return "", err
	}
	return a.Paths.NodeVersionDir(full), nil
}

// EnsureDevServers brings up the dev server of every linked node site
// and returns their ports. A site that fails to start is reported but
// does not block the others — its host serves a clear 503 instead.
func (a *App) EnsureDevServers() map[string]int {
	ports := map[string]int{}
	for _, s := range a.State.Sites {
		if s.Kind != "node" || s.Mode == "build" || s.DevPaused {
			continue
		}
		nodeDir, err := a.NodeVersionDirFor(s)
		if err != nil {
			fmt.Printf("warning: %s: %v\n", s.Name, err)
			continue
		}
		port, err := devserver.Ensure(a.Paths, s, a.State.Host(s), nodeDir)
		if err != nil {
			fmt.Printf("warning: %s: %v\n", s.Name, err)
			continue
		}
		ports[s.Name] = port
	}
	return ports
}

// UseGlobalNode switches the node/current junction (and therefore the
// `node`/`npm` on PATH) to the given installed full version.
func (a *App) UseGlobalNode(fullVersion string) error {
	if err := junction.Set(a.Paths.CurrentNode(), a.Paths.NodeVersionDir(fullVersion)); err != nil {
		return err
	}
	a.State.Config.GlobalNode = fullVersion
	return nil
}

// ActivateNode makes a version the default: junction, saved state, the
// node/npm/npx shims, and the PATH block.
func (a *App) ActivateNode(fullVersion string) error {
	if err := a.UseGlobalNode(fullVersion); err != nil {
		return err
	}
	if err := a.State.Save(); err != nil {
		return err
	}
	if err := WriteNodeShims(a.Paths.BinDir()); err != nil {
		fmt.Println("note: could not write the node shims -", err)
	}
	return EnsureUserPath(a.Paths.BinDir(), a.Paths.CurrentPhp())
}

// Apply converges everything after a state change: config files, hosts
// entries, php-cgi processes, and Caddy — started if any site needs
// serving, merely reloaded otherwise.
func (a *App) Apply() error {
	if err := a.State.Save(); err != nil {
		return err
	}
	devPorts := a.EnsureDevServers()
	if err := a.WriteCaddyfile(devPorts); err != nil {
		return err
	}
	if err := hosts.Sync(a.Hostnames()); err != nil {
		return err
	}
	for _, v := range a.NeededVersions() {
		if err := fcgi.Ensure(a.Paths, v); err != nil {
			return err
		}
	}
	// Self-heal MySQL too: any mullion command brings it back if it died.
	if v := a.State.Config.MySQL; v != "" {
		if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
			return err
		}
		if err := mysql.Start(a.Paths, v); err != nil {
			return err
		}
	}
	if len(a.State.Sites) > 0 {
		if err := caddy.EnsureInstalled(context.Background(), a.Paths); err != nil {
			return err
		}
		return caddy.Start(a.Paths)
	}
	return caddy.Reload(a.Paths)
}

// SwitchDatabase installs (if needed) and activates a database version —
// MySQL or MariaDB — optionally migrating the current user databases
// into it via dump/restore (the on-disk format is not portable across
// versions or flavors). Returns the backup directory when a migration
// dump was taken ("" otherwise).
func SwitchDatabase(ctx context.Context, a *App, version string, migrate bool) (string, error) {
	if err := mysql.Install(ctx, a.Paths, version); err != nil {
		return "", err
	}

	prev := a.State.Config.MySQL
	switching := prev != "" && prev != version
	backupDir, dumpFile := "", ""

	if switching && migrate && mysql.DataInitialized(a.Paths) {
		if !mysql.Running() {
			fmt.Printf("Starting %s to export your databases...\n", mysql.Label(prev))
			if err := mysql.Start(a.Paths, prev); err != nil {
				return "", fmt.Errorf("the old server (%s) could not start to export databases: %w", mysql.Label(prev), err)
			}
		}
		dbs, err := mysql.UserDatabases(a.Paths, prev)
		if err != nil {
			return "", err
		}
		if len(dbs) > 0 {
			backupDir = filepath.Join(a.Paths.BackupsDir(),
				time.Now().Format("2006-01-02_150405")+"-migrate-"+prev)
			fmt.Printf("Exporting %d database(s) to %s\n", len(dbs), backupDir)
			if err := mysql.BackupTo(a.Paths, prev, dbs, backupDir); err != nil {
				return "", err
			}
			dumpFile = filepath.Join(backupDir, "all-databases.sql")
		}
	}

	if switching && mysql.Running() {
		fmt.Printf("Stopping %s...\n", mysql.Label(prev))
		if err := mysql.Stop(a.Paths, prev); err != nil {
			return backupDir, err
		}
	}
	if switching && mysql.DataInitialized(a.Paths) {
		backup := a.Paths.MysqlDataDir() + "-" + prev + "-backup"
		os.RemoveAll(backup)
		// mysqld can hold its files for a moment even after the port
		// closes — retry instead of failing the whole switch.
		var renameErr error
		for i := 0; i < 20; i++ {
			if renameErr = os.Rename(a.Paths.MysqlDataDir(), backup); renameErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if renameErr != nil {
			return backupDir, fmt.Errorf("moving old data directory aside: %w", renameErr)
		}
		fmt.Println("Old data directory kept at", backup)
	}

	a.State.Config.MySQL = version
	if err := a.State.Save(); err != nil {
		return backupDir, err
	}
	if err := mysql.EnsureInitialized(a.Paths, version); err != nil {
		return backupDir, err
	}
	if err := mysql.Start(a.Paths, version); err != nil {
		return backupDir, err
	}

	if dumpFile != "" {
		fmt.Printf("Restoring your databases into %s...\n", mysql.Label(version))
		if err := mysql.RestoreFile(a.Paths, version, dumpFile); err != nil {
			return backupDir, fmt.Errorf("%w\nThe dump is kept at %s — import it with: mullion mysql restore \"%s\"", err, backupDir, backupDir)
		}
		fmt.Println("Databases migrated. Backup kept at", backupDir)
	}
	return backupDir, nil
}

// RestartPhp bounces one version's php-cgi worker so php.ini changes
// (like toggled extensions) take effect immediately.
func (a *App) RestartPhp(version string) error {
	if err := fcgi.StopVersion(a.Paths, version); err != nil {
		return err
	}
	for _, v := range a.NeededVersions() {
		if v == version {
			return fcgi.Ensure(a.Paths, v)
		}
	}
	return nil
}

// UseGlobal switches the `current` junction (and therefore the `php` on
// PATH) to the given installed full version.
func (a *App) UseGlobal(fullVersion string) error {
	if err := junction.Set(a.Paths.CurrentPhp(), a.Paths.PhpVersionDir(fullVersion)); err != nil {
		return err
	}
	a.State.Config.GlobalPHP = fullVersion
	return nil
}

// ActiveVersion reads which version the junction points at ("" if none).
func (a *App) ActiveVersion() string {
	return a.State.Config.GlobalPHP
}

// DetectProjectKind classifies a project directory: PHP wins when there
// is any index.php/artisan/composer.json (mixed Laravel+Vite projects
// are served as PHP — Vite runs alongside); otherwise a package.json
// means a frontend project served by a managed dev server.
func DetectProjectKind(dir string) string {
	for _, marker := range []string{
		"index.php",
		filepath.Join("public", "index.php"),
		"artisan",
		"composer.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return "php"
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "node"
	}
	return "php"
}

// ResolveBuildDir picks the production build directory to serve for a
// --build (static) site.
func ResolveBuildDir(dir, override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(filepath.Join(dir, override, "index.html")); err != nil {
			return "", fmt.Errorf("%s has no index.html — build the project first, or pass the right --dir", filepath.Join(dir, override))
		}
		return override, nil
	}
	for _, c := range []string{"dist", "build", "out", filepath.Join(".output", "public")} {
		if _, err := os.Stat(filepath.Join(dir, c, "index.html")); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no build output found (looked for dist/, build/, out/, .output/public) — run your build first, or pass --dir <folder>")
}

// usesVite reports whether a project runs on Vite (directly or through
// a Vite-based framework — node_modules/vite covers both).
func usesVite(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "vite")); err == nil {
		return true
	}
	for _, c := range []string{"vite.config.js", "vite.config.ts", "vite.config.mjs", "vite.config.mts"} {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			return true
		}
	}
	return false
}

// EnsureBuildOutput returns the directory holding the project's
// production build, RUNNING the build first (with the right Node) when
// none exists yet — switching a domain to build mode should never ask
// the user to go build things by hand.
func (a *App) EnsureBuildOutput(path, override string) (string, error) {
	if dir, err := ResolveBuildDir(path, override); err == nil {
		return dir, nil
	}
	nodeDir, err := a.NodeVersionDirFor(config.Site{Path: path})
	if err != nil {
		return "", err
	}
	if err := devserver.RunBuild(a.Paths, path, nodeDir); err != nil {
		return "", err
	}
	return ResolveBuildDir(path, override)
}
