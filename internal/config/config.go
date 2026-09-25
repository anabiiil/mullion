// Package config persists the tool's global settings and the registry of
// linked sites as JSON files under ~/.mullion.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pm/internal/pmdir"
)

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// Slugify normalizes a site name to lowercase letters, digits, and dashes.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

type Config struct {
	// TLD is the domain suffix appended to linked sites (default "test").
	TLD string `json:"tld"`
	// GlobalPHP is the full version (e.g. "8.3.26") the `current` junction points at.
	GlobalPHP string `json:"globalPhp"`
	// MySQL is the installed server version ("" = not installed).
	MySQL string `json:"mysql,omitempty"`
	// MySQLPassword is the root password of Mullion's MySQL ("" = none).
	// Stored in plain text: this is a LOCAL dev server bound to 127.0.0.1.
	MySQLPassword string `json:"mysqlPassword,omitempty"`
	// MySQLStopped records that the user stopped MySQL on purpose —
	// Apply/self-heal/autostart must not resurrect it until they start
	// it again. Starting MySQL clears this flag.
	MySQLStopped bool `json:"mysqlStopped,omitempty"`
	// Postgres is the installed server version ("" = not installed).
	Postgres string `json:"postgres,omitempty"`
	// PostgresPassword is the superuser password of Mullion's Postgres
	// ("" = none). Stored in plain text: a LOCAL dev server bound to
	// 127.0.0.1.
	PostgresPassword string `json:"postgresPassword,omitempty"`
	// PostgresStopped records that the user stopped Postgres on purpose
	// — see MySQLStopped.
	PostgresStopped bool `json:"postgresStopped,omitempty"`
	// Mongo is the installed server version ("" = not installed).
	Mongo string `json:"mongo,omitempty"`
	// MongoStopped records that the user stopped MongoDB on purpose —
	// see MySQLStopped.
	MongoStopped bool `json:"mongoStopped,omitempty"`
	// GlobalNode is the full Node version the node/current junction points at.
	GlobalNode string `json:"globalNode,omitempty"`
	// BackupDir is where database backups are written ("" = the default
	// next to the install root, which survives an uninstall).
	BackupDir string `json:"backupDir,omitempty"`
	// MySQLPort, PostgresPort and MongoPort override the engines' default
	// ports (3306, 5432, 27017) when they collide with something else.
	MySQLPort    int `json:"mysqlPort,omitempty"`
	PostgresPort int `json:"postgresPort,omitempty"`
	MongoPort    int `json:"mongoPort,omitempty"`
	// UI holds the control panel's preferences (terminal placement,
	// remembered tabs, favorite commands…). They live here, not in the
	// browser's storage, because the panel's origin (a random port)
	// changes on every launch and would forget them.
	UI map[string]string `json:"ui,omitempty"`
	// WildcardDNS enables Mullion's local DNS resolver for *.<tld>, so
	// sites' wildcard subdomains resolve without hosts-file entries.
	WildcardDNS bool `json:"wildcardDns,omitempty"`
}

type Site struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Kind is what serves the site: "" or "php" (php_fastcgi), "node"
	// (a managed dev server behind a reverse proxy), or "static"
	// (file_server over BuildDir — e.g. a frontend production build).
	Kind string `json:"kind,omitempty"`
	// PHP is a full version pinned for this site ("" = follow global).
	PHP string `json:"php,omitempty"`
	// Node is a full version pinned for this node site ("" = .nvmrc or global).
	Node string `json:"node,omitempty"`
	// BuildDir is the directory served for "static" sites, relative to Path.
	BuildDir string `json:"buildDir,omitempty"`
	// DevPort is the stable local port assigned to this site's dev server.
	DevPort int `json:"devPort,omitempty"`
	// Mode selects what a node site's domain serves: "" or "dev" (the
	// managed dev server) or "build" (the BuildDir production build).
	Mode string `json:"mode,omitempty"`
	// DevPaused records that the user stopped this site's dev server on
	// purpose — Mullion must not resurrect it until they start it again.
	DevPaused bool `json:"devPaused,omitempty"`
	Secure    bool `json:"secure"`
	// Aliases are extra subdomain labels served by this site, e.g.
	// "api" -> api.<name>.<tld>. A "*" entry serves every subdomain
	// (resolution then needs WildcardDNS).
	Aliases []string `json:"aliases,omitempty"`
	// Pinned floats the site to the top of the Sites page.
	Pinned bool `json:"pinned,omitempty"`
	// Workers are long-running background commands supervised for this
	// site (queue workers, the Laravel scheduler, custom daemons).
	Workers []Worker `json:"workers,omitempty"`
}

