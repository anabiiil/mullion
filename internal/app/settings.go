// This file backs the control panel's Settings (backup location), Node
// (npm versions), and Sites (pin) pages — see actions.go's doc comment
// for the pattern: UI-agnostic methods on App that the CLI and the
// panel both call.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/detect"
	"pm/internal/nodever"
)

// SetBackupDir changes where database backups are written. "" resets to
// the default (next to the install root, so it survives an uninstall).
// A leading "~/" is expanded to the user's home directory. The
// directory is created if missing and a write is verified (create+
// remove a temp file); a path inside the install root or the
// filesystem root is refused. It does NOT move any backups already
// written under the old location — callers that want that must copy
// them themselves.
func (a *App) SetBackupDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		a.State.Config.BackupDir = ""
		if err := a.State.Save(); err != nil {
			return err
		}
		a.Paths.Backups = ""
		return nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[2:])
		}
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("backup directory must be an absolute path: %q", dir)
	}
	dir = filepath.Clean(dir)

	if isRootPath(dir) {
		return fmt.Errorf("refusing to use the filesystem root as the backup directory")
	}
	if withinDir(dir, a.Paths.Home) {
		return fmt.Errorf("backup directory can't be inside %s — `mullion uninstall` wipes that directory, and it would take the backups with it", a.Paths.Home)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".mullion-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	os.Remove(probe)

	a.State.Config.BackupDir = dir
	if err := a.State.Save(); err != nil {
		return err
	}
	a.Paths.Backups = dir
	return nil
}

// isRootPath reports whether dir is a filesystem root ("/" on unix,
// "C:\" or similar on Windows).
func isRootPath(dir string) bool {
	return filepath.Dir(dir) == dir
}

// withinDir reports whether child is base itself or nested under it.
func withinDir(child, base string) bool {
	base = filepath.Clean(base)
	child = filepath.Clean(child)
	if child == base {
		return true
	}
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// BackupDirInfo reports the effective backup directory, whether it's
// the (unset) default, and the free space available on that volume.
func (a *App) BackupDirInfo() (dir string, isDefault bool, freeBytes uint64) {
	dir = a.Paths.BackupsDir()
	isDefault = a.State.Config.BackupDir == ""
	freeBytes, _ = diskFreeBytes(existingAncestor(dir))
	return dir, isDefault, freeBytes
}

// existingAncestor is dir, or its nearest parent that exists: the backup
// folder is only created by the first backup, and asking the OS for the
// free space of a missing folder fails (the panel showed "—").
func existingAncestor(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// SetSitePinned floats a site to the top of the Sites page (or unfloats
// it). It only persists the flag — pinning doesn't change how the site
// is served, so no Apply is needed.
func (a *App) SetSitePinned(name string, pinned bool) error {
	site := a.State.FindSite(name)
	if site == nil {
		return fmt.Errorf("no site named %q", name)
	}
	site.Pinned = pinned
	return a.State.Save()
}

// NodeNpm pairs an installed Node version with the npm version it
// currently ships, for the control panel's Node page.
type NodeNpm struct {
	Node    string `json:"node"`
	Npm     string `json:"npm"`
	Default bool   `json:"default"`
}

// NodeVersionsWithNpm lists every installed Node version alongside the
// npm version bundled in it.
func (a *App) NodeVersionsWithNpm() []NodeNpm {
	versions, err := nodever.Installed(a.Paths)
	if err != nil {
		return nil
	}
	out := make([]NodeNpm, 0, len(versions))
	for _, v := range versions {
		out = append(out, NodeNpm{
			Node:    v,
			Npm:     nodever.NpmVersionOf(a.Paths.NodeVersionDir(v)),
			Default: v == a.State.Config.GlobalNode,
		})
	}
	return out
}

// SiteInfo pairs a linked site's name with its detected project info
// and pin state, for the control panel's Sites page.
type SiteInfo struct {
	Name   string      `json:"name"`
	Detect detect.Info `json:"detect"`
	Pinned bool        `json:"pinned"`
}

// SitesInfo detects every linked site's project. The UI merges this
// with the existing site list from getState.
func (a *App) SitesInfo() []SiteInfo {
	out := make([]SiteInfo, 0, len(a.State.Sites))
	for _, s := range a.State.Sites {
		out = append(out, SiteInfo{
			Name:   s.Name,
			Detect: detect.Project(s.Path),
			Pinned: s.Pinned,
		})
	}
	return out
}
