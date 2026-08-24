// Package config persists the tool's global settings and the registry of
// linked sites as JSON files under ~/.mullion.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	// GlobalNode is the full Node version the node/current junction points at.
	GlobalNode string `json:"globalNode,omitempty"`
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
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
