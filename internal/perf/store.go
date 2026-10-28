package perf

// What the Performance tab keeps on disk, under <mullion home>/perf/<site>/:
// baselines/<name>.json (route-table snapshots), loadtests/<stamp>.json
// (load-test results) and reset.json (when the route stats were last
// reset).

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// Dir is a site's Performance folder.
func Dir(p pmdir.Paths, site string) string { return filepath.Join(p.Home, "perf", site) }

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── reset ────────────────────────────────────────────────────────────

// ResetAt is when the site's route stats were last reset (unix seconds,
// 0 = never). Requests before it are left out of the table — on top of
// the Watch tab's own Clear, which the feed already applies.
func ResetAt(p pmdir.Paths, site string) float64 {
	var v struct {
		At float64 `json:"at"`
	}
	data, err := os.ReadFile(filepath.Join(Dir(p, site), "reset.json"))
	if err == nil {
		_ = json.Unmarshal(data, &v)
	}
	return v.At
}

// Reset starts the route stats over from now.
func Reset(p pmdir.Paths, site string) error {
	return writeJSON(filepath.Join(Dir(p, site), "reset.json"), map[string]float64{"at": float64(time.Now().UnixMilli()) / 1000})
}

// Since keeps the requests at or after t.
func Since(reqs []Request, t float64) []Request {
	if t <= 0 {
		return reqs
	}
	out := reqs[:0:0]
	for _, r := range reqs {
		if r.T >= t {
			out = append(out, r)
		}
	}
	return out
}

// ── baselines ────────────────────────────────────────────────────────

// Baseline is a saved route table.
type Baseline struct {
	Name    string      `json:"name"`
	SavedAt time.Time   `json:"savedAt"`
	Note    string      `json:"note,omitempty"` // e.g. "production-like mode"
	Routes  []RouteStat `json:"routes"`
}

