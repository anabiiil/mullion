package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

// Feed merges a site's sources into one stream of Items: Caddy's access
// log (requests), Laravel's storage/logs/*.log (logs, exceptions) and
// the PHP side's events.jsonl (everything else). It is not safe for
// concurrent use: one Feed per reader, polled from one goroutine.
type Feed struct {
	paths    pmdir.Paths
	site     string
	settings Settings
	clear    ClearState

	caddy   *Tailer
	events  *Tailer
	rotated string
	logDir  string
	logs    map[string]*logTail
	scanned time.Time
	loc     *time.Location

	// queries per request/job, until its end event (bounded).
	queries map[string][]Query
	qOrder  []string
}

type logTail struct {
	t    *Tailer
	p    MonologParser
	idle int
}

const (
	maxOpenRequests = 300
	maxLogFiles     = 3
)

// NewFeed follows one site. host is its domain (the access log's name).
func NewFeed(p pmdir.Paths, s config.Site, host string) *Feed {
	f := &Feed{
		paths:    p,
		site:     s.Name,
		settings: SettingsOf(s),
		clear:    LoadClear(p, s.Name),
		caddy:    NewTailer(caddyLogPath(p, host)),
		events:   NewTailer(EventsPath(p, s.Name)),
		rotated:  RotatedPath(p, s.Name),
		logDir:   filepath.Join(s.Path, "storage", "logs"),
		logs:     map[string]*logTail{},
		queries:  map[string][]Query{},
		loc:      appTimezone(s.Path),
	}
	f.caddy.Floor = f.clear.Offsets[f.caddy.Path]
	return f
}

func caddyLogPath(p pmdir.Paths, host string) string { return filepath.Join(p.LogsDir(), host+".log") }

// Sources is the files a feed reads that aren't Mullion's own: Caddy's
// access log for the site and the project's storage/logs/*.log. A clear
// records where they end (see Clear).
func Sources(p pmdir.Paths, s config.Site, host string) []string {
	return append([]string{caddyLogPath(p, host)}, ProjectLogFiles(s.Path)...)
}

// appTimezone is the zone Laravel writes log timestamps in: APP_TIMEZONE
// from .env (Laravel 11+), else UTC (the framework default).
func appTimezone(dir string) *time.Location {
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if v, ok := strings.CutPrefix(l, "APP_TIMEZONE="); ok {
				v = strings.Trim(strings.TrimSpace(v), `"'`)
				if loc, err := time.LoadLocation(v); err == nil && v != "" {
					return loc
				}
			}
		}
	}
	return time.UTC
}

// Settings is the site's watcher settings the feed was opened with.
func (f *Feed) Settings() Settings { return f.settings }

// Backlog returns recent history (about n items per source, oldest
// first) and positions every source at its end.
func (f *Feed) Backlog(n int) []Item {
	var out []Item
	for _, l := range f.caddy.Backlog(n, 4<<20) {
		if it, ok := ParseCaddyLine(l); ok {
			out = append(out, it)
		}
	}
	// The rotated events file first, when the current one is short.
	cur := f.events.Backlog(n*4, 8<<20)
	if len(cur) < n*4 {
		old := NewTailer(f.rotated)
		cur = append(old.Backlog(n*4-len(cur), 8<<20), cur...)
	}
	out = append(out, f.eventItems(cur)...)
	f.scanLogs(true)
	for name, lt := range f.logs {
		for _, l := range lt.t.Backlog(n*20, 4<<20) {
			if e := lt.p.Feed(l); e != nil {
				out = append(out, e.Item(name))
			}
		}
		if e := lt.p.Flush(); e != nil {
			out = append(out, e.Item(name))
		}
	}
	return f.finish(out)
}

// Poll returns what arrived since the last call.
func (f *Feed) Poll() []Item {
	var out []Item
	for _, l := range f.caddy.Poll() {
		if it, ok := ParseCaddyLine(l); ok {
			out = append(out, it)
		}
	}
	out = append(out, f.eventItems(f.events.Poll())...)
	if time.Since(f.scanned) > 3*time.Second {
		f.scanLogs(false)
	}
	for name, lt := range f.logs {
		lines := lt.t.Poll()
		for _, l := range lines {
			if e := lt.p.Feed(l); e != nil {
				out = append(out, e.Item(name))
			}
		}
		// An entry ends at the next header — or when the file has been
		// quiet for a couple of polls.
		if len(lines) == 0 && lt.p.Pending() {
			if lt.idle++; lt.idle >= 2 {
				if e := lt.p.Flush(); e != nil {
					out = append(out, e.Item(name))
				}
				lt.idle = 0
			}
		} else {
			lt.idle = 0
		}
	}
	return f.finish(out)
}

func (f *Feed) finish(items []Item) []Item {
	if f.clear.Stamp() > 0 {
		kept := items[:0]
		for _, it := range items {
			if !f.clear.Hides(it) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Time() < items[j].Time() })
	return items
}

