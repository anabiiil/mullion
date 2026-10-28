package perf

import (
	"sort"
	"strconv"

	"pm/internal/watch"
)

// QuerySummary is a query shape the Watch tab saw misbehave.
type QuerySummary struct {
	Kind     string  `json:"kind"` // "n+1" or "slow"
	SQL      string  `json:"sql"`  // normalized
	Example  string  `json:"example"`
	Bindings []any   `json:"bindings"`
	Conn     string  `json:"conn,omitempty"`
	File     string  `json:"file,omitempty"`
	Line     int     `json:"line,omitempty"`
	Requests int     `json:"requests"` // how many requests/jobs it happened in
	Count    int     `json:"count"`    // queries in all
	MaxMs    float64 `json:"maxMs"`
	TotalMs  float64 `json:"totalMs"`
}

// SummarizeQueries groups the N+1 patterns and slow queries in a Watch
// feed's items by query shape, worst first (at most n of each kind).
func SummarizeQueries(items []watch.Item, slowMs float64, n int) []QuerySummary {
	type key struct{ kind, sql string }
	acc := map[key]*QuerySummary{}
	order := []key{}
	get := func(k key) *QuerySummary {
		if q := acc[k]; q != nil {
			return q
		}
		q := &QuerySummary{Kind: k.kind, SQL: k.sql, Bindings: []any{}}
		acc[k] = q
		order = append(order, k)
		return q
	}
	// The first query of each shape, for an example to explain.
	examples := map[string]watch.Item{}
	for _, it := range items {
		switch it.Type() {
		case "query":
			norm := str(it["norm"])
			if norm == "" {
				norm = watch.NormalizeSQL(str(it["sql"]))
			}
			if _, ok := examples[norm]; !ok {
				examples[norm] = it
			}
			ms := flt(it["ms"])
			if slowMs > 0 && ms >= slowMs {
				q := get(key{"slow", norm})
				q.Count++
				q.Requests++
				q.TotalMs += ms
				if ms > q.MaxMs {
					q.MaxMs = ms
					q.Example, q.Conn = str(it["sql"]), str(it["conn"])
					if b, ok := it["bindings"].([]any); ok {
						q.Bindings = b
					}
					q.File, q.Line = str(it["file"]), num(it["line"])
				}
			}
		case "analysis":
			for _, is := range issuesOf(it) {
				if is.Kind != "n+1" {
					continue
				}
				q := get(key{"n+1", is.SQL})
				q.Requests++
				q.Count += is.Count
				q.TotalMs += is.Ms
				if c := float64(is.Count); c > q.MaxMs {
					q.MaxMs = c // repeats in one request, for N+1
				}
				if q.File == "" {
					q.File, q.Line = is.File, is.Line
				}
			}
		}
	}
	var out []QuerySummary
	for _, k := range order {
		q := acc[k]
		if q.Kind == "n+1" {
			if ex, ok := examples[q.SQL]; ok {
				q.Example, q.Conn = str(ex["sql"]), str(ex["conn"])
				if b, ok := ex["bindings"].([]any); ok {
					q.Bindings = b
				}
			}
		}
		q.TotalMs = round2(q.TotalMs)
		out = append(out, *q)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "n+1"
		}
		return out[i].TotalMs > out[j].TotalMs
	})
	var res []QuerySummary
	cnt := map[string]int{}
	for _, q := range out {
		if cnt[q.Kind] < n {
			cnt[q.Kind]++
			res = append(res, q)
		}
	}
	return res
}

// CheckQueries turns the summaries into readiness findings.
func CheckQueries(qs []QuerySummary) []Finding {
	var out []Finding
	for _, q := range qs {
		where := ""
		if q.File != "" {
			where = q.File + ":" + strconv.Itoa(q.Line)
		}
		f := Finding{Category: "queries", Explain: &ExplainRef{SQL: q.Example, Bindings: q.Bindings, Conn: q.Conn}}
		f.Where = []string{q.SQL}
		if where != "" {
			f.Where = append(f.Where, where)
		}
		if q.Kind == "n+1" {
			f.ID = "queries.n+1:" + shortHash(q.SQL+where)
			f.Severity = SevMedium
			f.Title = "N+1: the same query ran up to " + strconv.Itoa(int(q.MaxMs)) + "× in one request"
			f.Why = "Seen in " + strconv.Itoa(q.Requests) + " request(s). A relation loaded inside a loop runs one query per row; eager-load it with with() (or load() / loadMissing()) so it is one query."
			f.Fix = "Post::with('author')->get();   // instead of $post->author inside the loop"
			f.Explain.NPlusOne = int(q.MaxMs)
		} else {
			f.ID = "queries.slow:" + shortHash(q.SQL)
			f.Severity = SevMedium
			f.Title = "Slow query: up to " + fmtMs(q.MaxMs)
			f.Why = "Seen " + strconv.Itoa(q.Count) + " time(s). Explain it to see whether an index would help."
		}
		out = append(out, f)
	}
	return out
}

// shortHash is a short stable id for a string (FNV-1a, hex).
func shortHash(s string) string {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 36)
}
