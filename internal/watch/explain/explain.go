// Package explain shows why a query the Watch tab recorded is slow and
// how to fix it: it runs the database's EXPLAIN through the site's own
// PHP (PDO handles every driver and its quoting; a small embedded
// script, php/mullion-explain.php, does the talking), turns the plan
// into one model (plan.go, mssql.go), and diagnoses it in plain
// language with a concrete fix and, where an index would help, the
// CREATE INDEX and the Laravel migration line (diagnose.go, suggest.go).
//
// Only plans are read: plain EXPLAIN, EXPLAIN QUERY PLAN and
// SHOWPLAN_XML don't run the statement. Analyze (SELECT only, on
// request) runs it in a read-only transaction that is rolled back.
package explain

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pm/internal/proc"
)

//go:embed php/mullion-explain.php
var scriptPHP []byte

// Timeout bounds one explain (the PHP process is killed after it).
const Timeout = 15 * time.Second

// ScriptName is the file EnsureScript writes.
const ScriptName = "mullion-explain.php"

// EnsureScript writes the PHP script into dir (when it changed) and
// returns its path.
func EnsureScript(dir string) (string, error) {
	path := filepath.Join(dir, ScriptName)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, scriptPHP) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, scriptPHP, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// Runner sends the PHP script its JSON request and returns its output.
type Runner func(ctx context.Context, request []byte) ([]byte, error)

// PHPRunner runs script with the php binary (and its php.ini, when
// given). The site's watcher prepend is kept out: no ini scan dir, no
// auto_prepend_file.
func PHPRunner(php, ini, script string) Runner {
	return func(ctx context.Context, request []byte) ([]byte, error) {
		args := []string{}
		if ini != "" {
			if _, err := os.Stat(ini); err == nil {
				args = append(args, "-c", ini)
			}
		}
		args = append(args, "-d", "auto_prepend_file=", "-d", "display_errors=stderr", "-d", "max_execution_time=0", script)
		cmd := exec.CommandContext(ctx, php, args...)
		cmd.Env = cleanEnv(os.Environ())
		cmd.Dir = filepath.Dir(script)
		cmd.Stdin = bytes.NewReader(request)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		proc.HideConsole(cmd)
		err := cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("the explain took longer than %s and was stopped", Timeout)
		}
		if err != nil && out.Len() == 0 {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 400 {
				msg = msg[:400] + "…"
			}
			if msg == "" {
				msg = err.Error()
			}
			return nil, fmt.Errorf("PHP failed: %s", msg)
		}
		return out.Bytes(), nil
	}
}

// cleanEnv drops what would load the watcher's prepend into the PHP CLI.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "PHP_INI_SCAN_DIR", "MULLION_WATCH":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Input is one explain request.
type Input struct {
	SQL      string `json:"sql"`
	Bindings []any  `json:"bindings"`
	Analyze  bool   `json:"analyze"`
	// NPlusOne is how often the watcher saw this query repeat in one
	// request (0 = not an N+1).
	NPlusOne int `json:"nPlusOne,omitempty"`
}

// Index is one index of a table.
type Index struct {
	Name        string   `json:"name"`
	Unique      bool     `json:"unique"`
	Primary     bool     `json:"primary"`
	Columns     []string `json:"columns"`
	Include     []string `json:"include,omitempty"`
	Cardinality *float64 `json:"cardinality,omitempty"`
	Def         string   `json:"def,omitempty"`
}

// Table is what the database says about one table of the query.
type Table struct {
	Rows    *float64          `json:"rows"`
	Indexes []Index           `json:"indexes"`
	Columns map[string]string `json:"columns"`
	Error   string            `json:"error,omitempty"`
}

// Result is an explained query.
type Result struct {
	Engine      string            `json:"engine"`
	Server      string            `json:"server,omitempty"`
	Via         string            `json:"via,omitempty"`
	Conn        string            `json:"conn"`
	SQL         string            `json:"sql"`
	BoundSQL    string            `json:"boundSql"`
	Verb        string            `json:"verb"`
	CanAnalyze  bool              `json:"canAnalyze"`
	Plan        Plan              `json:"plan"`
	Tables      map[string]*Table `json:"tables"`
	Findings    []Finding         `json:"findings"`
	Suggestions []Suggestion      `json:"suggestions"`
	Badges      []string          `json:"badges"`
	Notes       []string          `json:"notes"`
	Ms          float64           `json:"ms"`
}

