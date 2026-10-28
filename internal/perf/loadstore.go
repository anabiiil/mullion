package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pm/internal/loadtest"
	"pm/internal/pmdir"
)

// LoadResult is one saved load test.
type LoadResult struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	URL         string    `json:"url"`
	Method      string    `json:"method"`
	Concurrency int       `json:"concurrency"`
	DurationS   int       `json:"durationS,omitempty"`
	Requests    int       `json:"requestsWanted,omitempty"`
	WarmUpS     int       `json:"warmUpS,omitempty"`
	// Mode is how the site was served: "dev" or "prod" (production-like).
	Mode   string            `json:"mode"`
	Result loadtest.Snapshot `json:"result"`
}

// maxLoadResults is how many results a site keeps.
const maxLoadResults = 50

func loadDir(p pmdir.Paths, site string) string { return filepath.Join(Dir(p, site), "loadtests") }

// SaveLoadResult stores a finished test (the oldest beyond
// maxLoadResults are dropped).
func SaveLoadResult(p pmdir.Paths, site string, r LoadResult) (LoadResult, error) {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	if r.ID == "" {
		r.ID = r.At.Format("20060102-150405.000")
		r.ID = strings.ReplaceAll(r.ID, ".", "-")
	}
	if err := writeJSON(filepath.Join(loadDir(p, site), r.ID+".json"), r); err != nil {
		return r, err
	}
	list := listLoadFiles(p, site)
	for len(list) > maxLoadResults {
		_ = os.Remove(list[0])
		list = list[1:]
	}
	return r, nil
}

// listLoadFiles is the result files, oldest first (ids sort by time).
func listLoadFiles(p pmdir.Paths, site string) []string {
	m, _ := filepath.Glob(filepath.Join(loadDir(p, site), "*.json"))
	sort.Strings(m)
	return m
}

// LoadResults lists a site's results, newest first. Timelines and
// histograms are dropped unless full.
func LoadResults(p pmdir.Paths, site string, full bool) []LoadResult {
	files := listLoadFiles(p, site)
	out := []LoadResult{}
	for i := len(files) - 1; i >= 0; i-- {
		data, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		var r LoadResult
		if json.Unmarshal(data, &r) != nil || r.ID == "" {
			continue
		}
		if !full {
			r.Result.Timeline, r.Result.Histogram = nil, nil
		}
		out = append(out, r)
	}
	return out
}

// LoadResultByID reads one saved result.
func LoadResultByID(p pmdir.Paths, site, id string) (LoadResult, error) {
	var r LoadResult
	if !validLoadID(id) {
		return r, fmt.Errorf("no load test %q", id)
	}
	data, err := os.ReadFile(filepath.Join(loadDir(p, site), id+".json"))
	if err != nil {
		return r, fmt.Errorf("no load test %q", id)
	}
	return r, json.Unmarshal(data, &r)
}

// DeleteLoadResult removes one saved result.
func DeleteLoadResult(p pmdir.Paths, site, id string) error {
	if !validLoadID(id) {
		return fmt.Errorf("no load test %q", id)
	}
	return os.Remove(filepath.Join(loadDir(p, site), id+".json"))
}

// LoadCompare is a result next to the one before it on the same URL.
type LoadCompare struct {
	RPSPct float64 `json:"rpsPct"`
	P50Pct float64 `json:"p50Pct"`
	P95Pct float64 `json:"p95Pct"`
	P99Pct float64 `json:"p99Pct"`
	// Against is the earlier result's id and mode.
	Against     string `json:"against"`
	AgainstMode string `json:"againstMode"`
}

// CompareLoad compares cur with base (positive RPS = faster, positive
// latency = slower).
func CompareLoad(cur, base LoadResult) LoadCompare {
	return LoadCompare{
		RPSPct:      pct(cur.Result.RPS, base.Result.RPS),
		P50Pct:      pct(cur.Result.P50Ms, base.Result.P50Ms),
		P95Pct:      pct(cur.Result.P95Ms, base.Result.P95Ms),
		P99Pct:      pct(cur.Result.P99Ms, base.Result.P99Ms),
		Against:     base.ID,
		AgainstMode: base.Mode,
	}
}

// Previous is the newest result before cur on the same method and URL
// (ok=false when there is none). results are newest first.
func Previous(results []LoadResult, cur LoadResult) (LoadResult, bool) {
	for _, r := range results {
		if r.ID != cur.ID && r.At.Before(cur.At) && r.URL == cur.URL && r.Method == cur.Method {
			return r, true
		}
	}
	return LoadResult{}, false
}

func validLoadID(id string) bool {
	return id != "" && filepath.Base(id) == id && !strings.ContainsAny(id, `/\.`)
}
