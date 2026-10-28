package explain

// The diagnosis: plain-language findings from a plan plus the SQL
// itself, each with a severity, the why, and a concrete fix (and an
// index suggestion where one helps — see suggest.go).

import (
	"fmt"
	"sort"
	"strings"
)

// Finding is one problem with a query.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"` // high, medium, low, info
	Title    string `json:"title"`
	Why      string `json:"why"`
	Fix      string `json:"fix"`
	Table    string `json:"table,omitempty"`
	Badge    string `json:"badge,omitempty"` // short label for the query row
	Code     string `json:"code,omitempty"`  // a snippet for the fix (copyable)
	// Suggest is the index suggestion (position in Result.Suggestions)
	// that fixes it; -1 = none.
	Suggest int `json:"suggest"`
}

// Table sizes the rules care about.
const (
	SmallTable = 100    // below this a scan is just how small tables are read
	BigTable   = 1000   // a scan or sort here starts to cost
	HugeTable  = 100000 // and here it hurts
)

var sevRank = map[string]int{"high": 0, "medium": 1, "low": 2, "info": 3}

func sizeSeverity(rows float64) string {
	switch {
	case rows >= HugeTable:
		return "high"
	case rows >= BigTable:
		return "medium"
	}
	return "low"
}

func fmtRows(n float64) string {
	switch {
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.0fk", n/1e3)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", n/1e3)
	}
	return fmt.Sprintf("%.0f", n)
}

func tick(s string) string { return "`" + s + "`" }

// Diagnose fills res.Findings, res.Suggestions and res.Badges.
func Diagnose(res *Result, info SQLInfo, in Input) {
	d := &diag{res: res, info: info, done: map[string]bool{}}
	res.Plan.Walk(func(n *Node, _ int) {
		d.sorts = d.sorts || n.Filesort
		d.temps = d.temps || n.Temporary
	})
	d.planRules()
	d.sqlRules(in)
	d.nPlusOne(in)
	sort.SliceStable(res.Findings, func(i, j int) bool {
		return sevRank[res.Findings[i].Severity] < sevRank[res.Findings[j].Severity]
	})
	seen := map[string]bool{}
	for _, f := range res.Findings {
		if f.Badge != "" && f.Severity != "info" && f.Severity != "low" && !seen[f.Badge] && len(res.Badges) < 3 {
			seen[f.Badge] = true
			res.Badges = append(res.Badges, f.Badge)
		}
	}
	for _, t := range res.Tables {
		if t != nil && t.Error != "" {
			res.Notes = append(res.Notes, "Couldn't read a table's indexes: "+t.Error)
		}
	}
}

type diag struct {
	res  *Result
	info SQLInfo
	done map[string]bool // rule + table already reported
	// The plan sorts / builds a temporary table somewhere: an index
	// suggestion then also covers ORDER BY / GROUP BY.
	sorts, temps bool
}

