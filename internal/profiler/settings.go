// Package profiler is the PHP profiler behind `mullion profile` and the
// project page's Performance → Profiler tab: per-site SPX settings
// (which Caddy passes to php-fpm, see fastcgi.go), the request gate for
// the "next N" and "matching path" modes (gate.go), and reading SPX's
// reports into a flame-graph tree and a top-functions table (report.go).
//
// The SPX extension itself is built and loaded per PHP version by
// phpver.EnsureSpx; this package never touches php.ini.
package profiler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// Mode is what a profiled site profiles.
type Mode string

const (
	// ModeBrowser profiles only requests that ask for it: the browser
	// session SPX's control panel switched on (cookies), or a URL with
	// ?SPX_ENABLED=1&SPX_KEY=…. Nothing else pays anything.
	ModeBrowser Mode = "browser"
	// ModeNext profiles the next Count requests (those matching Path,
	// when set), then stops.
	ModeNext Mode = "next"
	// ModePath profiles every request whose path matches Path.
	ModePath Mode = "path"
	// ModeAll profiles every request (development only: each one writes
	// a report).
	ModeAll Mode = "all"
)

// ParseMode validates a mode name ("" = browser).
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", ModeBrowser:
		return ModeBrowser, nil
	case ModeNext, ModePath, ModeAll:
		return m, nil
	}
	return "", fmt.Errorf("unknown profiling mode %q (browser, next, path or all)", s)
}

// usesGate: the mode decides per request in the PHP gate.
func (m Mode) usesGate() bool { return m == ModeNext || m == ModePath }

// Site is one site's profiler settings.
type Site struct {
	Mode Mode `json:"mode"`
	// Count is how many requests ModeNext profiles.
	Count int `json:"count,omitempty"`
	// Path limits ModeNext/ModePath to matching request paths: a
	// prefix ("/api") or a glob ("/api/*/orders", * crosses slashes).
	Path string `json:"path,omitempty"`
	// Builtins also profiles PHP's internal functions (SPX_BUILTINS).
	Builtins bool `json:"builtins,omitempty"`
	// Key is the site's SPX key (spx.http_key): the browser trigger and
	// SPX's own web UI need it.
	Key string `json:"key"`
	// Since is when profiling was turned on (unix seconds).
	Since int64 `json:"since"`
}

// Limits cap what the reports of one site may keep on disk; the oldest
// go first.
type Limits struct {
	MaxReports int   `json:"maxReports"`
	MaxBytes   int64 `json:"maxBytes"`
}

// DefaultLimits keeps the newest 100 reports and at most 256 MB a site.
var DefaultLimits = Limits{MaxReports: 100, MaxBytes: 256 << 20}

// State is ~/.mullion/profiler/state.json.
type State struct {
	Sites  map[string]*Site `json:"sites"`
	Limits Limits           `json:"limits"`
}

// Dir is the profiler's root: ~/.mullion/profiler.
func Dir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "profiler") }

// SiteDir holds one site's gate config and reports.
func SiteDir(paths pmdir.Paths, site string) string { return filepath.Join(Dir(paths), site) }

// ReportsDir is the site's spx.data_dir.
func ReportsDir(paths pmdir.Paths, site string) string {
	return filepath.Join(SiteDir(paths, site), "reports")
}

func statePath(paths pmdir.Paths) string { return filepath.Join(Dir(paths), "state.json") }

// LoadState reads the profiler state (an empty one when there is none).
func LoadState(paths pmdir.Paths) (*State, error) {
	st := &State{Sites: map[string]*Site{}, Limits: DefaultLimits}
	data, err := os.ReadFile(statePath(paths))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("reading %s: %w", statePath(paths), err)
	}
	if st.Sites == nil {
		st.Sites = map[string]*Site{}
	}
	if st.Limits.MaxReports <= 0 {
		st.Limits.MaxReports = DefaultLimits.MaxReports
	}
	if st.Limits.MaxBytes <= 0 {
		st.Limits.MaxBytes = DefaultLimits.MaxBytes
	}
	return st, nil
}

// Save writes the state atomically.
func (st *State) Save(paths pmdir.Paths) error {
	if err := os.MkdirAll(Dir(paths), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(paths) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(paths))
}

// Get returns a site's settings (nil = not profiled).
func (st *State) Get(site string) *Site {
	if st == nil {
		return nil
	}
	return st.Sites[site]
}

// NewKey returns a random spx.http_key.
func NewKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Validate normalizes s and checks it is complete.
func (s *Site) Validate() error {
	m, err := ParseMode(string(s.Mode))
	if err != nil {
		return err
	}
	s.Mode = m
	s.Path = strings.TrimSpace(s.Path)
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") && !strings.HasPrefix(s.Path, "*") {
		s.Path = "/" + s.Path
	}
	switch s.Mode {
	case ModeNext:
		if s.Count <= 0 {
			s.Count = 10
		}
		if s.Count > 10000 {
			return errors.New("profile at most 10000 requests at a time")
		}
	case ModePath:
		if s.Path == "" {
			return errors.New("say which paths to profile, e.g. /api or /checkout/*")
		}
	}
	if s.Key == "" {
		k, err := NewKey()
		if err != nil {
			return err
		}
		s.Key = k
	}
	if s.Since == 0 {
		s.Since = time.Now().Unix()
	}
	return nil
}
