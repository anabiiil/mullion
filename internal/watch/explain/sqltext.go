package explain

// A small SQL reader for what the plan doesn't say: which tables and
// columns a statement filters and sorts on, how (equality, range, a
// function around the column, a leading-wildcard LIKE, OR across
// columns), its ORDER BY / OFFSET, SELECT * — enough for the diagnosis
// rules and the index suggestions. It reads the statements Laravel's
// query builder writes; it is not a full SQL parser and errs on the
// side of saying nothing.

import (
	"strconv"
	"strings"
)

type tok struct {
	kind byte   // 'w' word, 'q' quoted identifier, 's' string, 'n' number, 'p' placeholder, 'o' operator/punctuation
	text string // as written (identifiers unquoted)
	low  string // lower-cased text
	ph   int    // placeholder index (kind 'p')
	dep  int    // parenthesis depth at this token ("(" and ")" have the outer one)
}

// tokenize splits a statement into tokens, numbering "?" placeholders
// in order (Laravel's bindings are positional) and noting each token's
// parenthesis depth.
func tokenize(sql string) []tok {
	var out []tok
	ph, dep := 0, 0
	add := func(kind byte, text string) {
		out = append(out, tok{kind: kind, text: text, low: strings.ToLower(text), dep: dep})
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 4
			}
		case c == '\'':
			j := i + 1
			var b strings.Builder
			for j < len(sql) {
				if sql[j] == '\\' && j+1 < len(sql) {
					b.WriteByte(sql[j+1])
					j += 2
					continue
				}
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(sql[j])
				j++
			}
			add('s', b.String())
			i = j + 1
		case c == '"' || c == '`' || c == '[':
			end := byte(c)
			if c == '[' {
				end = ']'
			}
			j := strings.IndexByte(sql[i+1:], end)
			if j < 0 {
				j = len(sql) - i - 1
			}
			add('q', sql[i+1:i+1+j])
			i += j + 2
		case c == '?':
			out = append(out, tok{kind: 'p', text: "?", low: "?", ph: ph, dep: dep})
			ph++
			i++
		case c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(sql[i+1 : j])
			out = append(out, tok{kind: 'p', text: sql[i:j], low: sql[i:j], ph: n - 1, dep: dep})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(sql) && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '.') {
				j++
			}
			add('n', sql[i:j])
			i = j
		case (c == 'N' || c == 'n' || c == 'E' || c == 'e') && i+1 < len(sql) && sql[i+1] == '\'':
			i++ // N'unicode' (SQL Server) / E'escaped' (PostgreSQL): the string follows
		case isWordByte(c):
			j := i
			for j < len(sql) && (isWordByte(sql[j]) || sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			add('w', sql[i:j])
			i = j
		case c == '(':
			add('o', "(")
			dep++
			i++
		case c == ')':
			if dep > 0 {
				dep--
			}
			add('o', ")")
			i++
		default:
			// Two-character operators.
			if i+1 < len(sql) {
				two := sql[i : i+2]
				switch two {
				case "<=", ">=", "<>", "!=", "||", "::":
					add('o', two)
					i += 2
					continue
				}
			}
			add('o', string(c))
			i++
		}
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// Pred is one condition of a WHERE clause.
type Pred struct {
	Table string `json:"table,omitempty"` // the real table (alias resolved)
	Col   string `json:"col"`
	// Op is "eq" (=, IN, IS NULL), "range" (<, >, BETWEEN, LIKE 'x%'),
	// "like" (LIKE with a leading wildcard), "ne" or "other".
	Op string `json:"op"`
	// Func wraps the column (DATE(created_at) = ?) — no plain index helps.
	Func string `json:"func,omitempty"`
	// Value is the literal or the binding it is compared with (nil =
	// unknown); ValueKind "number", "string", "null", "column", "list",
	// "subquery" or "".
	Value     any    `json:"value,omitempty"`
	ValueKind string `json:"valueKind,omitempty"`
	InOr      bool   `json:"inOr,omitempty"`
	Sub       bool   `json:"sub,omitempty"` // in a subquery's WHERE
}