func (d *diag) has(rule string) bool {
	for _, f := range d.res.Findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func (d *diag) add(f Finding) {
	k := f.Rule + "\x00" + strings.ToLower(f.Table)
	if d.done[k] {
		return
	}
	d.done[k] = true
	d.res.Findings = append(d.res.Findings, f)
}

// tableRows is the best row count known for a table: the catalog's,
// else the plan's estimate.
func (d *diag) tableRows(table string, n *Node) float64 {
	if t := tableInfo(d.res, table); t != nil && t.Rows != nil && *t.Rows > 0 {
		return *t.Rows
	}
	if n == nil {
		return 0
	}
	r := n.Rows
	if n.ActualRows >= 0 {
		r = maxf(r, n.ActualRows*maxf(n.Loops, 1)+n.Removed)
	}
	return r
}

func (d *diag) predsOn(table string) []Pred {
	var out []Pred
	for _, p := range d.info.Where {
		if strings.EqualFold(lastPart(p.Table), lastPart(table)) {
			out = append(out, p)
		}
	}
	return out
}

func (d *diag) joinsOn(table string) []Pred {
	var out []Pred
	for _, p := range d.info.Joins {
		if strings.EqualFold(lastPart(p.Table), lastPart(table)) {
			out = append(out, p)
		}
	}
	return out
}

func colNames(ps []Pred) string {
	var out []string
	seen := map[string]bool{}
	for _, p := range ps {
		if !seen[p.Col] {
			seen[p.Col] = true
			out = append(out, tick(p.Col))
		}
	}
	return strings.Join(out, ", ")
}

// indexFix is the fix text pointing at a suggestion (or explaining why
// there's none).
func (d *diag) indexFix(table string, why purpose, reason string) (string, int) {
	i, covered := d.suggestFor(table, why, reason)
	if i >= 0 {
		s := d.res.Suggestions[i]
		txt := "Add an index on " + tick(s.Table) + " (" + strings.Join(s.Columns, ", ") + ") — the SQL and a migration are below."
		if s.Extends != "" {
			txt += " It also covers what " + tick(s.Extends) + " does, so that one can go."
		}
		return txt, i
	}
	if covered != "" {
		an := "ANALYZE TABLE " + table
		if d.res.Engine == "pgsql" || d.res.Engine == "sqlite" {
			an = "ANALYZE " + table
		} else if d.res.Engine == "sqlsrv" {
			an = "UPDATE STATISTICS " + table
		}
		return "The index " + tick(covered) + " already has these columns, yet the planner didn't use it — usually because the value matches a large share of the rows (low selectivity) or the statistics are stale. Refresh them with " + tick(an) + "; if the plan doesn't change, the filter just isn't selective enough to be worth an index.", -1
	}
	return "", -1
}

func (d *diag) suggestFor(table string, why purpose, reason string) (int, string) {
	why.order = why.order || d.sorts
	why.group = why.group || d.temps
	return suggestFor(d.res, table, d.info, why, reason)
}

// ── rules on the plan ──

func (d *diag) planRules() {
	res := d.res
	res.Plan.Walk(func(n *Node, _ int) {
		switch {
		case n.FullScan && n.Table != "":
			d.fullScan(n)
		case n.IndexScan && n.Table != "":
			rows := d.tableRows(n.Table, n)
			if rows >= BigTable && len(d.predsOn(n.Table)) > 0 && !n.Filesort {
				fix, si := d.indexFix(n.Table, purpose{filter: true}, "Lets the WHERE on "+n.Table+" seek straight to the matching rows.")
				if fix == "" {
					fix = "Index the columns the WHERE filters on, so the database can seek instead of scanning the index."
				}
				d.add(Finding{Rule: "index-scan", Severity: "low", Table: n.Table, Badge: "index scan",
					Title: "Full index scan on " + tick(n.Table),
					Why:   fmt.Sprintf("The database walks the whole index %s (~%s entries) instead of jumping to the rows it needs.", tick(n.Index), fmtRows(rows)),
					Fix:   fix, Suggest: si})
			}
		case n.Index != "" && n.Table != "" && !n.FullScan:
			d.selectivity(n)
		}
		d.examined(n)
		if n.Filesort {
			d.filesort(n)
		}
		if n.Temporary {
			d.temporary(n)
		}
		if n.DiskSort {
			fix := "Give the sort an index that returns rows in ORDER BY order, or sort fewer rows (filter first, paginate)."
			if res.Engine == "pgsql" {
				fix += " For a one-off report you can raise " + tick("work_mem") + " for the session (SET work_mem = '64MB')."
			}
			d.add(Finding{Rule: "disk-sort", Severity: "high", Table: n.Table, Badge: "disk sort",
				Title: "The sort spills to disk",
				Why:   "There are too many rows to sort in memory, so the database writes them to temporary files and merges them — much slower than an in-memory sort.",
				Fix:   fix, Suggest: -1})
		}
		if n.DiskHash {
			fix := "Join or group fewer rows (filter earlier), or index the join columns so a nested loop can seek instead of hashing everything."
			if res.Engine == "pgsql" {
				fix += " Raising " + tick("work_mem") + " for the session lets the hash fit in memory."
			}
			d.add(Finding{Rule: "disk-hash", Severity: "medium", Badge: "disk hash",
				Title: "The hash table spills to disk",
				Why:   "A hash join or aggregate needed more memory than it was allowed, so it was split into batches on disk.",
				Fix:   fix, Suggest: -1})
		}
		if n.Dependent {
			d.add(Finding{Rule: "dependent-subquery", Severity: "medium", Badge: "dependent subquery",
				Title:   "A subquery runs once per row",
				Why:     "The subquery refers to the outer query (it is correlated), so the database runs it again for every row the outer query reads" + loopsText(n) + ".",
				Fix:     "Rewrite it as a JOIN, or let Eloquent do it in one query: " + tick("->withCount('posts')") + " / " + tick("->withExists('posts')") + " instead of a counting subquery, " + tick("->whereIn('id', Post::select('user_id'))") + " instead of a correlated " + tick("whereHas") + " on big tables. An index on the subquery's join column makes each run cheap.",
				Suggest: -1})
		}
		if n.Lookup && n.Rows*maxf(n.Loops, 1) >= SmallTable {
			d.add(Finding{Rule: "key-lookup", Severity: "low", Table: n.Table, Badge: "key lookup",
				Title:   "Each matched row is fetched again (Key Lookup)",
				Why:     fmt.Sprintf("The index finds ~%s rows but lacks some selected columns, so each row is looked up in the table again.", fmtRows(n.Rows)),
				Fix:     "Select only the columns you need, or make the index covering by adding the selected columns as INCLUDE columns.",
				Suggest: -1})
		}
		if n.AutoIndex && n.Table != "" {
			fix, si := d.indexFix(n.Table, purpose{filter: true}, "SQLite builds this index on the fly every time the query runs.")
			if fix == "" {
				fix = "Create an index on the columns in the plan's AUTOMATIC INDEX line."
			}
			d.add(Finding{Rule: "auto-index", Severity: "medium", Table: n.Table, Badge: "no index",
				Title: "SQLite builds a temporary index on " + tick(n.Table) + " each time",
				Why:   "There's no index for this lookup, so SQLite creates one for the duration of the query (" + n.Op + ") — a sure sign a permanent one is missing.",
				Fix:   fix, Suggest: si})
		}
	})
	// Missing-index hints the plan carried but no rule claimed.
	for _, m := range res.Plan.Missing {
		i, _ := d.suggestFor(m.Table, purpose{filter: true}, "")
		sev := "medium"
		if m.Impact >= 50 {
			sev = "high"
		}
		d.add(Finding{Rule: "missing-index", Severity: sev, Table: m.Table, Badge: "missing index",
			Title: "SQL Server suggests an index on " + tick(m.Table),
			Why:   fmt.Sprintf("The optimizer estimates this query would be %.0f%% cheaper with it.", m.Impact),
			Fix:   "Create the index below (the server's own suggestion — check it doesn't duplicate an existing one).", Suggest: i})
	}
	for _, w := range res.Plan.Warnings {
		lm := strings.ToLower(w.Message)
		switch {
		case strings.Contains(lm, "conversion") || w.Code == "PlanAffectingConvert":
			d.add(Finding{Rule: "implicit-conversion", Severity: "medium", Badge: "conversion",
				Title: "A type conversion stops an index being used",
				Why:   "The server reported: " + w.Message,
				Fix:   "Compare the column with a value of its own type — e.g. pass a string to a VARCHAR column (" + tick("(string) $id") + "), or fix the column's type in a migration.", Suggest: -1})
		case w.Code == "SpillToTempDb":
			// Reported on the node already.
		default:
			d.add(Finding{Rule: "server-warning", Severity: "low", Title: "The server warned about this plan", Why: w.Message, Suggest: -1, Table: w.Code})
		}
	}
}

func loopsText(n *Node) string {
	if n.Loops > 1 {
		return fmt.Sprintf(" (%s times here)", fmtRows(n.Loops))
	}
	return ""
}

func (d *diag) fullScan(n *Node) {
	rows := d.tableRows(n.Table, n)
	if rows < SmallTable {
		return
	}
	preds := d.predsOn(n.Table)
	joins := d.joinsOn(n.Table)
	usable := false
	for _, p := range preds {
		if p.Func == "" && !p.InOr && (p.Op == "eq" || p.Op == "range") {
			usable = true
		}
	}
	sev := sizeSeverity(rows)
	f := Finding{Rule: "full-scan", Severity: sev, Table: n.Table, Badge: "full scan", Suggest: -1,
		Title: "Full table scan on " + tick(n.Table)}
	f.Why = fmt.Sprintf("The database reads all ~%s rows of %s to find the ones it needs.", fmtRows(rows), tick(n.Table))
	if rows < BigTable {
		f.Why += " It's a small table, so this is cheap today — but it grows with the data."
	}
	switch {
	case len(preds) == 0 && len(joins) == 0:
		f.Why += " The query has no condition on this table, so every row is wanted."
		f.Fix = "That's expected for a full listing. If you don't need every row, paginate (" + tick("->paginate()") + " / " + tick("->cursorPaginate()") + "), add a " + tick("->limit()") + ", or filter it."
		if rows < BigTable || d.info.HasLim {
			f.Severity = "info"
		} else if f.Severity == "high" {
			f.Severity = "medium"
		}
	case len(n.Possible) > 0 && n.Index == "" && (d.res.Engine == "mysql" || d.res.Engine == "mariadb"):
		f.Rule, f.Badge = "index-unused", "index unused"
		f.Title = "An index exists but MySQL chose a full scan of " + tick(n.Table)
		f.Why += " It considered " + tick(strings.Join(n.Possible, ", ")) + " but estimated that reading the whole table is cheaper — the value probably matches a large share of the rows (low selectivity), or the table's statistics are stale."
		f.Fix = "Run " + tick("ANALYZE TABLE "+n.Table) + " to refresh statistics. If the plan stays, the filter isn't selective: combine it with a more selective column in one composite index, or accept the scan for this query."
	default:
		why := purpose{filter: true}
		fix, si := d.indexFix(n.Table, why, "Lets the database find the rows by "+strings.Trim(colNames(append(preds, joins...)), "`")+" instead of scanning "+n.Table+".")
		if !usable && len(joins) == 0 {
			f.Why += " The conditions on it (" + colNames(preds) + ") can't use an index as written — see the other findings."
			if fix == "" {
				fix = "Rewrite the condition so the column stands alone (see below), then index it."
			}
		} else if si >= 0 {
			f.Title = "No usable index — full scan of " + tick(n.Table)
			f.Badge = "no index"
			f.Why += " No index on it starts with the column(s) the query filters on (" + colNames(append(preds, joins...)) + ")."
		}
		if si < 0 && len(preds) == 0 && len(joins) > 0 && fix != "" {
			// Joined on an indexed column and read whole all the same.
			fix = "It's joined on an indexed column, but the join needs a large share of its rows, so reading it whole (a hash join) is cheaper than looking each one up. Filter the other side of the join more, or select fewer rows — otherwise this is fine."
			if f.Severity == "high" {
				f.Severity = "medium"
			} else {
				f.Severity = "low"
			}
		}
		f.Fix, f.Suggest = fix, si
		if f.Fix == "" {
			f.Fix = "Index the columns the WHERE uses on this table."
		}
	}
	if n.Filtered > 0 && n.Filtered <= 10 && n.ActualRows < 0 {
		f.Why += fmt.Sprintf(" It expects to keep only ~%.0f%% of what it reads.", n.Filtered)
	}
	d.add(f)
}

func (d *diag) selectivity(n *Node) {
	rows := n.Rows
	total := d.tableRows(n.Table, nil)
	if rows < BigTable*10 || total <= 0 || rows/total < 0.3 || len(d.predsOn(n.Table)) == 0 {
		return
	}
	fix, si := d.indexFix(n.Table, purpose{filter: true}, "A composite index narrows the search more than "+n.Index+" alone.")
	if si < 0 {
		fix = "The index fits the query, but the value matches too many rows for it to help much. Add a more selective column the query also filters on to a composite index, or fetch fewer rows (paginate, " + tick("->limit()") + ")."
	}
	d.add(Finding{Rule: "low-selectivity", Severity: "medium", Table: n.Table, Badge: "low selectivity",
		Title: "The index " + tick(n.Index) + " barely narrows the search",
		Why:   fmt.Sprintf("Using it still leaves ~%s of the ~%s rows of %s to read — the indexed value is shared by a big share of the rows.", fmtRows(rows), fmtRows(total), tick(n.Table)),
		Fix:   fix, Suggest: si})
}

func (d *diag) examined(n *Node) {
	if n.Table == "" {
		return
	}
	var read, kept float64
	measured := false
	switch {
	case n.ActualRows >= 0 && n.Removed > 0 && d.res.Engine == "mysql":
		// mergeActual: ActualRows is what the scan read, Removed (a
		// total) what the Filter above it dropped.
		read = n.ActualRows * maxf(n.Loops, 1)
		kept = read - n.Removed
		measured = true
	case n.ActualRows >= 0 && n.Removed > 0:
		// PostgreSQL: both per loop.
		kept = n.ActualRows * maxf(n.Loops, 1)
		read = kept + n.Removed*maxf(n.Loops, 1)
		measured = true
	case n.ActualRows >= 0 && d.res.Engine == "mariadb" && n.Filtered > 0:
		read = n.ActualRows * maxf(n.Loops, 1)
		kept = read * n.Filtered / 100
		measured = true
	default:
		return
	}
	if !measured || read < BigTable || kept*10 > read {
		return
	}
	why := fmt.Sprintf("Measured: %s rows of %s were read and %s of them thrown away by the filter.", fmtRows(read), tick(n.Table), fmtRows(read-kept))
	// Already reported as a scan: add the measurement there.
	for i, f := range d.res.Findings {
		if (f.Rule == "full-scan" || f.Rule == "index-unused") && strings.EqualFold(f.Table, n.Table) {
			d.res.Findings[i].Why += " " + why
			return
		}
	}
	fix, si := d.indexFix(n.Table, purpose{filter: true}, "Lets the database skip the rows the filter throws away.")
	if fix == "" {
		fix = "Index the filtered columns so the rows that don't match are never read."
	}
	d.add(Finding{Rule: "rows-examined", Severity: sizeSeverity(read), Table: n.Table, Badge: "rows examined",
		Title: fmt.Sprintf("Reads ~%s rows to return ~%s", fmtRows(read), fmtRows(kept)),
		Why:   why,
		Fix:   fix, Suggest: si})
}

func (d *diag) filesort(n *Node) {
	rows := n.Rows
	if n.Filtered > 0 && n.Filtered < 100 {
		rows = n.Rows * n.Filtered / 100 // MySQL sorts what the filter keeps
	}
	if n.ActualRows >= 0 {
		rows = n.ActualRows * maxf(n.Loops, 1)
	}
	table := n.Table
	if table == "" {
		table = d.mainTable()
	}
	sev := "low"
	if rows >= BigTable {
		sev = "medium"
	}
	order := ""
	for _, o := range d.info.OrderBy {
		order += ", " + o.Col
		if o.Desc {
			order += " desc"
		}
	}
	why := "The rows are sorted after they're read, because no index returns them in the ORDER BY order"
	if order != "" {
		why += " (" + strings.TrimPrefix(order, ", ") + ")"
	}
	why += "."
	if rows > 0 {
		why += fmt.Sprintf(" ~%s rows are sorted.", fmtRows(rows))
	}
	fix, si := "", -1
	if table != "" && len(d.info.OrderBy) > 0 && !d.info.Random {
		fix, si = d.indexFix(table, purpose{filter: true, order: true}, "Returns the rows already in ORDER BY order, so no sort is needed.")
	}
	if fix == "" {
		fix = "An index whose columns are the WHERE's equality columns followed by the ORDER BY columns lets the database read rows in order. Sorting on an expression or on columns from several tables always needs a sort — then sort fewer rows."
	}
	d.add(Finding{Rule: "filesort", Severity: sev, Table: table, Badge: "filesort", Title: "Sorts the rows (filesort)", Why: why, Fix: fix, Suggest: si})
}

func (d *diag) temporary(n *Node) {
	table := n.Table
	if table == "" {
		table = d.mainTable()
	}
	what := "GROUP BY / DISTINCT"
	if len(d.info.GroupBy) == 0 && len(d.info.OrderBy) > 0 {
		what = "ORDER BY across tables / DISTINCT"
	}
	fix, si := "", -1
	if table != "" && len(d.info.GroupBy) > 0 {
		fix, si = d.indexFix(table, purpose{filter: true, group: true}, "Lets GROUP BY read groups in index order, without a temporary table.")
	}
	if fix == "" {
		fix = "Index the GROUP BY columns (after the WHERE's equality columns), or group/sort on columns of one table only."
	}
	d.add(Finding{Rule: "temporary", Severity: "medium", Table: table, Badge: "temporary",
		Title: "Builds a temporary table",
		Why:   "To resolve the " + what + ", the database copies rows into a temporary table first — on large results it goes to disk.",
		Fix:   fix, Suggest: si})
}

func (d *diag) mainTable() string {
	for _, t := range d.info.Tables {
		if t.Main {
			return t.Name
		}
	}
	return ""
}

// ── rules on the SQL ──

var dateFuncs = map[string]bool{"date": true, "year": true, "month": true, "day": true, "date_format": true, "to_char": true, "strftime": true, "extract": true, "unix_timestamp": true, "datepart": true, "convert": true, "date_trunc": true, "cast": true}
var caseFuncs = map[string]bool{"lower": true, "upper": true, "lcase": true, "ucase": true}

func (d *diag) sqlRules(in Input) {
	info, res := d.info, d.res
	for _, p := range info.Where {
		t := p.Table
		big := d.tableRows(t, nil)
		switch {
		case p.Op == "like":
			sev := "medium"
			if big >= HugeTable {
				sev = "high"
			}
			fix := "A B-tree index can only match a LIKE from its start ('abc%'). For search: "
			code := ""
			switch res.Engine {
			case "mysql", "mariadb":
				fix += "add a FULLTEXT index and query it with " + tick("->whereFullText('"+p.Col+"', $term)") + "."
				code = "Schema::table('" + t + "', function (Blueprint $table) {\n    $table->fullText('" + p.Col + "');\n});"
			case "pgsql":
				fix += "a trigram index (pg_trgm) makes LIKE '%x%' and ILIKE fast."
				code = "CREATE EXTENSION IF NOT EXISTS pg_trgm;\nCREATE INDEX " + IndexName(t, []string{p.Col}) + "_trgm ON " + quoteIdent("pgsql", t) + " USING gin (" + quoteIdent("pgsql", p.Col) + " gin_trgm_ops);"
			case "sqlite":
				fix += "an FTS5 virtual table, or Laravel Scout for app search."
			default:
				fix += "a full-text index, or Laravel Scout."
			}
			fix += " If you only need 'starts with', drop the leading %."
			d.add(Finding{Rule: "leading-wildcard", Severity: sev, Table: t, Badge: "leading %",
				Title: "LIKE with a leading wildcard on " + tick(p.Col),
				Why:   fmt.Sprintf("%s LIKE %s starts with a wildcard, so no index on %s can be used — every row is checked.", tick(p.Col), quoteVal(p.Value), tick(p.Col)),
				Fix:   fix, Code: code, Suggest: -1})
		case p.Func != "":
			d.funcOnColumn(p)
		}
		// String column compared with a number (unless the server
		// already reported the conversion).
		if p.ValueKind == "number" && (res.Engine == "mysql" || res.Engine == "mariadb" || res.Engine == "sqlsrv") && !d.has("implicit-conversion") {
			if ti := tableInfo(res, t); ti != nil {
				typ := strings.ToLower(colType(ti, p.Col))
				if typ != "" && (strings.Contains(typ, "char") || strings.Contains(typ, "text") || typ == "enum" || typ == "set") {
					d.add(Finding{Rule: "implicit-conversion", Severity: "medium", Table: t, Badge: "conversion",
						Title: tick(p.Col) + " is a string column compared with a number",
						Why:   fmt.Sprintf("%s is %s but is compared with %v, so the database converts every row's value to a number before comparing — an index on %s can't be used.", tick(p.Col), strings.ToUpper(typ), p.Value, tick(p.Col)),
						Fix:   "Pass the value as a string: " + tick("->where('"+p.Col+"', (string) $value)") + " — or change the column to a numeric type if it only holds numbers.", Suggest: -1})
				}
			}
		}
	}
	for _, group := range info.OrCols {
		var cols []string
		for _, c := range group {
			cols = append(cols, tick(strings.TrimPrefix(c[strings.LastIndexByte(c, '.')+1:], "")))
		}
		d.add(Finding{Rule: "or-columns", Severity: "medium", Badge: "OR", Table: strings.Join(group, ","),
			Title:   "OR across different columns (" + strings.Join(cols, ", ") + ")",
			Why:     "One index can't serve conditions on different columns joined by OR, so the database often scans instead (MySQL can sometimes merge two indexes, at a cost).",
			Fix:     "Index each column separately and split the query into a UNION of the two conditions — in Laravel, " + tick("$a->union($b)") + " — or restructure the filter (e.g. a single search column).",
			Suggest: -1})
	}
	if info.Random {
		rows := d.tableRows(d.mainTable(), nil)
		sev := "medium"
		if rows >= BigTable {
			sev = "high"
		}
		d.add(Finding{Rule: "order-random", Severity: sev, Table: d.mainTable(), Badge: "RAND()",
			Title: "ORDER BY RAND() sorts the whole table",
			Why: "To pick random rows the database gives every row a random number and sorts them all, every time" + func() string {
				if rows > 0 {
					return fmt.Sprintf(" (~%s rows here)", fmtRows(rows))
				}
				return ""
			}() + ".",
			Fix:     "Pick random ids instead — e.g. fetch the id range, choose ids in PHP and " + tick("->whereIn('id', $ids)") + " — or cache one random selection for a while instead of " + tick("inRandomOrder()") + " per request.",
			Suggest: -1})
	}
	if info.Offset >= 1000 {
		sev := "medium"
		if info.Offset >= 10000 {
			sev = "high"
		}
		d.add(Finding{Rule: "deep-offset", Severity: sev, Badge: "deep offset",
			Title:   fmt.Sprintf("Deep pagination (OFFSET %d)", info.Offset),
			Why:     fmt.Sprintf("OFFSET %d makes the database read and throw away %d rows before returning the page — later pages get slower and slower.", info.Offset, info.Offset),
			Fix:     "Use keyset (cursor) pagination: " + tick("->orderBy('id')->cursorPaginate(50)") + ", or " + tick("->where('id', '>', $lastId)->orderBy('id')->limit(50)") + ".",
			Suggest: -1})
	}
	if info.Star && info.Verb == "select" {
		if t := d.mainTable(); t != "" {
			if ti := tableInfo(res, t); ti != nil && len(ti.Columns) > 0 {
				var wide []string
				for c, typ := range ti.Columns {
					lt := strings.ToLower(typ)
					if strings.Contains(lt, "text") || strings.Contains(lt, "blob") || strings.Contains(lt, "json") || lt == "bytea" || strings.Contains(lt, "binary") || strings.Contains(lt, "(max)") {
						wide = append(wide, c)
					}
				}
				sort.Strings(wide)
				if len(ti.Columns) >= 15 || len(wide) > 0 {
					why := fmt.Sprintf("SELECT * fetches all %d columns of %s", len(ti.Columns), tick(t))
					if len(wide) > 0 {
						why += ", including large ones (" + strings.Join(wide, ", ") + ")"
					}
					why += " — more to read, send and hydrate, and an index can never cover the query."
					title := "SELECT * on a wide table"
					if len(wide) > 0 {
						title = "SELECT * fetches large columns"
					}
					d.add(Finding{Rule: "select-star", Severity: "low", Table: t, Badge: "SELECT *",
						Title: title, Why: why,
						Fix:     "Select the columns you use: " + tick("->select(['id', 'name', …])") + " (with eager loads, keep the keys the relations need).",
						Suggest: -1})
				}
			}
		}
	}
	if info.NoWhere {
		d.add(Finding{Rule: "no-where", Severity: "high", Table: d.mainTable(), Badge: "no WHERE",
			Title:   strings.ToUpper(info.Verb) + " without a WHERE",
			Why:     "This statement changes every row of the table.",
			Fix:     "If that's not intended, add the condition. For big tables, do it in chunks (" + tick("->chunkById(1000, …)") + ") to keep locks short.",
			Suggest: -1})
	}
}

func colType(t *Table, col string) string {
	if v, ok := t.Columns[col]; ok {
		return v
	}
	for k, v := range t.Columns {
		if strings.EqualFold(k, col) {
			return v
		}
	}
	return ""
}

func quoteVal(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return fmt.Sprint(v)
}

func (d *diag) funcOnColumn(p Pred) {
	res := d.res
	t := p.Table
	why := fmt.Sprintf("%s(%s) wraps the column in a function, so an index on %s can't be used — the database computes it for every row.", strings.ToUpper(p.Func), p.Col, tick(p.Col))
	fix, code := "", ""
	switch {
	case dateFuncs[p.Func]:
		fix = "Compare the bare column with a range instead: " + tick("whereDate('"+p.Col+"', $day)") + " becomes " + tick("whereBetween('"+p.Col+"', [$day->startOfDay(), $day->endOfDay()])") + " (and " + tick("whereYear") + " a range over the year), then index " + tick(p.Col) + "."
	case caseFuncs[p.Func]:
		switch res.Engine {
		case "mysql", "mariadb":
			fix = "MySQL's default collations are case-insensitive already — compare the bare column (" + tick("->where('"+p.Col+"', $value)") + "). Otherwise index the expression (MySQL 8.0.13+)."
			code = "Schema::table('" + t + "', function (Blueprint $table) {\n    $table->rawIndex('(" + p.Func + "(" + p.Col + "))', '" + IndexName(t, []string{p.Func, p.Col}) + "');\n});"
		case "pgsql":
			fix = "Index the expression, so PostgreSQL can use it for exactly this condition (or use the citext type / ILIKE with a trigram index)."
			code = "CREATE INDEX " + IndexName(t, []string{p.Func, p.Col}) + " ON " + quoteIdent("pgsql", t) + " (" + p.Func + "(" + quoteIdent("pgsql", p.Col) + "));"
		case "sqlite":
			fix = "Declare the column COLLATE NOCASE and compare it bare, or create an index on the expression."
			code = "CREATE INDEX " + IndexName(t, []string{p.Func, p.Col}) + " ON " + quoteIdent("sqlite", t) + " (" + p.Func + "(" + quoteIdent("sqlite", p.Col) + "));"
		default:
			fix = "SQL Server's default collation is case-insensitive — compare the bare column; otherwise add a computed column and index it."
		}
	default:
		fix = "Rewrite the condition so the column stands alone on one side (move the function to the value), or index the expression itself."
	}
	d.add(Finding{Rule: "function-on-column", Severity: "medium", Table: t, Badge: "function on column",
		Title: "A function on " + tick(p.Col) + " in the WHERE", Why: why, Fix: fix, Code: code, Suggest: -1})
}

// ── N+1 ──

// Relation is a guess at the Eloquent relation behind an N+1 query.
type Relation struct {
	Name string `json:"name"` // e.g. "posts"
	Kind string `json:"kind"` // belongsTo, hasMany, morphMany, belongsToMany
	Key  string `json:"key"`  // the column the lazy load filters on
}

// GuessRelation reads the relation a lazy load is for from the query
// Eloquent runs: `where "posts"."user_id" = ?` (or IN) is a hasMany
// posts on the owner, `where "users"."id" = ?` a belongsTo user, a
// pivot join a belongsToMany.
func GuessRelation(info SQLInfo) (Relation, bool) {
	main := ""
	for _, t := range info.Tables {
		if t.Main {
			main = t.Name
			break
		}
	}
	if main == "" {
		return Relation{}, false
	}
	hasType := false
	for _, p := range info.Where {
		if p.Op == "eq" && strings.HasSuffix(strings.ToLower(p.Col), "_type") {
			hasType = true
		}
	}
	for _, p := range info.Where {
		if p.Op != "eq" || p.InOr || p.Sub || p.Func != "" || p.ValueKind == "column" {
			continue
		}
		col := strings.ToLower(p.Col)
		onMain := strings.EqualFold(lastPart(p.Table), lastPart(main))
		switch {
		case !onMain && strings.HasSuffix(col, "_id"):
			return Relation{Name: camel(lastPart(main)), Kind: "belongsToMany", Key: p.Col}, true
		case onMain && strings.HasSuffix(col, "_id"):
			kind := "hasMany"
			if hasType {
				kind = "morphMany"
			}
			return Relation{Name: camel(lastPart(main)), Kind: kind, Key: p.Col}, true
		case onMain && (col == "id" || col == "uuid" || col == "ulid"):
			return Relation{Name: camel(singular(lastPart(main))), Kind: "belongsTo", Key: p.Col}, true
		}
	}
	return Relation{}, false
}

func camel(s string) string {
	parts := strings.Split(strings.ToLower(s), "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// singular undoes the common English plurals table names use.
func singular(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.HasSuffix(l, "ies") && len(l) > 4:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(l, "sses"), strings.HasSuffix(l, "shes"), strings.HasSuffix(l, "ches"), strings.HasSuffix(l, "xes"), strings.HasSuffix(l, "zes"):
		return s[:len(s)-2]
	case strings.HasSuffix(l, "ss"):
		return s
	case strings.HasSuffix(l, "s") && len(l) > 1:
		return s[:len(s)-1]
	}
	return s
}

func (d *diag) nPlusOne(in Input) {
	if in.NPlusOne < 2 {
		return
	}
	f := Finding{Rule: "n-plus-one", Severity: "high", Badge: "N+1", Suggest: -1,
		Title: fmt.Sprintf("N+1: this query ran %d times in one request", in.NPlusOne)}
	f.Why = "It's the same query with a different value each time — the signature of a relation loaded lazily inside a loop (one query per model). However fast each one is, the round trips add up."
	rel, ok := GuessRelation(d.info)
	if ok {
		f.Table = d.mainTable()
		f.Why += fmt.Sprintf(" The filter on %s suggests a %s relation: %s.", tick(rel.Key), rel.Kind, tick(rel.Name))
		f.Fix = "Eager-load the relation before the loop, so it's one query for all rows: " + tick("->with('"+rel.Name+"')") + " on the query (or " + tick("$models->load('"+rel.Name+"')") + " on a collection)."
		f.Code = "// Before: one query per model\n$items = Model::all();\n// After: one query for all of them\n$items = Model::with('" + rel.Name + "')->get();"
		// The lazy load's key should be indexed too.
		if strings.HasSuffix(strings.ToLower(rel.Key), "_id") {
			tbl := f.Table
			if rel.Kind == "belongsToMany" {
				for _, p := range d.info.Where {
					if strings.EqualFold(p.Col, rel.Key) {
						tbl = p.Table
					}
				}
			}
			if i, covered := d.suggestFor(tbl, purpose{filter: true}, "The relation's foreign key — each lazy (and eager) load filters on it."); i >= 0 {
				f.Fix += " Also index the foreign key (below)."
				f.Suggest = i
			} else {
				_ = covered
			}
		}
	} else {
		f.Fix = "Find the loop that runs it (the file:line above) and load the data once before it — " + tick("->with('relation')") + " for a relation, or one " + tick("whereIn") + " query keyed by id."
	}
	f.Fix += " To catch these early, add " + tick("Model::preventLazyLoading(! app()->isProduction());") + " to AppServiceProvider::boot()."
	d.add(f)
}
