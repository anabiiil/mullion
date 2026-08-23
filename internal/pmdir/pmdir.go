// Package pmdir resolves every filesystem path the tool owns under its
// install root: C:\Mullion — visible and easy to find, like other dev
// stacks — with backups next to it in C:\Mullion-Backups so they
// survive an uninstall.
package pmdir

import (
	"os"
	"path/filepath"
	"runtime"
)

type Paths struct {
	Home string
}

func New() (Paths, error) {
	if runtime.GOOS != "windows" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		return Paths{Home: filepath.Join(home, ".mullion")}, nil
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	return Paths{Home: drive + `\Mullion`}, nil
}

// BackupsDir lives OUTSIDE Home on purpose: `mullion uninstall` wipes
// Home, and a backup that dies with the thing it backs up is no backup.
func (p Paths) BackupsDir() string { return p.Home + "-Backups" }

func (p Paths) BinDir() string                  { return filepath.Join(p.Home, "bin") }
func (p Paths) CaddyExe() string                { return filepath.Join(p.BinDir(), exeName("caddy")) }
func (p Paths) PhpDir() string                  { return filepath.Join(p.Home, "php") }
func (p Paths) PhpVersionDir(v string) string   { return filepath.Join(p.PhpDir(), v) }
func (p Paths) CurrentPhp() string              { return filepath.Join(p.PhpDir(), "current") }
func (p Paths) MysqlDir() string                { return filepath.Join(p.Home, "mysql") }
func (p Paths) MysqlVersionDir(v string) string { return filepath.Join(p.MysqlDir(), v) }
func (p Paths) MysqlDataDir() string            { return filepath.Join(p.MysqlDir(), "data") }
func (p Paths) MysqlIni() string                { return filepath.Join(p.MysqlDir(), "my.ini") }
func (p Paths) PhpMyAdminDir() string           { return filepath.Join(p.Home, "phpmyadmin") }
func (p Paths) VersionFile() string             { return filepath.Join(p.Home, "version") }
func (p Paths) ConfigFile() string              { return filepath.Join(p.Home, "config.json") }
func (p Paths) SitesFile() string               { return filepath.Join(p.Home, "sites.json") }
func (p Paths) Caddyfile() string               { return filepath.Join(p.Home, "Caddyfile") }
func (p Paths) LogsDir() string                 { return filepath.Join(p.Home, "logs") }
func (p Paths) PidsDir() string                 { return filepath.Join(p.Home, "pids") }
func (p Paths) TmpDir() string                  { return filepath.Join(p.Home, "tmp") }

// EnsureLayout creates every directory the tool needs.
func (p Paths) EnsureLayout() error {
	for _, dir := range []string{p.Home, p.BinDir(), p.PhpDir(), p.LogsDir(), p.PidsDir(), p.TmpDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ExeName appends the platform's executable suffix (".exe" on Windows).
func ExeName(base string) string { return exeName(base) }