// ColRef is a column of ORDER BY / GROUP BY.
type ColRef struct {
	Table string `json:"table,omitempty"`
	Col   string `json:"col"`
	Desc  bool   `json:"desc,omitempty"`
}

// TableRef is a table the statement reads or writes.
type TableRef struct {
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
	Main  bool   `json:"main,omitempty"` // in the outer statement (not a subquery)
	depth int
}

// SQLInfo is what the reader found.
type SQLInfo struct {
	Verb   string     `json:"verb"`
	Tables []TableRef `json:"tables"`
	Star   bool       `json:"star,omitempty"`
	Where  []Pred     `json:"where,omitempty"`
	OrCols [][]string `json:"orCols,omitempty"` // OR groups across different columns ("t.c")
	// Joins are the column = column conditions of JOIN … ON, one Pred
	// per side (Value is the other side, "table.col").
	Joins   []Pred   `json:"joins,omitempty"`
	OrderBy []ColRef `json:"orderBy,omitempty"`
	GroupBy []ColRef `json:"groupBy,omitempty"`
	Random  bool     `json:"random,omitempty"` // ORDER BY RAND()
	Offset  int64    `json:"offset,omitempty"`
	HasLim  bool     `json:"hasLimit,omitempty"`
	NoWhere bool     `json:"noWhere,omitempty"` // UPDATE/DELETE with no WHERE
	Subq    bool     `json:"subquery,omitempty"`
}

var clauseEnd = map[string]bool{
	"group": true, "order": true, "limit": true, "having": true, "offset": true, "union": true,
	"for": true, "returning": true, "window": true, "fetch": true, "lock": true, "intersect": true, "except": true,
}

// notAlias are words that can follow a table name and aren't an alias.
var notAlias = map[string]bool{
	"where": true, "join": true, "inner": true, "left": true, "right": true, "full": true, "cross": true,
	"on": true, "group": true, "order": true, "limit": true, "offset": true, "having": true, "union": true,
	"set": true, "values": true, "using": true, "natural": true, "straight_join": true, "for": true,
	"lock": true, "window": true, "use": true, "force": true, "ignore": true, "with": true, "select": true,
	"outer": true, "returning": true, "fetch": true, "as": true, "default": true, "partition": true,
	"tablesample": true, "intersect": true, "except": true, "into": true, "from": true, "lateral": true,
}