// Cleared re-reads the clear marks after a Clear: older history is
// hidden. A full clear also drops what was being collected (queries of
// open requests, a half-read log entry).
func (f *Feed) Cleared() {
	prev := f.clear.At
	f.clear = LoadClear(f.paths, f.site)
	if f.clear.At != prev {
		f.queries = map[string][]Query{}
		f.qOrder = nil
		for _, lt := range f.logs {
			lt.p = MonologParser{Loc: f.loc}
		}
	}
}

// scanLogs picks the most recently written *.log files (laravel.log or
// the daily laravel-YYYY-MM-DD.log). New files start from their
// beginning; the initial scan (backlog) leaves positioning to Backlog.
func (f *Feed) scanLogs(initial bool) {
	f.scanned = time.Now()
	matches, _ := filepath.Glob(filepath.Join(f.logDir, "*.log"))
	type cand struct {
		path string
		mod  time.Time
	}
	var cs []cand
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			cs = append(cs, cand{m, fi.ModTime()})
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].mod.After(cs[j].mod) })
	if len(cs) > maxLogFiles {
		cs = cs[:maxLogFiles]
	}
	for _, c := range cs {
		name := filepath.Base(c.path)
		if f.logs[name] != nil {
			continue
		}
		lt := &logTail{t: NewTailer(c.path)}
		lt.t.Floor = f.clear.Offsets[c.path]
		lt.p.Loc = f.loc
		if !initial {
			lt.t.started = true // a file that appeared now: read it all
		}
		f.logs[name] = lt
	}
}

// eventItems decodes the PHP side's JSON lines, re-applies redaction,
// and adds an "analysis" item when a request or job ends.
func (f *Feed) eventItems(lines []string) []Item {
	var out []Item
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l[0] != '{' {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(l), &it); err != nil || it.Type() == "" {
			continue
		}
		it["src"] = "php"
		rid := it.RID()
		switch it.Type() {
		case "query":
			sql, _ := it["sql"].(string)
			b, _ := it["bindings"].([]any)
			b = RedactBindings(sql, b)
			it["bindings"] = b
			it["norm"] = NormalizeSQL(sql)
			ms, _ := it["ms"].(float64)
			file, _ := it["file"].(string)
			line, _ := it["line"].(float64)
			f.addQuery(rid, Query{SQL: sql, Bindings: b, Ms: ms, File: file, Line: int(line)})
		case "http":
			if h, ok := it["headers"].(map[string]any); ok {
				for k := range h {
					if IsSecretName(k) {
						h[k] = Redacted
					}
				}
			}
		}
		out = append(out, it)
		if ends(it) {
			if a, ok := f.analysis(rid, it.Time()); ok {
				out = append(out, a)
			}
		}
	}
	return out
}

// ends reports whether an item closes its request/job timeline.
func ends(it Item) bool {
	switch it.Type() {
	case "request", "command":
		return true
	case "job":
		op, _ := it["op"].(string)
		return op == "processed" || op == "failed"
	}
	return false
}

func (f *Feed) addQuery(rid string, q Query) {
	if _, ok := f.queries[rid]; !ok {
		f.qOrder = append(f.qOrder, rid)
		if len(f.qOrder) > maxOpenRequests {
			delete(f.queries, f.qOrder[0])
			f.qOrder = f.qOrder[1:]
		}
	}
	if len(f.queries[rid]) < 5000 {
		f.queries[rid] = append(f.queries[rid], q)
	}
}

func (f *Feed) analysis(rid string, t float64) (Item, bool) {
	qs := f.queries[rid]
	if len(qs) == 0 {
		return nil, false
	}
	delete(f.queries, rid)
	for i, r := range f.qOrder {
		if r == rid {
			f.qOrder = append(f.qOrder[:i], f.qOrder[i+1:]...)
			break
		}
	}
	a := Analyze(qs, f.settings.NPlusOne, float64(f.settings.SlowQueryMs))
	return Item{"type": "analysis", "src": "php", "t": t, "rid": rid, "queries": a.Queries, "ms": a.Ms,
		"slow": a.Slow, "duplicates": a.Duplicates, "issues": a.Issues}, true
}

// Recent returns the request-side history only — Caddy's access log and
// the PHP side's events (with their analysis items), not storage/logs —
// about n requests deep, oldest first, with the clear marks applied.
// The Performance tab aggregates it per route.
func (f *Feed) Recent(n int) []Item {
	var out []Item
	for _, l := range f.caddy.Backlog(n, 8<<20) {
		if it, ok := ParseCaddyLine(l); ok {
			out = append(out, it)
		}
	}
	cur := f.events.Backlog(n*20, 24<<20)
	if len(cur) < n*20 {
		old := NewTailer(f.rotated)
		cur = append(old.Backlog(n*20-len(cur), 24<<20), cur...)
	}
	out = append(out, f.eventItems(cur)...)
	return f.finish(out)
}