// phpResponse is the script's output.
type phpResponse struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error"`
	Driver  string            `json:"driver"`
	Via     string            `json:"via"`
	Server  string            `json:"server"`
	SQL     string            `json:"sql"`
	Tables  map[string]*Table `json:"tables"`
	Explain struct {
		Tabular   []map[string]any `json:"tabular"`
		Warnings  []Warning        `json:"warnings"`
		JSON      string           `json:"json"`
		JSONError string           `json:"jsonError"`
		Analyze   string           `json:"analyze"`
		Rows      []map[string]any `json:"rows"`
		XML       string           `json:"xml"`
	} `json:"explain"`
}

// Explain checks, runs, parses and diagnoses one statement.
func Explain(ctx context.Context, run Runner, c Conn, in Input) (*Result, error) {
	start := time.Now()
	sql := strings.TrimSpace(in.SQL)
	verb, analyzable, err := Explainable(sql)
	if err != nil {
		return nil, err
	}
	if in.Analyze && !analyzable {
		return nil, ErrAnalyzeSelect
	}
	if in.Analyze && c.Driver == "sqlite" {
		return nil, errors.New("SQLite has no EXPLAIN ANALYZE — its query plan is all it reports")
	}
	if in.Analyze && c.Driver == "sqlsrv" {
		return nil, errors.New("Analyze isn't available for SQL Server — the estimated plan (with its missing-index hints) is shown instead")
	}
	bindings, redacted := neutralBindings(sql, in.Bindings)
	info := AnalyzeSQL(sql, bindings)
	var tables []string
	seen := map[string]bool{}
	for _, t := range info.Tables {
		if !seen[strings.ToLower(t.Name)] && safeIdent.MatchString(t.Name) {
			seen[strings.ToLower(t.Name)] = true
			tables = append(tables, t.Name)
		}
	}
	req, _ := json.Marshal(map[string]any{
		"conn": map[string]any{"driver": c.Driver, "host": c.Host, "port": c.Port, "socket": c.Socket,
			"database": c.Database, "username": c.Username, "password": c.Password},
		"sql": sql, "bindings": bindings, "analyze": in.Analyze, "tables": tables,
	})
	cctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(cctx, req)
	if err != nil {
		return nil, errors.New(scrub(err.Error(), c.Password))
	}
	res, err := Build(out, c, in, info)
	if err != nil {
		return nil, errors.New(scrub(err.Error(), c.Password))
	}
	res.SQL, res.Verb, res.CanAnalyze = sql, verb, analyzable && c.Driver != "sqlite" && c.Driver != "sqlsrv"
	if redacted > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d binding(s) were redacted when recorded (secret-looking columns); Mullion used a neutral stand-in value, so the plan may differ slightly from the real one.", redacted))
	}
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

var safeIdent = regexp.MustCompile(`^[A-Za-z0-9_$]+(\.[A-Za-z0-9_$]+)?$`)

// Build turns the PHP script's output into a diagnosed Result.
func Build(out []byte, c Conn, in Input, info SQLInfo) (*Result, error) {
	var r phpResponse
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		s := strings.TrimSpace(string(out))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return nil, fmt.Errorf("unexpected output from PHP: %s", s)
	}
	if !r.OK {
		return nil, fmt.Errorf("%s: %s", c.Label(), r.Error)
	}
	res := &Result{Engine: r.Driver, Server: r.Server, Via: r.Via, Conn: c.Label(), BoundSQL: r.SQL, Tables: r.Tables,
		Findings: []Finding{}, Suggestions: []Suggestion{}, Badges: []string{}, Notes: []string{}}
	if res.Tables == nil {
		res.Tables = map[string]*Table{}
	}
	p, err := parsePlan(r, in.Analyze)
	if err != nil {
		return nil, err
	}
	res.Plan = p
	Diagnose(res, info, in)
	return res, nil
}