// AnalyzeSQL reads a statement; bindings fill in placeholder values.
func AnalyzeSQL(sql string, bindings []any) SQLInfo {
	ts := tokenize(sql)
	info := SQLInfo{}
	// The verb: skip leading parentheses.
	for _, t := range ts {
		if t.kind == 'w' {
			info.Verb = t.low
			break
		}
		if t.text != "(" {
			break
		}
	}
	base := 0
	if len(ts) > 0 && ts[0].text == "(" {
		base = 1
	}
	info.Tables = tablesOf(ts, base)
	for i, t := range ts {
		if t.kind == 'w' && t.low == "select" && t.dep > base {
			info.Subq = true
		}
		// SELECT * / SELECT t.*
		if t.kind == 'w' && t.low == "select" && t.dep == base {
			j := i + 1
			if j < len(ts) && ts[j].low == "distinct" {
				j++
			}
			if j < len(ts) && ts[j].text == "*" {
				info.Star = true
			}
			if j+2 < len(ts) && (ts[j].kind == 'w' || ts[j].kind == 'q') && ts[j+1].text == "." && ts[j+2].text == "*" {
				info.Star = true
			}
		}
	}
	resolve := func(q string) string {
		if q == "" {
			for _, tr := range info.Tables {
				if tr.Main {
					return tr.Name
				}
			}
			if len(info.Tables) > 0 {
				return info.Tables[0].Name
			}
			return ""
		}
		lq := strings.ToLower(q)
		for _, tr := range info.Tables {
			if strings.ToLower(tr.Alias) == lq {
				return tr.Name
			}
		}
		for _, tr := range info.Tables {
			if strings.ToLower(tr.Name) == lq || strings.ToLower(lastPart(tr.Name)) == lq {
				return tr.Name
			}
		}
		return q
	}
	// WHERE clauses: the outer one, then those of subqueries (their
	// predicates are marked Sub).
	outer := false
	for wi, t := range ts {
		if t.kind != 'w' || t.low != "where" || t.dep < base {
			continue
		}
		end := wi + 1
		for end < len(ts) {
			e := ts[end]
			if e.dep < t.dep || e.text == ";" || e.dep == t.dep && e.kind == 'w' && clauseEnd[e.low] {
				break
			}
			end++
		}
		sub := SQLInfo{}
		r := &whereReader{ts: ts[wi+1 : end], bindings: bindings, resolve: resolve, info: &sub}
		r.conds(t.dep, false)
		info.OrCols = append(info.OrCols, sub.OrCols...)
		info.Subq = info.Subq || sub.Subq
		// A subquery's condition on an outer table (users.id = posts.user_id
		// inside a correlated subquery) is a reference, not a filter on it.
		here := func(table string) bool {
			if t.dep == base {
				return true
			}
			for _, tr := range info.Tables {
				if tr.depth == t.dep && strings.EqualFold(tr.Name, table) {
					return true
				}
			}
			return false
		}
		for _, p := range sub.Where {
			p.Sub = t.dep > base
			if here(p.Table) {
				info.Where = append(info.Where, p)
			}
			// col = other.col: the other side is a condition on its table too.
			if other, ok := p.Value.(string); ok && p.ValueKind == "column" && p.Op == "eq" {
				q, col := "", other
				if k := strings.LastIndexByte(other, '.'); k >= 0 {
					q, col = other[:k], other[k+1:]
				}
				m := Pred{Table: resolve(q), Col: col, Op: "eq", ValueKind: "column", Value: p.Table + "." + p.Col, Sub: p.Sub, InOr: p.InOr}
				if here(m.Table) {
					info.Where = append(info.Where, m)
				}
			}
		}
		if t.dep == base {
			outer = true
		}
	}
	if !outer && (info.Verb == "update" || info.Verb == "delete") {
		info.NoWhere = true
	}
	// JOIN … ON conditions (any level).
	for i, t := range ts {
		if t.kind != 'w' || t.low != "on" || i == 0 {
			continue
		}
		end := i + 1
		for end < len(ts) {
			e := ts[end]
			if e.dep < t.dep || e.dep == t.dep && (e.kind == 'w' && (e.low == "join" || e.low == "inner" || e.low == "left" || e.low == "right" || e.low == "cross" || e.low == "full" || e.low == "where" || clauseEnd[e.low])) {
				break
			}
			end++
		}
		sub := SQLInfo{}
		r := &whereReader{ts: ts[i+1 : end], bindings: bindings, resolve: resolve, info: &sub}
		r.conds(t.dep, false)
		for _, p := range sub.Where {
			if p.ValueKind != "column" || p.Op != "eq" {
				continue
			}
			info.Joins = append(info.Joins, p)
			other, _ := p.Value.(string)
			q, col := "", other
			if k := strings.LastIndexByte(other, '.'); k >= 0 {
				q, col = other[:k], other[k+1:]
			}
			info.Joins = append(info.Joins, Pred{Table: resolve(q), Col: col, Op: "eq", ValueKind: "column", Value: p.Table + "." + p.Col})
		}
	}
	// ORDER BY / GROUP BY / LIMIT / OFFSET at the outer level.
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.dep != base || t.kind != 'w' {
			continue
		}
		switch t.low {
		case "order", "group":
			if i+1 >= len(ts) || ts[i+1].low != "by" {
				continue
			}
			refs, random, next := colList(ts, i+2, base)
			for k := range refs {
				refs[k].Table = resolve(refs[k].Table)
			}
			if t.low == "order" {
				info.OrderBy, info.Random = refs, random
			} else {
				info.GroupBy = refs
			}
			i = next - 1
		case "limit":
			info.HasLim = true
			// MySQL's LIMIT offset, count.
			if i+3 < len(ts) && ts[i+2].text == "," {
				if v, ok := intValue(ts[i+1], bindings); ok {
					info.Offset = v
				}
			}
		case "offset":
			if i+1 < len(ts) {
				if v, ok := intValue(ts[i+1], bindings); ok {
					info.Offset = v
				}
			}
		case "top", "fetch":
			info.HasLim = true
		}
	}
	return info
}