var baselineName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,59}$`)

func baselinePath(p pmdir.Paths, site, name string) string {
	file := strings.ReplaceAll(strings.TrimSpace(name), " ", "_")
	return filepath.Join(Dir(p, site), "baselines", file+".json")
}

// SaveBaseline snapshots a route table under a name (letters, digits,
// space . _ -; replaces one of the same name).
func SaveBaseline(p pmdir.Paths, site, name, note string, routes []RouteStat) (Baseline, error) {
	name = strings.TrimSpace(name)
	if !baselineName.MatchString(name) {
		return Baseline{}, errors.New("a baseline name is letters, digits, spaces, dots, dashes or underscores (up to 60)")
	}
	if len(routes) == 0 {
		return Baseline{}, errors.New("there are no routes to save yet — use the site first")
	}
	rs := make([]RouteStat, len(routes))
	for i, r := range routes {
		r.Recent = nil
		rs[i] = r
	}
	b := Baseline{Name: name, SavedAt: time.Now().UTC().Truncate(time.Second), Note: note, Routes: rs}
	return b, writeJSON(baselinePath(p, site, name), b)
}

// LoadBaseline reads one baseline.
func LoadBaseline(p pmdir.Paths, site, name string) (Baseline, error) {
	var b Baseline
	if !baselineName.MatchString(strings.TrimSpace(name)) {
		return b, fmt.Errorf("no baseline named %q", name)
	}
	data, err := os.ReadFile(baselinePath(p, site, name))
	if err != nil {
		return b, fmt.Errorf("no baseline named %q", name)
	}
	err = json.Unmarshal(data, &b)
	return b, err
}

// BaselineInfo is a baseline in the list.
type BaselineInfo struct {
	Name    string    `json:"name"`
	SavedAt time.Time `json:"savedAt"`
	Note    string    `json:"note,omitempty"`
	Routes  int       `json:"routes"`
}

// ListBaselines lists a site's baselines, newest first.
func ListBaselines(p pmdir.Paths, site string) []BaselineInfo {
	matches, _ := filepath.Glob(filepath.Join(Dir(p, site), "baselines", "*.json"))
	out := []BaselineInfo{}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var b Baseline
		if json.Unmarshal(data, &b) != nil || b.Name == "" {
			continue
		}
		out = append(out, BaselineInfo{Name: b.Name, SavedAt: b.SavedAt, Note: b.Note, Routes: len(b.Routes)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out
}

// DeleteBaseline removes one.
func DeleteBaseline(p pmdir.Paths, site, name string) error {
	if !baselineName.MatchString(strings.TrimSpace(name)) {
		return fmt.Errorf("no baseline named %q", name)
	}
	err := os.Remove(baselinePath(p, site, name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no baseline named %q", name)
	}
	return err
}

// RouteDelta compares one route with a baseline.
type RouteDelta struct {
	Key     string `json:"key"`
	Status  string `json:"status"` // "changed", "new" (not in the baseline), "gone" (only there)
	Count   int    `json:"count"`
	BaseCnt int    `json:"baseCount"`
	// Values now and then, and the change in percent (0 when either is 0).
	P95Ms      float64 `json:"p95Ms"`
	BaseP95Ms  float64 `json:"baseP95Ms"`
	P95Pct     float64 `json:"p95Pct"`
	AvgMs      float64 `json:"avgMs"`
	BaseAvgMs  float64 `json:"baseAvgMs"`
	AvgPct     float64 `json:"avgPct"`
	Queries    float64 `json:"avgQueries"`
	BaseQ      float64 `json:"baseAvgQueries"`
	QueriesPct float64 `json:"queriesPct"`
	Memory     int64   `json:"peakMemory"`
	BaseMemory int64   `json:"basePeakMemory"`
	MemoryPct  float64 `json:"memoryPct"`
	// Regression: p95, average queries or peak memory grew past
	// RegressionPct (and by a noticeable absolute amount).
	Regression  bool `json:"regression"`
	Improvement bool `json:"improvement"`
}

// RegressionPct is the growth that marks a regression.
const RegressionPct = 20

func pct(now, base float64) float64 {
	if base == 0 || now == 0 {
		return 0
	}
	return math.Round((now-base)/base*1000) / 10
}

// Diff compares the current route table with a baseline, regressions
// first (largest p95 growth first), then new routes, the rest, and the
// routes gone since.
func Diff(cur, base []RouteStat) []RouteDelta {
	bm := map[string]RouteStat{}
	for _, b := range base {
		bm[b.Key] = b
	}
	out := []RouteDelta{}
	seen := map[string]bool{}
	for _, c := range cur {
		seen[c.Key] = true
		d := RouteDelta{Key: c.Key, Count: c.Count, P95Ms: c.P95Ms, AvgMs: c.AvgMs, Queries: c.AvgQueries, Memory: c.PeakMemory}
		b, ok := bm[c.Key]
		if !ok {
			d.Status = "new"
			out = append(out, d)
			continue
		}
		d.Status = "changed"
		d.BaseCnt, d.BaseP95Ms, d.BaseAvgMs, d.BaseQ, d.BaseMemory = b.Count, b.P95Ms, b.AvgMs, b.AvgQueries, b.PeakMemory
		d.P95Pct, d.AvgPct = pct(c.P95Ms, b.P95Ms), pct(c.AvgMs, b.AvgMs)
		d.QueriesPct, d.MemoryPct = pct(c.AvgQueries, b.AvgQueries), pct(float64(c.PeakMemory), float64(b.PeakMemory))
		d.Regression = (d.P95Pct >= RegressionPct && c.P95Ms-b.P95Ms >= 5) ||
			(d.QueriesPct >= RegressionPct && c.AvgQueries-b.AvgQueries >= 1) ||
			(d.MemoryPct >= RegressionPct && c.PeakMemory-b.PeakMemory >= 2<<20)
		d.Improvement = !d.Regression && d.P95Pct <= -RegressionPct && b.P95Ms-c.P95Ms >= 5
		out = append(out, d)
	}
	for _, b := range base {
		if !seen[b.Key] {
			out = append(out, RouteDelta{Key: b.Key, Status: "gone", BaseCnt: b.Count, BaseP95Ms: b.P95Ms, BaseAvgMs: b.AvgMs, BaseQ: b.AvgQueries, BaseMemory: b.PeakMemory})
		}
	}
	rank := func(d RouteDelta) int {
		switch {
		case d.Regression:
			return 0
		case d.Status == "new":
			return 1
		case d.Status == "gone":
			return 3
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].P95Pct > out[j].P95Pct
	})
	return out
}
