package explain

// The plan model every engine's output is turned into: a tree of nodes
// for the panel, each with the few normalized facts the diagnosis
// reads (is it a full scan, which table and index, estimated and actual
// rows, sorts and temporary tables, correlated subqueries).

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Node is one step of a plan.
type Node struct {
	Op     string  `json:"op"`              // as the engine names it: "ALL", "Seq Scan", "SCAN users", "Clustered Index Scan"
	Table  string  `json:"table,omitempty"` // real table name
	Alias  string  `json:"alias,omitempty"`
	Index  string  `json:"index,omitempty"`
	Detail string  `json:"detail,omitempty"` // condition / sort key / engine text
	Rows   float64 `json:"rows"`             // estimated rows (per loop)
	Cost   float64 `json:"cost,omitempty"`
	// Actual* come from Analyze (-1 = not measured). ActualRows is per
	// loop, as the engines report it.
	ActualRows float64 `json:"actualRows"`
	Loops      float64 `json:"loops,omitempty"`
	Removed    float64 `json:"removed,omitempty"` // rows read then thrown away by a filter (PG)
	TimeMs     float64 `json:"timeMs,omitempty"`

	// Normalized facts.
	FullScan  bool     `json:"fullScan,omitempty"`  // reads the whole table (or whole index)
	IndexScan bool     `json:"indexScan,omitempty"` // a full scan of an index (MySQL type=index, MSSQL Index Scan)
	Possible  []string `json:"possibleKeys,omitempty"`
	Filtered  float64  `json:"filtered,omitempty"` // MySQL: % of examined rows kept (0 = unknown)
	Filesort  bool     `json:"filesort,omitempty"`
	Temporary bool     `json:"temporary,omitempty"`
	DiskSort  bool     `json:"diskSort,omitempty"`
	DiskHash  bool     `json:"diskHash,omitempty"`
	Dependent bool     `json:"dependent,omitempty"` // correlated subquery: runs once per outer row
	Lookup    bool     `json:"lookup,omitempty"`    // MSSQL Key/RID Lookup
	AutoIndex bool     `json:"autoIndex,omitempty"` // SQLite built a temporary index (a missing one)
	Extra     []string `json:"extra,omitempty"`

	Children []*Node `json:"children,omitempty"`
}

// MissingIndex is SQL Server's own index suggestion.
type MissingIndex struct {
	Table      string   `json:"table"`
	Impact     float64  `json:"impact"`
	Equality   []string `json:"equality,omitempty"`
	Inequality []string `json:"inequality,omitempty"`
	Include    []string `json:"include,omitempty"`
}

// Warning is a note the server attached to the plan.
type Warning struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Plan is one statement's plan, in Mullion's model plus the raw text.
type Plan struct {
	Engine   string  `json:"engine"`
	Nodes    []*Node `json:"nodes"`
	Analyzed bool    `json:"analyzed,omitempty"`
	// Tabular is MySQL/MariaDB's classic EXPLAIN (one row per table).
	Tabular  []map[string]any `json:"tabular,omitempty"`
	Missing  []MissingIndex   `json:"missingIndexes,omitempty"`
	Warnings []Warning        `json:"warnings,omitempty"`
	Cost     float64          `json:"cost,omitempty"`
	TimeMs   float64          `json:"timeMs,omitempty"` // Analyze: execution time
	Raw      string           `json:"raw"`
	RawKind  string           `json:"rawKind"` // "json", "xml" or "text"
	// RawAnalyze is MySQL's EXPLAIN ANALYZE text.
	RawAnalyze string `json:"rawAnalyze,omitempty"`
}

// Walk calls fn for every node, depth first.
func (p *Plan) Walk(fn func(n *Node, depth int)) {
	var rec func(ns []*Node, d int)
	rec = func(ns []*Node, d int) {
		for _, n := range ns {
			fn(n, d)
			rec(n.Children, d+1)
		}
	}
	rec(p.Nodes, 0)
}

func newNode(op string) *Node { return &Node{Op: op, ActualRows: -1} }

// toFloat reads a JSON number, a numeric string or a bool.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0
		}
		return f
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