func lastPart(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func intValue(t tok, bindings []any) (int64, bool) {
	switch t.kind {
	case 'n':
		v, err := strconv.ParseInt(t.text, 10, 64)
		return v, err == nil
	case 'p':
		if t.ph >= 0 && t.ph < len(bindings) {
			switch b := bindings[t.ph].(type) {
			case float64:
				return int64(b), true
			case int:
				return int64(b), true
			case int64:
				return b, true
			case string:
				v, err := strconv.ParseInt(b, 10, 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// ident reads a possibly qualified name at ts[i]: "a", "a"."b", s.t.
// It returns the parts and the index after them.
func ident(ts []tok, i int) ([]string, int) {
	if i >= len(ts) || ts[i].kind != 'w' && ts[i].kind != 'q' {
		return nil, i
	}
	parts := []string{ts[i].text}
	i++
	for i+1 < len(ts) && ts[i].text == "." && (ts[i+1].kind == 'w' || ts[i+1].kind == 'q') {
		parts = append(parts, ts[i+1].text)
		i += 2
	}
	return parts, i
}

func tablesOf(ts []tok, base int) []TableRef {
	var out []TableRef
	seen := map[string]bool{}
	for i := 0; i < len(ts); i++ {
		t := ts[i]
		if t.kind != 'w' {
			continue
		}
		start := -1
		switch t.low {
		case "from", "join", "into":
			start = i + 1
		case "update":
			if i == 0 || ts[i-1].low != "for" && ts[i-1].low != "key" && ts[i-1].low != "on" {
				start = i + 1
			}
		case "delete":
			// DELETE t FROM … names the table again after FROM.
			continue
		}
		if start < 0 {
			continue
		}
		for j := start; j < len(ts); {
			if j < len(ts) && ts[j].kind == 'w' && (ts[j].low == "only" || ts[j].low == "lateral" || ts[j].low == "ignore" || ts[j].low == "low_priority") {
				j++
			}
			if j < len(ts) && ts[j].text == "(" {
				break // a derived table
			}
			parts, next := ident(ts, j)
			if parts == nil || ts[j].kind == 'w' && notAlias[ts[j].low] {
				break
			}
			name := strings.Join(parts, ".")
			alias := ""
			k := next
			if k < len(ts) && ts[k].low == "as" {
				k++
			}
			// A function call (FROM generate_series(...)) isn't a table.
			if next < len(ts) && ts[next].text == "(" && t.low != "into" {
				break
			}
			if k < len(ts) && (ts[k].kind == 'w' && !notAlias[ts[k].low] || ts[k].kind == 'q') {
				alias = ts[k].text
				k++
			}
			key := strings.ToLower(name + "\x00" + alias)
			if !seen[key] {
				seen[key] = true
				out = append(out, TableRef{Name: name, Alias: alias, Main: t.dep == base, depth: t.dep})
			}
			if k < len(ts) && ts[k].text == "," && t.low == "from" {
				j = k + 1
				continue
			}
			break
		}
	}
	return out
}

func colList(ts []tok, i, base int) ([]ColRef, bool, int) {
	var out []ColRef
	random := false
	for i < len(ts) {
		t := ts[i]
		if t.dep < base || t.dep == base && t.kind == 'w' && clauseEnd[t.low] && t.low != "order" && t.low != "group" || t.text == ";" {
			break
		}
		if t.dep == base && t.kind == 'w' && (t.low == "limit" || t.low == "offset" || t.low == "having" || t.low == "order" || t.low == "fetch" || t.low == "union") {
			break
		}
		if t.kind == 'w' && (t.low == "rand" || t.low == "random" || t.low == "newid") && i+1 < len(ts) && ts[i+1].text == "(" {
			random = true
		}
		if t.dep == base && (t.kind == 'w' || t.kind == 'q') && !(i+1 < len(ts) && ts[i+1].text == "(") {
			parts, next := ident(ts, i)
			if t.kind == 'w' && (t.low == "asc" || t.low == "desc" || t.low == "nulls" || t.low == "first" || t.low == "last" || t.low == "collate") {
				if t.low == "desc" && len(out) > 0 {
					out[len(out)-1].Desc = true
				}
				i++
				continue
			}
			ref := ColRef{Col: parts[len(parts)-1]}
			if len(parts) > 1 {
				ref.Table = strings.Join(parts[:len(parts)-1], ".")
			}
			out = append(out, ref)
			i = next
			continue
		}
		i++
	}
	return out, random, i
}

// whereReader walks a WHERE clause's tokens.
type whereReader struct {
	ts       []tok
	pos      int
	bindings []any
	resolve  func(string) string
	info     *SQLInfo
}

func (r *whereReader) peek() *tok {
	if r.pos < len(r.ts) {
		return &r.ts[r.pos]
	}
	return nil
}

// conds reads conditions at depth dep until the group ends, splitting
// on AND; an OR at this level makes it an OR group.
func (r *whereReader) conds(dep int, inOr bool) {
	var group [][]Pred
	cur := []Pred{}
	hasOr := false
	for r.pos < len(r.ts) {
		t := r.ts[r.pos]
		if t.dep < dep {
			break
		}
		if t.dep == dep && t.kind == 'w' && t.low == "and" {
			r.pos++
			continue
		}
		if t.dep == dep && t.kind == 'w' && t.low == "or" {
			hasOr = true
			group = append(group, cur)
			cur = []Pred{}
			r.pos++
			continue
		}
		if t.dep == dep && t.kind == 'w' && t.low == "not" {
			r.pos++
			continue
		}
		if t.text == "(" && t.dep == dep && !r.isCallAt(r.pos-1) {
			r.pos++
			sub := r.collect(func() { r.conds(dep+1, false) })
			cur = append(cur, sub...)
			if p := r.peek(); p != nil && p.text == ")" {
				r.pos++
			}
			continue
		}
		if p, ok := r.pred(dep); ok {
			cur = append(cur, p)
			continue
		}
		r.pos++
	}
	group = append(group, cur)
	if !hasOr {
		for _, p := range cur {
			p.InOr = p.InOr || inOr
			r.info.Where = append(r.info.Where, p)
		}
		return
	}
	cols := map[string]bool{}
	var list []string
	for _, g := range group {
		for _, p := range g {
			p.InOr = true
			r.info.Where = append(r.info.Where, p)
			k := p.Table + "." + p.Col
			if !cols[k] {
				cols[k] = true
				list = append(list, k)
			}
		}
	}
	if len(list) > 1 {
		r.info.OrCols = append(r.info.OrCols, list)
	}
}

// collect runs fn and returns the predicates it added, removing them
// from info.Where (the caller re-adds them with its own OR context).
func (r *whereReader) collect(fn func()) []Pred {
	before := len(r.info.Where)
	fn()
	got := append([]Pred(nil), r.info.Where[before:]...)
	r.info.Where = r.info.Where[:before]
	return got
}

func (r *whereReader) isCallAt(i int) bool {
	if i < 0 || i >= len(r.ts) {
		return false
	}
	t := r.ts[i]
	return t.kind == 'w' && !isOpWord(t.low)
}

func isOpWord(w string) bool {
	switch w {
	case "and", "or", "not", "in", "exists", "where", "on", "like", "ilike", "is", "between":
		return true
	}
	return false
}

func (r *whereReader) value(t tok) (any, string) {
	switch t.kind {
	case 'p':
		if t.ph >= 0 && t.ph < len(r.bindings) {
			v := r.bindings[t.ph]
			switch v.(type) {
			case float64, int, int64, bool:
				return v, "number"
			case nil:
				return nil, "null"
			case string:
				return v, "string"
			}
			return v, ""
		}
		return nil, ""
	case 'n':
		f, _ := strconv.ParseFloat(t.text, 64)
		return f, "number"
	case 's':
		return t.text, "string"
	case 'w':
		if t.low == "null" {
			return nil, "null"
		}
		if t.low == "true" || t.low == "false" {
			return t.low == "true", "number"
		}
		return nil, "column"
	case 'q':
		return nil, "column"
	}
	return nil, ""
}

// pred reads one comparison: [func(] column [)] op value.
func (r *whereReader) pred(dep int) (Pred, bool) {
	start := r.pos
	ts := r.ts
	i := r.pos
	fn := ""
	if i+1 < len(ts) && ts[i].kind == 'w' && ts[i+1].text == "(" && !isOpWord(ts[i].low) {
		if ts[i].low == "exists" {
			r.info.Subq = true
			return Pred{}, false
		}
		fn = ts[i].low
		i += 2
		// The first column inside the call.
		for i < len(ts) && ts[i].dep > dep && ts[i].kind != 'q' && !(ts[i].kind == 'w' && !(i+1 < len(ts) && ts[i+1].text == "(")) {
			i++
		}
	}
	parts, next := ident(ts, i)
	if parts == nil || ts[i].kind == 'w' && (isOpWord(ts[i].low) || ts[i].low == "null") {
		r.pos = start
		return Pred{}, false
	}
	if fn != "" {
		// Skip to the call's closing parenthesis.
		for next < len(ts) && ts[next].dep > dep {
			next++
		}
		if next < len(ts) && ts[next].text == ")" {
			next++
		}
	}
	p := Pred{Col: parts[len(parts)-1], Func: fn}
	if len(parts) > 1 {
		p.Table = strings.Join(parts[:len(parts)-1], ".")
	}
	p.Table = r.resolve(p.Table)
	i = next
	// A cast (col::text) counts as a function.
	if i+1 < len(ts) && ts[i].text == "::" {
		p.Func = "cast"
		i += 2
	}
	if i >= len(ts) {
		r.pos = i
		return Pred{}, false
	}
	op := ts[i]
	not := false
	if op.low == "not" && i+1 < len(ts) {
		not = true
		i++
		op = ts[i]
	}
	switch {
	case op.text == "=":
		p.Op = "eq"
		if i+1 < len(ts) && (ts[i+1].kind == 'q' || ts[i+1].kind == 'w' && ts[i+1].low != "null" && ts[i+1].low != "true" && ts[i+1].low != "false" && !(i+2 < len(ts) && ts[i+2].text == "(")) {
			parts, next := ident(ts, i+1)
			p.Value, p.ValueKind = strings.Join(parts, "."), "column"
			i = next
			break
		}
		if i+1 < len(ts) {
			p.Value, p.ValueKind = r.value(ts[i+1])
			i++
		}
		i++
	case op.text == "<" || op.text == ">" || op.text == "<=" || op.text == ">=":
		p.Op = "range"
		if i+1 < len(ts) {
			p.Value, p.ValueKind = r.value(ts[i+1])
			i++
		}
		i++
	case op.text == "<>" || op.text == "!=":
		p.Op = "ne"
		i += 2
	case op.low == "in":
		p.Op = "eq"
		p.ValueKind = "list"
		if not {
			p.Op = "ne"
		}
		i++
		if i+1 < len(ts) && ts[i].text == "(" && ts[i+1].low == "select" {
			p.ValueKind = "subquery"
			r.info.Subq = true
		}
		// Skip the list.
		if i < len(ts) && ts[i].text == "(" {
			d := ts[i].dep
			i++
			for i < len(ts) && !(ts[i].dep == d && ts[i].text == ")") {
				i++
			}
			i++
		}
	case op.low == "is":
		p.Op = "eq"
		p.ValueKind = "null"
		i++
		if i < len(ts) && ts[i].low == "not" {
			p.Op = "ne"
			i++
		}
		i++
	case op.low == "between":
		p.Op = "range"
		if not {
			p.Op = "other"
		}
		i++
		for k := 0; k < 3 && i < len(ts); k++ {
			i++
		}
	case op.low == "like" || op.low == "ilike":
		i++
		if i < len(ts) {
			p.Value, p.ValueKind = r.value(ts[i])
			i++
		}
		p.Op = "range"
		if s, ok := p.Value.(string); ok && (strings.HasPrefix(s, "%") || strings.HasPrefix(s, "_")) {
			p.Op = "like"
		} else if !ok {
			p.Op = "other"
		}
		if not {
			p.Op = "other"
		}
		if op.low == "ilike" && p.Op == "range" {
			p.Op = "other" // ILIKE can't use a plain b-tree index
		}
	default:
		r.pos = start
		return Pred{}, false
	}
	r.pos = i
	return p, true
}

// Explainable checks a statement can be explained: SELECT (or WITH …),
// UPDATE, DELETE and INSERT … SELECT, one statement. analyzable is true
// for a plain read (SELECT, or WITH without data-changing statements,
// no SELECT … INTO): only those may be run by Analyze.
func Explainable(sql string) (verb string, analyzable bool, err error) {
	ts := tokenize(sql)
	for i, t := range ts {
		if t.text == ";" {
			for _, rest := range ts[i+1:] {
				if rest.text != ";" {
					return "", false, errMulti
				}
			}
		}
	}
	for _, t := range ts {
		if t.kind == 'w' {
			verb = t.low
			break
		}
		if t.text != "(" {
			break
		}
	}
	dml := false
	into := false
	selects := false
	for i, t := range ts {
		if t.kind != 'w' {
			continue
		}
		if i+1 < len(ts) && ts[i+1].text == "(" && (t.low == "replace" || t.low == "insert") {
			continue // REPLACE()/INSERT() string functions
		}
		switch t.low {
		case "insert", "update", "delete", "merge", "replace", "truncate", "drop", "create", "alter", "grant", "call", "copy":
			// UPDATE inside "FOR UPDATE" / "ON UPDATE" isn't a statement.
			dml = true
		case "into", "outfile", "dumpfile":
			into = true
		case "select":
			selects = true
		}
	}
	if verb == "select" || verb == "with" {
		forUpdate := false
		for i, t := range ts {
			if t.kind == 'w' && t.low == "update" && i > 0 && (ts[i-1].low == "for" || ts[i-1].low == "on" || ts[i-1].low == "key") {
				forUpdate = true
			}
		}
		if verb == "with" && dml && !forUpdate {
			return verb, false, nil
		}
		return verb, !into && !(dml && !forUpdate), nil
	}
	switch verb {
	case "update", "delete":
		return verb, false, nil
	case "insert", "replace":
		if selects {
			return verb, false, nil
		}
		return verb, false, errInsertValues
	case "":
		return "", false, errEmpty
	}
	return verb, false, errNotExplainable(verb)
}