// Worker is one supervised background command of a site.
type Worker struct {
	// ID is stable and unique within the site (used in pid/log names).
	ID string `json:"id"`
	// Name is the label shown in the UI ("Queue: default").
	Name string `json:"name"`
	// Kind hints the UI and defaults: "queue", "scheduler", "custom".
	Kind string `json:"kind"`
	// Command runs in the site's folder through the platform shell,
	// with the site's PHP/Node on PATH (e.g. "php artisan queue:work").
	Command string `json:"command"`
	// AutoStart brings it up with the stack (and restarts it if it
	// crashes); false = only when started by hand.
	AutoStart bool `json:"autoStart,omitempty"`
	// Paused records a deliberate stop, like DevPaused.
	Paused bool `json:"paused,omitempty"`
}

// IsPHP reports whether the site is served through php_fastcgi.
func (s Site) IsPHP() bool { return s.Kind == "" || s.Kind == "php" }

type State struct {
	Config Config
	Sites  []Site

	paths pmdir.Paths
}

func Load(paths pmdir.Paths) (*State, error) {
	s := &State{
		Config: Config{TLD: "test"},
		paths:  paths,
	}
	if err := readJSON(paths.ConfigFile(), &s.Config); err != nil {
		return nil, fmt.Errorf("reading %s: %w", paths.ConfigFile(), err)
	}
	if s.Config.TLD == "" {
		s.Config.TLD = "test"
	}
	if err := readJSON(paths.SitesFile(), &s.Sites); err != nil {
		return nil, fmt.Errorf("reading %s: %w", paths.SitesFile(), err)
	}
	return s, nil
}

func (s *State) Save() error {
	if err := writeJSON(s.paths.ConfigFile(), s.Config); err != nil {
		return err
	}
	return writeJSON(s.paths.SitesFile(), s.Sites)
}

// Host returns the full hostname for a site, e.g. "blog.test".
func (s *State) Host(site Site) string {
	return site.Name + "." + s.Config.TLD
}

func (s *State) FindSite(name string) *Site {
	for i := range s.Sites {
		if strings.EqualFold(s.Sites[i].Name, name) {
			return &s.Sites[i]
		}
	}
	return nil
}

// FindSiteByPath matches a site by its linked directory.
func (s *State) FindSiteByPath(path string) *Site {
	for i := range s.Sites {
		if strings.EqualFold(s.Sites[i].Path, path) {
			return &s.Sites[i]
		}
	}
	return nil
}

func (s *State) AddSite(site Site) {
	s.Sites = append(s.Sites, site)
	sort.Slice(s.Sites, func(i, j int) bool { return s.Sites[i].Name < s.Sites[j].Name })
}

// RenameSite gives a site a new name (and therefore a new domain).
func (s *State) RenameSite(oldName, newName string) error {
	newName = Slugify(newName)
	if newName == "" {
		return fmt.Errorf("invalid site name")
	}
	site := s.FindSite(oldName)
	if site == nil {
		return fmt.Errorf("no site named %q", oldName)
	}
	if !strings.EqualFold(oldName, newName) && s.FindSite(newName) != nil {
		return fmt.Errorf("a site named %q already exists", newName)
	}
	site.Name = newName
	sort.Slice(s.Sites, func(i, j int) bool { return s.Sites[i].Name < s.Sites[j].Name })
	return nil
}

func (s *State) RemoveSite(name string) bool {
	for i := range s.Sites {
		if strings.EqualFold(s.Sites[i].Name, name) {
			s.Sites = append(s.Sites[:i], s.Sites[i+1:]...)
			return true
		}
	}
	return false
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Write a temp file and rename it over the target: the panel reads
	// these files on every request, and a reader racing a plain
	// truncate-and-write would see an empty or half-written file.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