func toStr(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case float64:
		if s == math.Trunc(s) {
			return strconv.FormatInt(int64(s), 10)
		}
		return strconv.FormatFloat(s, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// ── MySQL / MariaDB ─────────────────────────────────────────────────

// mysqlTabular turns classic EXPLAIN rows into nodes (one per table).
func mysqlTabular(rows []map[string]any) []*Node {
	var out []*Node
	for _, r := range rows {
		get := func(k string) any {
			for kk, v := range r {
				if strings.EqualFold(kk, k) {
					return v
				}
			}
			return nil
		}
		typ := toStr(get("type"))
		n := newNode(typ)
		n.Table = toStr(get("table"))
		n.Index = toStr(get("key"))
		n.Rows = toFloat(get("rows"))
		n.Filtered = toFloat(get("filtered"))
		if pk := toStr(get("possible_keys")); pk != "" {
			for _, k := range strings.Split(pk, ",") {
				if k = strings.TrimSpace(k); k != "" {
					n.Possible = append(n.Possible, k)
				}
			}
		}
		st := strings.ToUpper(toStr(get("select_type")))
		extra := toStr(get("Extra"))
		for _, e := range strings.Split(extra, ";") {
			if e = strings.TrimSpace(e); e != "" {
				n.Extra = append(n.Extra, e)
			}
		}
		lx := strings.ToLower(extra)
		n.Filesort = strings.Contains(lx, "using filesort")
		n.Temporary = strings.Contains(lx, "using temporary")
		n.Dependent = strings.HasPrefix(st, "DEPENDENT") || strings.HasPrefix(st, "UNCACHEABLE")
		switch strings.ToUpper(typ) {
		case "ALL":
			n.FullScan = true
		case "INDEX":
			// A full index scan: cheap when "Using index" (covering) and
			// ordered, but it still reads every entry. On PRIMARY it is
			// InnoDB's clustered index — the table itself.
			if strings.EqualFold(n.Index, "PRIMARY") {
				n.FullScan = true
			} else {
				n.IndexScan = true
			}
		}
		var parts []string
		if id := toStr(get("id")); id != "" {
			parts = append(parts, "id "+id)
		}
		if st != "" && st != "SIMPLE" {
			parts = append(parts, st)
		}
		if ref := toStr(get("ref")); ref != "" {
			parts = append(parts, "ref "+ref)
		}
		n.Detail = strings.Join(parts, " · ")
		// Derived/union rows (<derived2>) aren't real tables.
		if strings.HasPrefix(n.Table, "<") {
			n.Table = ""
		}
		out = append(out, n)
	}
	return out
}

// mysqlJSONCost reads query_block.cost_info.query_cost (MySQL) — MariaDB
// has no total cost.
func mysqlJSONCost(raw string) float64 {
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return 0
	}
	if qb, ok := v["query_block"].(map[string]any); ok {
		if ci, ok := qb["cost_info"].(map[string]any); ok {
			return toFloat(ci["query_cost"])
		}
	}
	return 0
}

// mariaAnalyzeJSON walks MariaDB's ANALYZE FORMAT=JSON (and EXPLAIN
// FORMAT=JSON, of either server): every "table" object is a node; sort
// and temporary-table wrappers mark the tables under them.
func mariaAnalyzeJSON(raw string) ([]*Node, float64) {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return nil, 0
	}
	var out []*Node
	total := 0.0
	var rec func(x any, filesort, temp, dep bool)
	rec = func(x any, filesort, temp, dep bool) {
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				rec(e, filesort, temp, dep)
			}
		case map[string]any:
			if qb, ok := t["query_block"].(map[string]any); ok && total == 0 {
				total = toFloat(qb["r_total_time_ms"])
			}
			for k, e := range t {
				switch k {
				case "table":
					tb, ok := e.(map[string]any)
					if !ok {
						continue
					}
					n := newNode(toStr(tb["access_type"]))
					n.Table = toStr(tb["table_name"])
					n.Index = toStr(tb["key"])
					n.Rows = toFloat(tb["rows"])
					if n.Rows == 0 {
						n.Rows = toFloat(tb["rows_examined_per_scan"])
					}
					n.Filtered = toFloat(tb["filtered"])
					if r, ok := tb["r_rows"]; ok {
						n.ActualRows = toFloat(r)
						n.Loops = toFloat(tb["r_loops"])
						if f, ok := tb["r_filtered"]; ok {
							n.Filtered = toFloat(f)
						}
						n.TimeMs = toFloat(tb["r_total_time_ms"])
					}
					if pk, ok := tb["possible_keys"].([]any); ok {
						for _, k := range pk {
							n.Possible = append(n.Possible, toStr(k))
						}
					}
					n.Detail = toStr(tb["attached_condition"])
					n.FullScan = strings.EqualFold(n.Op, "ALL")
					n.IndexScan = strings.EqualFold(n.Op, "index")
					n.Filesort, n.Temporary, n.Dependent = filesort, temp, dep
					if strings.HasPrefix(n.Table, "<") {
						n.Table = ""
					}
					out = append(out, n)
					rec(tb, false, false, dep)
				case "filesort", "read_sorted_file":
					rec(e, true, temp, dep)
				case "temporary_table":
					rec(e, filesort, true, dep)
				case "ordering_operation", "grouping_operation", "duplicates_removal":
					m, _ := e.(map[string]any)
					fs := filesort || m != nil && m["using_filesort"] == true
					tp := temp || m != nil && m["using_temporary_table"] == true
					rec(e, fs, tp, dep)
				case "attached_subqueries", "subqueries":
					list, _ := e.([]any)
					for _, s := range list {
						m, _ := s.(map[string]any)
						d := dep || m != nil && (m["dependent"] == true || m["expression_cache"] != nil)
						rec(s, false, false, d)
					}
				default:
					rec(e, filesort, temp, dep)
				}
			}
		}
	}
	rec(v, false, false, false)
	return out, total
}

