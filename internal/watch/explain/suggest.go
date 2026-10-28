package explain

// Index suggestions. The heuristic, per table (the usual rule for
// B-tree indexes — MySQL, MariaDB, PostgreSQL, SQLite, SQL Server):
//
//  1. the columns the WHERE (and JOIN … ON) compares with = / IN / IS
//     NULL, in the order they appear — any order works for equality, so
//     they go first;
//  2. then one range column (<, >, BETWEEN, LIKE 'abc%') — an index
//     can't be used past a range;
//  3. then the ORDER BY (or GROUP BY) columns when no range column was
//     added and they all belong to this table, so rows come out of the
//     index already sorted (no filesort).
//
// Columns wrapped in a function, compared inside an OR, or matched with
// a leading wildcard can't use a plain index and are left out. No
// suggestion is made when an existing index already starts with the
// same columns (equality columns in any order) — then the problem is
// the planner's choice (selectivity, stale statistics), not a missing
// index. For SQL Server the server's own missing-index hint wins.

import (
	"fmt"
	"hash/crc32"
	"regexp"
	"strings"
)

// Suggestion is an index to add.
type Suggestion struct {
	Table     string   `json:"table"`
	Columns   []string `json:"columns"`
	Include   []string `json:"include,omitempty"`
	Name      string   `json:"name"`
	SQL       string   `json:"sql"`
	Migration string   `json:"migration"`
	Reason    string   `json:"reason"`
	Source    string   `json:"source"` // "heuristic" or "server"
	Impact    float64  `json:"impact,omitempty"`
	// Extends names an existing index this one would make redundant.
	Extends string `json:"extends,omitempty"`
}

// purpose says which clauses an index should serve.
type purpose struct{ filter, order, group bool }

// IndexColumns applies the heuristic above to one table.
func IndexColumns(table string, info SQLInfo, why purpose) []string {
	var cols []string
	seen := map[string]bool{}
	add := func(c string) {
		k := strings.ToLower(c)
		if c != "" && !seen[k] && len(cols) < 5 {
			seen[k] = true
			cols = append(cols, c)
		}
	}
	mine := func(t string) bool {
		return strings.EqualFold(t, table) || strings.EqualFold(lastPart(t), lastPart(table))
	}
	rng := ""
	if why.filter {
		for _, p := range info.Where {
			if !mine(p.Table) || p.Func != "" || p.InOr {
				continue
			}
			switch p.Op {
			case "eq":
				add(p.Col)
			case "range":
				if rng == "" {
					rng = p.Col
				}
			}
		}
		// Join columns matter on the table being looked up, not on the
		// one driving the join (the main table, when it has filters).
		if !isMain(table, info) || len(cols) == 0 && rng == "" {
			for _, p := range info.Joins {
				if mine(p.Table) && p.Func == "" {
					add(p.Col)
				}
			}
		}
		if rng != "" && !seen[strings.ToLower(rng)] {
			add(rng)
		} else {
			rng = ""
		}
	}
	if rng == "" && !info.Random {
		list := info.OrderBy
		if why.group && len(info.GroupBy) > 0 {
			list = info.GroupBy
		}
		if (why.order || why.group) && len(list) > 0 {
			all := true
			for _, o := range list {
				if !mine(o.Table) {
					all = false
				}
			}
			if all {
				for _, o := range list {
					add(o.Col)
				}
			}
		}
	}
	return cols
}

func isMain(table string, info SQLInfo) bool {
	for _, t := range info.Tables {
		if t.Main {
			return strings.EqualFold(lastPart(t.Name), lastPart(table))
		}
	}
	return false
}

// coveredBy returns the existing index that already starts with cols
// (equality columns in any order: the first nEq index columns must be
// the same set), or "".
func coveredBy(cols []string, nEq int, idx []Index) string {
	for _, ix := range idx {
		if len(ix.Columns) < len(cols) {
			continue
		}
		ok := true
		set := map[string]bool{}
		for _, c := range cols[:nEq] {
			set[strings.ToLower(c)] = true
		}
		for i, c := range cols {
			ic := strings.ToLower(ix.Columns[i])
			if i < nEq {
				if !set[ic] {
					ok = false
					break
				}
				continue
			}
			if ic != strings.ToLower(c) {
				ok = false
				break
			}
		}
		if ok {
			return ix.Name
		}
	}
	return ""
}

// extends returns an existing index whose columns are a prefix of cols
// (the new one makes it redundant), or "".
func extends(cols []string, idx []Index) string {
	for _, ix := range idx {
		if ix.Primary || ix.Unique || len(ix.Columns) >= len(cols) || len(ix.Columns) == 0 {
			continue
		}
		ok := true
		for i, c := range ix.Columns {
			if !strings.EqualFold(c, cols[i]) {
				ok = false
				break
			}
		}
		if ok {
			return ix.Name
		}
	}
	return ""
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// IndexName is idx_<table>_<columns>, shortened (with a checksum) to
// stay under every engine's identifier limit.
func IndexName(table string, cols []string) string {
	name := "idx_" + strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(lastPart(table)+"_"+strings.Join(cols, "_")), "_"), "_")
	if len(name) > 60 {
		name = fmt.Sprintf("%s_%08x", name[:51], crc32.ChecksumIEEE([]byte(name)))
	}
	return name
}

var simpleIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// reserved are column names people use that need quoting.
var reserved = map[string]bool{"order": true, "group": true, "key": true, "index": true, "user": true, "table": true,
	"select": true, "from": true, "where": true, "type": false, "desc": true, "range": true, "value": false, "rank": true, "check": true}

