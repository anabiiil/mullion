package watch

// Clearing a site's watcher history. Our own events files are emptied;
// Caddy's access log and the project's storage/logs/*.log aren't ours to
// empty (unless the user asks — ClearProjectLogs), so a clear records
// where those files ended (and when) and the feed starts after that —
// on reconnect and after a panel restart too.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// ClearState is a site's persisted "since" marks (<site dir>/clear.json).
type ClearState struct {
	// At hides everything older (Clear everything).
	At float64 `json:"at,omitempty"`
	// Types hides older items of one kind ("Clear this view"): keyed by
	// item type ("query") or type/src ("exception/file").
	Types map[string]float64 `json:"types,omitempty"`
	// Offsets is where each source file ended at the clear: the feed
	// starts reading there (unless the file has since been truncated or
	// replaced, which is when it is shorter than the mark).
	Offsets map[string]int64 `json:"offsets,omitempty"`
	// ByOffset lists the marks ("" = At, else a Types key) that hide
	// storage/logs items by Offsets instead of by time: a log's
	// timestamps are only as good as the app-timezone guess, and a wrong
	// guess would hide lines written after the clear.
	ByOffset []string `json:"byOffset,omitempty"`
}

func clearPath(p pmdir.Paths, site string) string {
	return filepath.Join(p.WatchSiteDir(site), "clear.json")
}

// legacyClearedPath is the older single-timestamp file, still honoured.
func legacyClearedPath(p pmdir.Paths, site string) string {
	return filepath.Join(p.WatchSiteDir(site), "cleared")
}

// LoadClear reads a site's clear marks (zero value: never cleared).
func LoadClear(p pmdir.Paths, site string) ClearState {
	var c ClearState
	if data, err := os.ReadFile(clearPath(p, site)); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	if data, err := os.ReadFile(legacyClearedPath(p, site)); err == nil {
		if f, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); f > c.At {
			c.At = f
		}
	}
	return c
}

func saveClear(p pmdir.Paths, site string, c ClearState) error {
	dir := p.WatchSiteDir(site)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := clearPath(p, site) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, clearPath(p, site)); err != nil {
		return err
	}
	_ = os.Remove(legacyClearedPath(p, site))
	return nil
}

// Stamp changes with every clear (live streams poll it).
func (c ClearState) Stamp() float64 {
	s := c.At
	for _, t := range c.Types {
		if t > s {
			s = t
		}
	}
	return s
}

// Hides reports whether a clear hides the item.
func (c ClearState) Hides(it Item) bool {
	t := it.Time()
	src, _ := it["src"].(string)
	byTime := func(key string) bool { return src != "file" || !contains(c.ByOffset, key) }
	if t < c.At && byTime("") {
		return true
	}
	if len(c.Types) == 0 {
		return false
	}
	typ := it.Type()
	if t < c.Types[typ] && byTime(typ) {
		return true
	}
	return src != "" && t < c.Types[typ+"/"+src] && byTime(typ+"/"+src)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ClearedAt is when the site's watcher was last cleared (0 = never).
func ClearedAt(p pmdir.Paths, site string) float64 { return LoadClear(p, site).At }

func nowSeconds() float64 { return float64(time.Now().UnixMilli()) / 1000 }

// fileEnds is each existing file's current size.
func fileEnds(paths []string) map[string]int64 {
	out := map[string]int64{}
	for _, path := range paths {
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			out[path] = fi.Size()
		}
	}
	return out
}

// Clear empties the site's events files and hides everything older —
// in sources (Caddy's log, storage/logs/*.log: see Sources) by where
// they end now, so their old lines don't come back on reconnect.
func Clear(p pmdir.Paths, site string, sources ...string) error {
	dir := p.WatchSiteDir(site)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Truncate(EventsPath(p, site), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(RotatedPath(p, site)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c := ClearState{At: nowSeconds(), Offsets: fileEnds(sources)}
	if len(c.Offsets) > 0 {
		c.ByOffset = []string{""}
	}
	return saveClear(p, site, c)
}

// ClearTypes hides the older items of some kinds (one view of the Watch
// tab). Marking log-file kinds also moves those files' marks (sources)
// to their ends: their timestamps are only as good as the app's
// timezone guess.
func ClearTypes(p pmdir.Paths, site string, types []string, sources ...string) error {
	c := LoadClear(p, site)
	now := nowSeconds()
	if c.Types == nil {
		c.Types = map[string]float64{}
	}
	byOffset := c.ByOffset[:0:0]
	for _, k := range c.ByOffset {
		if !contains(types, k) {
			byOffset = append(byOffset, k)
		}
	}
	for _, t := range types {
		if t = strings.TrimSpace(t); t != "" {
			c.Types[t] = now
			if len(sources) > 0 && (t == "log" || t == "exception/file") {
				byOffset = append(byOffset, t)
			}
		}
	}
	c.ByOffset = byOffset
	if len(sources) > 0 {
		if c.Offsets == nil {
			c.Offsets = map[string]int64{}
		}
		for k, v := range fileEnds(sources) {
			c.Offsets[k] = v
		}
	}
	return saveClear(p, site, c)
}

// ProjectLogFiles is the project's storage/logs/*.log.
func ProjectLogFiles(projectDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(projectDir, "storage", "logs", "*.log"))
	var out []string
	for _, m := range matches {
		if fi, err := os.Lstat(m); err == nil && fi.Mode().IsRegular() {
			out = append(out, m)
		}
	}
	return out
}

// ClearProjectLogs empties the project's storage/logs/*.log files (they
// stay, empty: the app keeps writing to them). Returns the files emptied.
func ClearProjectLogs(projectDir string) ([]string, error) {
	var done []string
	for _, f := range ProjectLogFiles(projectDir) {
		if err := os.Truncate(f, 0); err != nil {
			return done, err
		}
		done = append(done, f)
	}
	return done, nil
}

// DataSize is what the site's watcher keeps on disk (its events files
// and settings), in bytes.
func DataSize(p pmdir.Paths, site string) int64 {
	var n int64
	entries, _ := os.ReadDir(p.WatchSiteDir(site))
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}