// mysqlAnalyzeTree parses MySQL's EXPLAIN ANALYZE text tree:
//
//	-> Filter: (users.email = 'a')  (cost=101 rows=100) (actual time=0.1..2.3 rows=1 loops=1)
//	    -> Table scan on users  (cost=101 rows=1000) (actual time=0.05..1.9 rows=1000 loops=1)
func mysqlAnalyzeTree(text string) []*Node {
	type lvl struct {
		n      *Node
		indent int
	}
	var roots []*Node
	var stack []lvl
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, "-> ")
		if i < 0 {
			continue
		}
		indent := i
		body := line[i+3:]
		n := newNode(body)
		if j := strings.Index(body, "  ("); j >= 0 {
			n.Op = body[:j]
			meta := body[j:]
			if m := reCost.FindStringSubmatch(meta); m != nil {
				n.Cost = toFloat(m[1])
				n.Rows = toFloat(m[2])
			}
			if m := reActual.FindStringSubmatch(meta); m != nil {
				n.TimeMs = toFloat(m[2])
				n.ActualRows = toFloat(m[3])
				n.Loops = toFloat(m[4])
			}
		}
		lop := strings.ToLower(n.Op)
		switch {
		case strings.HasPrefix(lop, "table scan on "):
			n.FullScan = true
			n.Table = firstWord(n.Op[len("table scan on "):])
		case strings.HasPrefix(lop, "index scan on "):
			n.IndexScan = true
			rest := n.Op[len("index scan on "):]
			n.Table = firstWord(rest)
			if k := strings.Index(rest, " using "); k >= 0 {
				n.Index = firstWord(rest[k+7:])
			}
		case strings.Contains(lop, "index lookup on ") || strings.Contains(lop, "index range scan on ") || strings.Contains(lop, "single-row index lookup on ") || strings.Contains(lop, "covering index lookup on "):
			k := strings.Index(lop, " on ")
			rest := n.Op[k+4:]
			n.Table = firstWord(rest)
			if u := strings.Index(rest, " using "); u >= 0 {
				n.Index = firstWord(rest[u+7:])
			}
		case strings.HasPrefix(lop, "sort"):
			n.Filesort = true
		case strings.HasPrefix(lop, "materialize") || strings.HasPrefix(lop, "temporary table"):
			n.Temporary = true
		}
		if strings.Contains(lop, "dependent") {
			n.Dependent = true
		}
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, n)
		} else {
			p := stack[len(stack)-1].n
			p.Children = append(p.Children, n)
		}
		stack = append(stack, lvl{n, indent})
	}
	return roots
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " ("); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, "`\"")
}

// ── PostgreSQL ──────────────────────────────────────────────────────