func quoteIdent(engine, name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		if simpleIdent.MatchString(p) && !reserved[p] {
			continue
		}
		switch engine {
		case "mysql", "mariadb":
			parts[i] = "`" + strings.ReplaceAll(p, "`", "``") + "`"
		case "sqlsrv":
			parts[i] = "[" + strings.ReplaceAll(p, "]", "]]") + "]"
		default:
			parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
		}
	}
	return strings.Join(parts, ".")
}

func phpList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = "'" + strings.ReplaceAll(c, "'", `\'`) + "'"
	}
	if len(q) == 1 {
		return q[0]
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// newSuggestion builds the SQL and migration for an index.
func newSuggestion(engine, table string, cols, include []string, reason, source string) Suggestion {
	s := Suggestion{Table: table, Columns: cols, Include: include, Name: IndexName(table, cols), Reason: reason, Source: source}
	qc := make([]string, len(cols))
	for i, c := range cols {
		qc[i] = quoteIdent(engine, c)
	}
	s.SQL = fmt.Sprintf("CREATE INDEX %s ON %s (%s)", s.Name, quoteIdent(engine, table), strings.Join(qc, ", "))
	if len(include) > 0 && (engine == "sqlsrv" || engine == "pgsql") {
		qi := make([]string, len(include))
		for i, c := range include {
			qi[i] = quoteIdent(engine, c)
		}
		s.SQL += " INCLUDE (" + strings.Join(qi, ", ") + ")"
	}
	s.SQL += ";"
	body := "    $table->index(" + phpList(cols) + ");\n"
	if len(include) > 0 {
		body += "    // The server would also INCLUDE (" + strings.Join(include, ", ") + ") to make it covering —\n    // Blueprint can't; use DB::statement() with the SQL above for that.\n"
	}
	s.Migration = "Schema::table('" + table + "', function (Blueprint $table) {\n" + body + "});"
	return s
}

// suggestFor adds (or explains away) an index for table. It returns
// the suggestion's position in res.Suggestions (-1 = none) and the
// name of an existing index that already covers it.
func suggestFor(res *Result, table string, info SQLInfo, why purpose, reason string) (int, string) {
	for i, s := range res.Suggestions {
		if strings.EqualFold(s.Table, table) {
			return i, ""
		}
	}
	// SQL Server's own hint first.
	for _, m := range res.Plan.Missing {
		if strings.EqualFold(lastPart(m.Table), lastPart(table)) {
			cols := append(append([]string{}, m.Equality...), m.Inequality...)
			s := newSuggestion(res.Engine, table, cols, m.Include, fmt.Sprintf("SQL Server's optimizer suggests this index (estimated %.0f%% cheaper).", m.Impact), "server")
			s.Impact = m.Impact
			res.Suggestions = append(res.Suggestions, s)
			return len(res.Suggestions) - 1, ""
		}
	}
	cols := IndexColumns(table, info, why)
	if len(cols) == 0 {
		return -1, ""
	}
	nEq := 0
	for _, c := range cols {
		if isEqCol(table, c, info) {
			nEq++
		} else {
			break
		}
	}
	var idx []Index
	if t := tableInfo(res, table); t != nil {
		idx = t.Indexes
	}
	if by := coveredBy(cols, nEq, idx); by != "" {
		return -1, by
	}
	s := newSuggestion(res.Engine, table, cols, nil, describe(table, cols, nEq, info, reason), "heuristic")
	s.Extends = extends(cols, idx)
	res.Suggestions = append(res.Suggestions, s)
	return len(res.Suggestions) - 1, ""
}

// describe says what each column of a suggested index is for.
func describe(table string, cols []string, nEq int, info SQLInfo, fallback string) string {
	var parts []string
	if nEq > 0 {
		eq := "finds the rows by " + strings.Join(cols[:nEq], ", ")
		if nEq < len(cols) {
			eq += " (equality columns first)"
		}
		parts = append(parts, eq)
	}
	var rng, ord []string
	for _, c := range cols[nEq:] {
		isRange := false
		for _, p := range info.Where {
			if strings.EqualFold(p.Col, c) && p.Op == "range" && strings.EqualFold(lastPart(p.Table), lastPart(table)) {
				isRange = true
			}
		}
		if isRange {
			rng = append(rng, c)
		} else {
			ord = append(ord, c)
		}
	}
	if len(rng) > 0 {
		parts = append(parts, "then narrows by the range on "+strings.Join(rng, ", "))
	}
	if len(ord) > 0 {
		parts = append(parts, "and returns them already in "+strings.Join(ord, ", ")+" order, so there's no sort")
	}
	if len(parts) == 0 {
		return fallback
	}
	out := strings.Join(parts, ", ")
	return strings.ToUpper(out[:1]) + out[1:] + "."
}

func isEqCol(table, col string, info SQLInfo) bool {
	for _, p := range info.Where {
		if strings.EqualFold(p.Col, col) && p.Op == "eq" && strings.EqualFold(lastPart(p.Table), lastPart(table)) {
			return true
		}
	}
	for _, p := range info.Joins {
		if strings.EqualFold(p.Col, col) && strings.EqualFold(lastPart(p.Table), lastPart(table)) {
			return true
		}
	}
	return false
}

// tableInfo finds a table's metadata (names may differ in case or
// schema qualification).
func tableInfo(res *Result, table string) *Table {
	if t, ok := res.Tables[table]; ok {
		return t
	}
	for k, t := range res.Tables {
		if strings.EqualFold(k, table) || strings.EqualFold(lastPart(k), lastPart(table)) {
			return t
		}
	}
	return nil
}
