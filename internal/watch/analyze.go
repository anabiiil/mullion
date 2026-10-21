package watch

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	sqlString = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'`)
	sqlNumber = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	sqlInList = regexp.MustCompile(`(?i)\bin\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	sqlSpace  = regexp.MustCompile(`\s+`)
)

// NormalizeSQL reduces a statement to its shape: literals become "?",
// IN lists collapse to "in (?)" and whitespace is squeezed — so the
// same query with other values (the N+1 pattern) compares equal.
func NormalizeSQL(sql string) string {
	s := sqlString.ReplaceAllString(sql, "?")
	s = sqlNumber.ReplaceAllString(s, "?")
	s = sqlInList.ReplaceAllString(s, "in (?)")
	s = sqlSpace.ReplaceAllString(strings.TrimSpace(s), " ")
	return strings.ToLower(s)
}

// Query is what the analysis needs from one "query" item.
type Query struct {
	SQL      string
	Bindings []any
	Ms       float64
	File     string
	Line     int
}

// QueryIssue is one finding within a request.
type QueryIssue struct {
	Kind  string  `json:"kind"` // "n+1" or "duplicate"
	SQL   string  `json:"sql"`  // normalized (n+1) or as run (duplicate)
	File  string  `json:"file,omitempty"`
	Line  int     `json:"line,omitempty"`
	Count int     `json:"count"`
	Ms    float64 `json:"ms"`
}

// Analysis summarises one request's queries.
type Analysis struct {
	Queries    int          `json:"queries"`
	Ms         float64      `json:"ms"`
	Slow       int          `json:"slow"`
	Issues     []QueryIssue `json:"issues"`
	Duplicates int          `json:"duplicates"`
}

// Analyze finds N+1 patterns (the same normalized statement run at
// least nPlusOne times from the same file:line), exact duplicates (same
// SQL and bindings, more than once) and slow queries (≥ slowMs).
func Analyze(qs []Query, nPlusOne int, slowMs float64) Analysis {
	if nPlusOne < 2 {
		nPlusOne = 2
	}
	a := Analysis{Queries: len(qs), Issues: []QueryIssue{}}
	type agg struct {
		issue QueryIssue
		order int
	}
	n1 := map[string]*agg{}
	dup := map[string]*agg{}
	for i, q := range qs {
		a.Ms += q.Ms
		if slowMs > 0 && q.Ms >= slowMs {
			a.Slow++
		}
		norm := NormalizeSQL(q.SQL)
		k := norm + "\x00" + q.File + ":" + strconv.Itoa(q.Line)
		if g := n1[k]; g != nil {
			g.issue.Count++
			g.issue.Ms += q.Ms
		} else {
			n1[k] = &agg{QueryIssue{Kind: "n+1", SQL: norm, File: q.File, Line: q.Line, Count: 1, Ms: q.Ms}, i}
		}
		b, _ := json.Marshal(q.Bindings)
		dk := q.SQL + "\x00" + string(b)
		if g := dup[dk]; g != nil {
			g.issue.Count++
			g.issue.Ms += q.Ms
		} else {
			dup[dk] = &agg{QueryIssue{Kind: "duplicate", SQL: q.SQL, File: q.File, Line: q.Line, Count: 1, Ms: q.Ms}, i}
		}
	}
	var found []*agg
	for _, g := range n1 {
		if g.issue.Count >= nPlusOne {
			found = append(found, g)
		}
	}
	for _, g := range dup {
		if g.issue.Count > 1 {
			a.Duplicates += g.issue.Count - 1
			found = append(found, g)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].issue.Kind != found[j].issue.Kind {
			return found[i].issue.Kind == "n+1"
		}
		return found[i].order < found[j].order
	})
	for _, g := range found {
		g.issue.Ms = round2(g.issue.Ms)
		a.Issues = append(a.Issues, g.issue)
	}
	a.Ms = round2(a.Ms)
	return a
}
