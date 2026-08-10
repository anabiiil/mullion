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
}

type Site struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// PHP is a full version pinned for this site ("" = follow global).
	PHP    string `json:"php,omitempty"`
	Secure bool   `json:"secure"`
}

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