// pgPlan parses EXPLAIN (FORMAT JSON) [ANALYZE] output.
func pgPlan(raw string) ([]*Node, float64, float64, error) {
	var doc []map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
		return nil, 0, 0, fmt.Errorf("unreadable PostgreSQL plan")
	}
	root, _ := doc[0]["Plan"].(map[string]any)
	if root == nil {
		return nil, 0, 0, fmt.Errorf("unreadable PostgreSQL plan")
	}
	var rec func(m map[string]any, dep bool) *Node
	rec = func(m map[string]any, dep bool) *Node {
		typ := toStr(m["Node Type"])
		n := newNode(typ)
		n.Table = toStr(m["Relation Name"])
		n.Alias = toStr(m["Alias"])
		if n.Alias == n.Table {
			n.Alias = ""
		}
		n.Index = toStr(m["Index Name"])
		n.Rows = toFloat(m["Plan Rows"])
		n.Cost = toFloat(m["Total Cost"])
		if v, ok := m["Actual Rows"]; ok {
			n.ActualRows = toFloat(v)
			n.Loops = toFloat(m["Actual Loops"])
			n.TimeMs = toFloat(m["Actual Total Time"])
		}
		n.Removed = toFloat(m["Rows Removed by Filter"]) + toFloat(m["Rows Removed by Index Recheck"])
		var det []string
		for _, k := range []string{"Index Cond", "Recheck Cond", "Filter", "Join Filter", "Hash Cond", "Merge Cond"} {
			if v := toStr(m[k]); v != "" {
				det = append(det, k+": "+v)
			}
		}
		if sk, ok := m["Sort Key"].([]any); ok {
			var keys []string
			for _, k := range sk {
				keys = append(keys, toStr(k))
			}
			det = append(det, "Sort Key: "+strings.Join(keys, ", "))
		}
		if v := toStr(m["Sort Method"]); v != "" {
			det = append(det, "Sort Method: "+v)
			n.Extra = append(n.Extra, "Sort Method: "+v)
			if strings.Contains(strings.ToLower(v), "external") {
				n.DiskSort = true
			}
		}
		if strings.EqualFold(toStr(m["Sort Space Type"]), "Disk") {
			n.DiskSort = true
		}
		if toFloat(m["Hash Batches"]) > 1 || toFloat(m["Disk Usage"]) > 0 {
			n.DiskHash = true
			n.Extra = append(n.Extra, fmt.Sprintf("Hash Batches: %s", toStr(m["Hash Batches"])))
		}
		n.Detail = strings.Join(det, " · ")
		switch typ {
		case "Seq Scan", "Parallel Seq Scan":
			n.FullScan = true
		case "Index Scan", "Index Only Scan":
			// No Index Cond: the whole index is walked (for its order).
			if m["Index Cond"] == nil {
				n.IndexScan = true
			}
		case "Sort", "Incremental Sort":
			n.Filesort = true
		}
		rel := toStr(m["Parent Relationship"])
		if rel == "SubPlan" {
			dep = true
		}
		n.Dependent = dep && rel == "SubPlan"
		if kids, ok := m["Plans"].([]any); ok {
			for _, k := range kids {
				if km, ok := k.(map[string]any); ok {
					n.Children = append(n.Children, rec(km, dep))
				}
			}
		}
		return n
	}
	n := rec(root, false)
	return []*Node{n}, toFloat(root["Total Cost"]), toFloat(doc[0]["Execution Time"]), nil
}

// ── SQLite ──────────────────────────────────────────────────────────

// sqlitePlan turns EXPLAIN QUERY PLAN rows (id, parent, detail) into a
// tree.
func sqlitePlan(rows []map[string]any) []*Node {
	byID := map[int]*Node{}
	var roots []*Node
	corr := map[int]bool{}
	for _, r := range rows {
		id := int(toFloat(r["id"]))
		parent := int(toFloat(r["parent"]))
		detail := toStr(r["detail"])
		n := newNode(detail)
		d := strings.ToUpper(detail)
		words := strings.Fields(detail)
		switch {
		case strings.HasPrefix(d, "SCAN "):
			name := words[1]
			if strings.EqualFold(name, "TABLE") && len(words) > 2 { // SQLite < 3.36: "SCAN TABLE users"
				name = words[2]
			}
			n.Table = name
			if strings.Contains(d, " USING COVERING INDEX ") || strings.Contains(d, " USING INDEX ") {
				n.IndexScan = true
				n.Index = after(detail, "INDEX ")
			} else if !strings.HasPrefix(d, "SCAN SUBQUERY") && !strings.HasPrefix(d, "SCAN CONSTANT") {
				n.FullScan = true
			}
		case strings.HasPrefix(d, "SEARCH "):
			name := words[1]
			if strings.EqualFold(name, "TABLE") && len(words) > 2 {
				name = words[2]
			}
			n.Table = name
			if strings.Contains(d, "AUTOMATIC") {
				n.AutoIndex = true
			} else if strings.Contains(d, "PRIMARY KEY") {
				n.Index = "PRIMARY"
			} else {
				n.Index = after(detail, "INDEX ")
			}
		case strings.HasPrefix(d, "USE TEMP B-TREE FOR ORDER BY") || strings.HasPrefix(d, "USE TEMP B-TREE FOR LAST"):
			n.Filesort = true
		case strings.HasPrefix(d, "USE TEMP B-TREE"):
			n.Temporary = true
		case strings.HasPrefix(d, "CORRELATED"):
			n.Dependent = true
			corr[id] = true
		}
		if strings.HasPrefix(n.Table, "(") {
			n.Table = ""
		}
		if corr[parent] {
			n.Dependent = true
			corr[id] = true
		}
		byID[id] = n
		if p := byID[parent]; p != nil && parent != 0 {
			p.Children = append(p.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	return roots
}

// after returns the word following marker in s.
func after(s, marker string) string {
	i := strings.Index(strings.ToUpper(s), strings.ToUpper(marker))
	if i < 0 {
		return ""
	}
	return firstWord(s[i+len(marker):])
}
