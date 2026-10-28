package perf

import (
	"sort"
	"strconv"
	"strings"

	"pm/internal/watch/explain"
)

// BigTableRows is the row estimate from which a table without a
// secondary index is worth a look.
const BigTableRows = 10000

// laravelInternal are framework tables whose shape Laravel owns.
var laravelInternal = map[string]bool{
	"migrations": true, "cache": true, "cache_locks": true, "sessions": true, "jobs": true,
	"job_batches": true, "failed_jobs": true, "password_reset_tokens": true, "password_resets": true,
	"personal_access_tokens": true, "telescope_entries": true, "telescope_entries_tags": true, "telescope_monitoring": true,
	"pulse_entries": true, "pulse_aggregates": true, "pulse_values": true,
}

// CheckSchema runs the database rules over a schema read: foreign keys
// (and *_id columns) without an index, tables without a primary key,
// big tables with no secondary index.
func CheckSchema(s *explain.Schema) []Finding {
	if s == nil {
		return nil
	}
	var out []Finding
	names := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	rowsOf := func(t *explain.Table) float64 {
		if t == nil || t.Rows == nil {
			return -1
		}
		return *t.Rows
	}
	// covered: some index starts with these columns, in order.
	covered := func(t *explain.Table, cols []string) bool {
		for _, ix := range t.Indexes {
			if len(ix.Columns) < len(cols) {
				continue
			}
			ok := true
			for i, c := range cols {
				if !strings.EqualFold(ix.Columns[i], c) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	fkCols := map[string]bool{}
	for _, fk := range s.ForeignKeys {
		t := s.Tables[fk.Table]
		if t == nil || len(fk.Columns) == 0 {
			continue
		}
		fkCols[strings.ToLower(fk.Table+"."+fk.Columns[0])] = true
		if covered(t, fk.Columns) {
			continue
		}
		cols := strings.Join(fk.Columns, ", ")
		sev := SevMedium
		if r := rowsOf(t); r >= 0 && r < 1000 {
			sev = SevLow
		}
		out = append(out, Finding{ID: "db.fk-index:" + fk.Table + "." + strings.Join(fk.Columns, "+"), Category: "database", Severity: sev,
			Title: fk.Table + "." + cols + " references " + fk.Ref + " but has no index",
			Why:   "Every query that loads " + fk.Table + " by its " + fk.Ref + " (a hasMany relation, a whereHas, a cascade on delete) scans the whole table without an index on " + cols + "." + rowsNote(rowsOf(t)),
			Fix:   indexFix(s.Engine, fk.Table, fk.Columns)})
	}
	for _, n := range names {
		t := s.Tables[n]
		if t == nil || t.Error != "" || laravelInternal[strings.ToLower(n)] {
			continue
		}
		rows := rowsOf(t)
		hasPK, secondary := false, 0
		for _, ix := range t.Indexes {
			if ix.Primary {
				hasPK = true
			} else {
				secondary++
			}
		}
		if !hasPK {
			out = append(out, Finding{ID: "db.no-pk:" + n, Category: "database", Severity: SevMedium,
				Title: n + " has no primary key",
				Why:   "Without a primary key rows can't be addressed reliably (updates and deletes by id scan the table), replication tools and Eloquent's find() need one, and InnoDB invents a hidden one." + rowsNote(rows),
				Fix:   "// migration\n$table->id();   // or $table->primary(['a_id', 'b_id']) for a pivot table"})
		}
		// *_id columns nobody indexed (no foreign key declared).
		var cols []string
		for c := range t.Columns {
			lc := strings.ToLower(c)
			if strings.HasSuffix(lc, "_id") && lc != "id" && !fkCols[strings.ToLower(n+"."+c)] && !covered(t, []string{c}) {
				cols = append(cols, c)
			}
		}
		sort.Strings(cols)
		if len(cols) > 0 && rows >= 1000 {
			out = append(out, Finding{ID: "db.id-index:" + n, Category: "database", Severity: SevLow,
				Title: n + ": " + strings.Join(cols, ", ") + " not indexed",
				Why:   "Columns named *_id usually hold a relation's key and are filtered on (where user_id = ?); without an index each such query reads every row." + rowsNote(rows),
				Fix:   indexFix(s.Engine, n, cols[:1])})
		}
		if rows >= BigTableRows && secondary == 0 {
			out = append(out, Finding{ID: "db.big-no-index:" + n, Category: "database", Severity: SevLow,
				Title: n + " has about " + fmtRows(rows) + " rows and no secondary index",
				Why:   "Any query on " + n + " that isn't by primary key reads the whole table. Index the columns your where, order by and join clauses use — the Watch tab's Explain shows which.",
				Fix:   "// migration\n$table->index(['column_you_filter_on']);"})
		}
	}
	return out
}

func rowsNote(rows float64) string {
	if rows < 0 {
		return ""
	}
	return " (about " + fmtRows(rows) + " rows now.)"
}

func fmtRows(r float64) string {
	switch {
	case r >= 1e6:
		return strconv.FormatFloat(float64(int(r/1e5))/10, 'f', -1, 64) + "M"
	case r >= 1e4:
		return strconv.Itoa(int(r/1e3)) + "k"
	}
	return strconv.Itoa(int(r))
}

// indexFix is the CREATE INDEX and the Laravel migration line.
func indexFix(engine, table string, cols []string) string {
	name := strings.ToLower(table + "_" + strings.Join(cols, "_") + "_index")
	q := func(s string) string {
		switch engine {
		case "mysql", "mariadb":
			return "`" + s + "`"
		case "sqlsrv":
			return "[" + s + "]"
		}
		return `"` + s + `"`
	}
	qc := make([]string, len(cols))
	for i, c := range cols {
		qc[i] = q(c)
	}
	mig := "'" + cols[0] + "'"
	if len(cols) > 1 {
		mig = "['" + strings.Join(cols, "', '") + "']"
	}
	return "CREATE INDEX " + q(name) + " ON " + q(table) + " (" + strings.Join(qc, ", ") + ");\n\n// migration\nSchema::table('" + table + "', function (Blueprint $table) {\n    $table->index(" + mig + ");\n});"
}
