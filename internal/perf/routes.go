// Package perf is the project page's Performance tab (and `mullion
// perf`): per-route timings built from the Watch tab's data, budgets
// and saved baselines, a production-readiness audit (Laravel, PHP,
// database schema, frontend build), and the stored results of load
// tests. Everything is computed from local files; nothing leaves the
// computer.
package perf

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pm/internal/watch"
)

// Request is one HTTP request as the route table needs it: Caddy's
// access-log line joined (by request id) with what the PHP side
// recorded for it.
type Request struct {
	RID     string  `json:"rid,omitempty"`
	T       float64 `json:"t"`
	Method  string  `json:"method"`
	Path    string  `json:"path"`
	Route   string  `json:"route,omitempty"` // Laravel's uri ("posts/{post}")
	Name    string  `json:"name,omitempty"`  // route name
	Action  string  `json:"action,omitempty"`
	Status  int     `json:"status"`
	Ms      float64 `json:"ms"`
	Queries int     `json:"queries"`
	QueryMs float64 `json:"queryMs"`
	Memory  int64   `json:"memory"` // peak, bytes (0 = unknown)
	// NPlusOne counts the N+1 patterns the Watch analysis found.
	NPlusOne int `json:"nPlusOne"`
	// Deep: the PHP side recorded queries for it (the deep watcher).
	Deep bool `json:"deep"`
}

// RequestsFromItems joins a Watch feed's items into requests: Caddy's
// "request" lines (static assets left out), the PHP side's "request"
// events and their "analysis" items, matched by request id. A PHP
// event without a Caddy line (no request ids — Caddy before 2.8) is a
// request of its own.
func RequestsFromItems(items []watch.Item) []Request {
	var out []*Request
	byRID := map[string]*Request{}
	get := func(rid string) *Request {
		if rid == "" {
			return nil
		}
		return byRID[rid]
	}
	add := func(r *Request) {
		out = append(out, r)
		if r.RID != "" {
			byRID[r.RID] = r
		}
	}
	for _, it := range items {
		src, _ := it["src"].(string)
		rid := it.RID()
		switch it.Type() {
		case "request":
			if src == "caddy" {
				if st, _ := it["static"].(bool); st {
					continue
				}
				p, _ := it["path"].(string)
				r := get(rid)
				if r == nil {
					r = &Request{RID: rid}
					add(r)
				}
				r.T = it.Time()
				r.Method = str(it["method"])
				r.Path = p
				r.Status = num(it["status"])
				r.Ms = flt(it["ms"]) // the whole request, as the browser waited
				continue
			}
			r := get(rid)
			if r == nil {
				r = &Request{RID: rid, T: it.Time(), Method: str(it["method"]), Status: num(it["status"]), Ms: flt(it["ms"])}
				uri := str(it["uri"])
				if i := strings.IndexByte(uri, '?'); i >= 0 {
					uri = uri[:i]
				}
				r.Path = uri
				add(r)
			}
			if m, ok := it["route"].(map[string]any); ok {
				r.Route = str(m["uri"])
				r.Name = str(m["name"])
				r.Action = str(m["action"])
			}
			if v := int64(flt(it["memory"])); v > 0 {
				r.Memory = v
			}
			if _, ok := it["queries"]; ok {
				r.Deep = true
				r.Queries = num(it["queries"])
				r.QueryMs = flt(it["queryMs"])
			}
			if r.Status == 0 {
				r.Status = num(it["status"])
			}
		case "analysis":
			r := get(rid)
			if r == nil {
				continue // a job's or a command's
			}
			if !r.Deep {
				r.Deep = true
				r.Queries = num(it["queries"])
				r.QueryMs = flt(it["ms"])
			}
			for _, is := range issuesOf(it) {
				if is.Kind == "n+1" {
					r.NPlusOne++
				}
			}
		}
	}
	res := make([]Request, 0, len(out))
	for _, r := range out {
		if r.Method == "" || r.Path == "" {
			continue
		}
		res = append(res, *r)
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].T < res[j].T })
	return res
}

var (
	uuidSeg  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	ulidSeg  = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)
	hexSeg   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	digitSeg = regexp.MustCompile(`^\d+$`)
	// A slug ending in an id (post-42, 42-hello) stays as typed: it is
	// usually one route per slug only on purpose.
)

