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
	"time"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/fcgi"
	"pm/internal/hosts"
	"pm/internal/junction"
	"pm/internal/mysql"
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
		if s.PHP != "" {
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

// WriteCaddyfile regenerates ~/.mullion/Caddyfile from the current state.
func (a *App) WriteCaddyfile() error {
	var confs []caddy.SiteConf
	for _, s := range a.State.Sites {
		version := a.SiteVersion(s)
		if version == "" {
			return fmt.Errorf("no PHP version set: run `mullion use <version>` first")
		}
		port, err := phpver.FcgiPort(version)
		if err != nil {
			return err
		}
		confs = append(confs, caddy.SiteConf{
			Host:     a.State.Host(s),
			Root:     caddy.DocRoot(s.Path),
			FcgiPort: port,
			Secure:   s.Secure,
		})
	}
	content := caddy.Generate(confs, a.Paths.LogsDir())
	return writeFile(a.Paths.Caddyfile(), content)
}

// Apply converges everything after a state change: config files, hosts
// entries, php-cgi processes, and Caddy — started if any site needs
// serving, merely reloaded otherwise.
func (a *App) Apply() error {
	if err := a.State.Save(); err != nil {
		return err
	}
	if err := a.WriteCaddyfile(); err != nil {
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