func parsePlan(r phpResponse, analyze bool) (Plan, error) {
	p := Plan{Engine: r.Driver, Warnings: r.Explain.Warnings}
	switch r.Driver {
	case "mysql", "mariadb":
		p.Tabular = r.Explain.Tabular
		p.Nodes = mysqlTabular(r.Explain.Tabular)
		p.Raw, p.RawKind = r.Explain.JSON, "json"
		p.Cost = mysqlJSONCost(r.Explain.JSON)
		if analyze && r.Explain.Analyze != "" {
			p.Analyzed = true
			if r.Driver == "mariadb" {
				// MariaDB's ANALYZE FORMAT=JSON: measured rows per table.
				nodes, ms := mariaAnalyzeJSON(r.Explain.Analyze)
				if len(nodes) > 0 {
					p.Nodes = nodes
				}
				p.TimeMs = ms
				p.Raw = r.Explain.Analyze
			} else {
				p.RawAnalyze = r.Explain.Analyze
				tree := mysqlAnalyzeTree(r.Explain.Analyze)
				mergeActual(p.Nodes, tree)
				p.Nodes = append(p.Nodes, &Node{Op: "EXPLAIN ANALYZE", ActualRows: -1, Children: tree})
				if len(tree) > 0 {
					p.TimeMs = tree[0].TimeMs
				}
			}
		} else if len(p.Nodes) > 0 && r.Driver == "mariadb" && r.Explain.JSON != "" {
			// MariaDB's JSON marks sorts per table more precisely.
			if nodes, _ := mariaAnalyzeJSON(r.Explain.JSON); len(nodes) == len(p.Nodes) {
				for i := range nodes {
					p.Nodes[i].Filesort = p.Nodes[i].Filesort || nodes[i].Filesort
				}
			}
		}
	case "pgsql":
		nodes, cost, ms, err := pgPlan(r.Explain.JSON)
		if err != nil {
			return p, err
		}
		p.Nodes, p.Cost, p.TimeMs, p.Analyzed = nodes, cost, ms, analyze
		p.Raw, p.RawKind = r.Explain.JSON, "json"
	case "sqlite":
		p.Nodes = sqlitePlan(r.Explain.Rows)
		var lines []string
		for _, row := range r.Explain.Rows {
			lines = append(lines, fmt.Sprintf("%s|%s|%s", toStr(row["id"]), toStr(row["parent"]), toStr(row["detail"])))
		}
		p.Raw, p.RawKind = "id|parent|detail\n"+strings.Join(lines, "\n"), "text"
	case "sqlsrv":
		nodes, missing, warns, cost, err := mssqlPlan(r.Explain.XML)
		if err != nil {
			return p, err
		}
		p.Nodes, p.Missing, p.Cost = nodes, missing, cost
		p.Warnings = append(p.Warnings, warns...)
		p.Raw, p.RawKind = r.Explain.XML, "xml"
	default:
		return p, fmt.Errorf("unknown driver %q", r.Driver)
	}
	if p.Nodes == nil {
		p.Nodes = []*Node{}
	}
	return p, nil
}

// mergeActual copies MySQL EXPLAIN ANALYZE's measured rows onto the
// tabular nodes (matched by table).
func mergeActual(tab []*Node, tree []*Node) {
	var walk func(ns []*Node, parent *Node)
	walk = func(ns []*Node, parent *Node) {
		for _, n := range ns {
			if n.Table != "" && (n.FullScan || n.IndexScan || n.Index != "") {
				for _, t := range tab {
					if strings.EqualFold(t.Table, n.Table) && t.ActualRows < 0 {
						t.ActualRows, t.Loops = n.ActualRows, n.Loops
						// A Filter above the scan: what it kept.
						if parent != nil && strings.HasPrefix(strings.ToLower(parent.Op), "filter") && parent.ActualRows >= 0 {
							t.Removed = (n.ActualRows - parent.ActualRows) * maxf(n.Loops, 1)
						}
						break
					}
				}
			}
			walk(n.Children, n)
		}
	}
	walk(tree, nil)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// scrub removes the password from a message.
func scrub(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "********")
}

const redactedValue = "[redacted]"

// neutralBindings swaps the watcher's "[redacted]" stand-ins for a value
// of a plausible type (an id-like column gets 1, anything else 'x').
func neutralBindings(sql string, b []any) ([]any, int) {
	n := 0
	out := make([]any, len(b))
	var cols []string
	for i, v := range b {
		out[i] = v
		if s, ok := v.(string); ok && s == redactedValue {
			if cols == nil {
				cols = placeholderCols(sql)
			}
			col := ""
			if i < len(cols) {
				col = strings.ToLower(cols[i])
			}
			if col == "id" || strings.HasSuffix(col, "_id") {
				out[i] = 1
			} else {
				out[i] = "x"
			}
			n++
		}
	}
	return out, n
}

// placeholderCols maps each placeholder to the column before it.
func placeholderCols(sql string) []string {
	ts := tokenize(sql)
	var out []string
	for i, t := range ts {
		if t.kind != 'p' {
			continue
		}
		col := ""
		for j := i - 1; j >= 0 && j >= i-3; j-- {
			if ts[j].kind == 'w' && !isOpWord(ts[j].low) || ts[j].kind == 'q' {
				col = ts[j].text
				break
			}
		}
		out = append(out, col)
	}
	return out
}