// NormalizePath reduces a request path to its route shape: numeric ids,
// UUIDs, ULIDs and long hex strings become {id}; the query string and a
// trailing slash go.
func NormalizePath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == "/" {
		return "/"
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		if digitSeg.MatchString(s) || uuidSeg.MatchString(s) || hexSeg.MatchString(s) || (ulidSeg.MatchString(s) && hasDigit(s)) {
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// RouteKey is a request's row in the table: method and route pattern
// (Laravel's own when the PHP side recorded it, else the normalized
// path).
func RouteKey(r Request) (key, pattern string) {
	pattern = NormalizePath(r.Path)
	if r.Route != "" {
		pattern = "/" + strings.TrimPrefix(r.Route, "/")
	}
	m := strings.ToUpper(r.Method)
	if m == "HEAD" {
		m = "GET"
	}
	return m + " " + pattern, pattern
}

// Budgets are a site's per-route limits (see config.Perf).
type Budgets struct {
	MaxMs       int `json:"maxMs"`
	MaxQueries  int `json:"maxQueries"`
	MaxMemoryMB int `json:"maxMemoryMB"`
}

// Default budgets.
const (
	DefaultMaxMs       = 300
	DefaultMaxQueries  = 30
	DefaultMaxMemoryMB = 64
)

// WithDefaults fills unset budgets.
func (b Budgets) WithDefaults() Budgets {
	if b.MaxMs <= 0 {
		b.MaxMs = DefaultMaxMs
	}
	if b.MaxQueries <= 0 {
		b.MaxQueries = DefaultMaxQueries
	}
	if b.MaxMemoryMB <= 0 {
		b.MaxMemoryMB = DefaultMaxMemoryMB
	}
	return b
}

// RouteStat is one row of the route table.
type RouteStat struct {
	Key     string  `json:"key"`
	Method  string  `json:"method"`
	Pattern string  `json:"pattern"`
	Name    string  `json:"name,omitempty"`
	Action  string  `json:"action,omitempty"`
	Count   int     `json:"count"`
	AvgMs   float64 `json:"avgMs"`
	P50Ms   float64 `json:"p50Ms"`
	P95Ms   float64 `json:"p95Ms"`
	P99Ms   float64 `json:"p99Ms"`
	MaxMs   float64 `json:"maxMs"`
	TotalMs float64 `json:"totalMs"`
	// Query stats come from requests the deep watcher saw (DeepCount).
	DeepCount  int     `json:"deepCount"`
	AvgQueries float64 `json:"avgQueries"`
	MaxQueries int     `json:"maxQueries"`
	AvgQueryMs float64 `json:"avgQueryMs"`
	PeakMemory int64   `json:"peakMemory"`
	AvgMemory  int64   `json:"avgMemory"`
	Errors     int     `json:"errors"`
	ErrorRate  float64 `json:"errorRate"` // 0..1, status >= 500
	NPlusOne   int     `json:"nPlusOne"`  // requests with an N+1 pattern
	LastT      float64 `json:"lastT"`
	LastPath   string  `json:"lastPath"`
	// Recent are the last durations (ms, oldest first) for a sparkline.
	Recent []float64 `json:"recent,omitempty"`
	// Over lists why the row breaks a budget ("" = within budget).
	Over []string `json:"over"`
}

// recentMax is how many durations a row keeps for its sparkline.
const recentMax = 40

// Aggregate builds the route table: one row per method + route pattern,
// slowest total time first.
func Aggregate(reqs []Request, b Budgets) []RouteStat {
	b = b.WithDefaults()
	type acc struct {
		st      RouteStat
		ms      []float64
		qSum    int
		qMsSum  float64
		memSum  int64
		memN    int64
		lastSet bool
	}
	rows := map[string]*acc{}
	var order []string
	// Requests the PHP side didn't describe (older than its events in
	// the window, or Watch without Deep) take the Laravel route of the
	// same normalized path when another request had it.
	learned := map[string]Request{}
	for _, r := range reqs {
		if r.Route != "" {
			learned[strings.ToUpper(r.Method)+" "+NormalizePath(r.Path)] = r
		}
	}
	for _, r := range reqs {
		if r.Route == "" {
			if l, ok := learned[strings.ToUpper(r.Method)+" "+NormalizePath(r.Path)]; ok {
				r.Route, r.Name, r.Action = l.Route, l.Name, l.Action
			}
		}
		key, pattern := RouteKey(r)
		a := rows[key]
		if a == nil {
			m, _, _ := strings.Cut(key, " ")
			a = &acc{st: RouteStat{Key: key, Method: m, Pattern: pattern}}
			rows[key] = a
			order = append(order, key)
		}
		s := &a.st
		s.Count++
		a.ms = append(a.ms, r.Ms)
		s.TotalMs += r.Ms
		if r.Ms > s.MaxMs {
			s.MaxMs = r.Ms
		}
		if r.Name != "" {
			s.Name = r.Name
		}
		if r.Action != "" {
			s.Action = r.Action
		}
		if r.Deep {
			s.DeepCount++
			a.qSum += r.Queries
			a.qMsSum += r.QueryMs
			if r.Queries > s.MaxQueries {
				s.MaxQueries = r.Queries
			}
		}
		if r.Memory > 0 {
			a.memSum += r.Memory
			a.memN++
			if r.Memory > s.PeakMemory {
				s.PeakMemory = r.Memory
			}
		}
		if r.Status >= 500 {
			s.Errors++
		}
		if r.NPlusOne > 0 {
			s.NPlusOne++
		}
		if r.T >= s.LastT {
			s.LastT, s.LastPath = r.T, r.Path
		}
	}
	out := make([]RouteStat, 0, len(rows))
	for _, k := range order {
		a := rows[k]
		s := a.st
		s.AvgMs = round2(s.TotalMs / float64(s.Count))
		s.TotalMs = round2(s.TotalMs)
		s.MaxMs = round2(s.MaxMs)
		sorted := append([]float64(nil), a.ms...)
		sort.Float64s(sorted)
		s.P50Ms, s.P95Ms, s.P99Ms = Percentile(sorted, 50), Percentile(sorted, 95), Percentile(sorted, 99)
		if s.DeepCount > 0 {
			s.AvgQueries = round2(float64(a.qSum) / float64(s.DeepCount))
			s.AvgQueryMs = round2(a.qMsSum / float64(s.DeepCount))
		}
		if a.memN > 0 {
			s.AvgMemory = a.memSum / a.memN
		}
		s.ErrorRate = round4(float64(s.Errors) / float64(s.Count))
		rec := a.ms
		if len(rec) > recentMax {
			rec = rec[len(rec)-recentMax:]
		}
		s.Recent = append([]float64(nil), rec...)
		s.Over = OverBudget(s, b)
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	return out
}

// OverBudget explains the budgets a row breaks.
func OverBudget(s RouteStat, b Budgets) []string {
	b = b.WithDefaults()
	over := []string{}
	if s.P95Ms > float64(b.MaxMs) {
		over = append(over, "p95 "+fmtMs(s.P95Ms)+" over the "+strconv.Itoa(b.MaxMs)+" ms budget")
	}
	if s.DeepCount > 0 && s.MaxQueries > b.MaxQueries {
		over = append(over, "up to "+strconv.Itoa(s.MaxQueries)+" queries (budget "+strconv.Itoa(b.MaxQueries)+")")
	}
	if mb := float64(s.PeakMemory) / (1 << 20); s.PeakMemory > 0 && mb > float64(b.MaxMemoryMB) {
		over = append(over, "peak memory "+strconv.FormatFloat(math.Round(mb*10)/10, 'f', -1, 64)+" MB (budget "+strconv.Itoa(b.MaxMemoryMB)+" MB)")
	}
	return over
}

// Percentile is the nearest-rank percentile of sorted values (0 when
// empty).
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return round2(sorted[rank-1])
}

func fmtMs(v float64) string {
	if v >= 1000 {
		return strconv.FormatFloat(math.Round(v/10)/100, 'f', -1, 64) + " s"
	}
	return strconv.FormatFloat(math.Round(v), 'f', -1, 64) + " ms"
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func str(v any) string { s, _ := v.(string); return s }
func flt(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
func num(v any) int { return int(flt(v)) }

// issuesOf reads an analysis item's issues: Go values when the feed
// made the item in this process, JSON-decoded maps otherwise.
func issuesOf(it watch.Item) []watch.QueryIssue {
	switch v := it["issues"].(type) {
	case []watch.QueryIssue:
		return v
	case []any:
		out := make([]watch.QueryIssue, 0, len(v))
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				out = append(out, watch.QueryIssue{Kind: str(m["kind"]), SQL: str(m["sql"]), File: str(m["file"]), Line: num(m["line"]), Count: num(m["count"]), Ms: flt(m["ms"])})
			}
		}
		return out
	}
	return nil
}
