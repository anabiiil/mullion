package explain

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The end-to-end test runs the real PHP script against real databases.
// It is skipped unless MULLION_TEST_PHP names a PHP binary (with the
// sqlite3 or pdo_sqlite extension). It always builds a throwaway SQLite
// database; MULLION_TEST_MYSQL / MULLION_TEST_PGSQL ("host:port/db/user")
// add a scratch MySQL / PostgreSQL server (never point them at a real
// one: the test only reads, but it expects its own tables users/posts).
// MULLION_TEST_DUMP=<dir> saves each raw script output there.

type e2eCase struct {
	name     string
	sql      string
	bindings []any
	analyze  bool
	nplus1   int
	want     []string // rules expected among the findings
	wantSugg string   // expected first suggestion SQL substring ("" = none checked)
}

var e2eCases = []e2eCase{
	{name: "fk-no-index", sql: `select * from "posts" where "posts"."user_id" = ? and "posts"."user_id" is not null`, bindings: []any{42.0}, nplus1: 12,
		want: []string{"full-scan", "n-plus-one"}, wantSugg: "(user_id)"},
	{name: "email-indexed", sql: `select * from "users" where "email" = ? limit 1`, bindings: []any{"user7@example.test"}},
	{name: "leading-like", sql: `select * from "users" where "name" like ?`, bindings: []any{"%ser 12%"}, want: []string{"leading-wildcard"}},
	{name: "order-filesort", sql: `select * from "posts" where "published" = ? order by "created_at" desc limit 20`, bindings: []any{true}, want: []string{"filesort"}},
	{name: "date-func", sql: `select count(*) as aggregate from "users" where date("created_at") = ?`, bindings: []any{"2026-10-01"}, want: []string{"function-on-column"}},
	{name: "deep-offset", sql: `select * from "posts" order by "id" asc limit 20 offset 40000`},
	{name: "status-low-selectivity", sql: `select * from "users" where "status" = ?`, bindings: []any{"active"}},
	{name: "dependent", sql: `select "users".*, (select count(*) from "posts" where "users"."id" = "posts"."user_id") as "posts_count" from "users" where "status" = ? limit 50`, bindings: []any{"banned"}},
	{name: "analyze-fk", sql: `select * from "posts" where "user_id" = ?`, bindings: []any{42.0}, analyze: true},
	{name: "update", sql: `update "posts" set "title" = ? where "user_id" = ?`, bindings: []any{"x", 3.0}, want: []string{"full-scan"}},
}

// forEngine rewrites the test SQL's "quoted" identifiers for MySQL.
func forEngine(driver, sql string) string {
	if driver == "mysql" || driver == "mariadb" {
		sql = strings.ReplaceAll(sql, `"`, "`")
		sql = strings.ReplaceAll(sql, "date(`created_at`)", "date(`created_at`)")
	}
	return sql
}

func TestExplainEndToEnd(t *testing.T) {
	php := os.Getenv("MULLION_TEST_PHP")
	if php == "" {
		t.Skip("set MULLION_TEST_PHP to a PHP binary to run the explain end-to-end test")
	}
	dir := t.TempDir()
	script, err := EnsureScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	ini := filepath.Join(filepath.Dir(php), "php.ini")
	run := PHPRunner(php, ini, script)
	dump := os.Getenv("MULLION_TEST_DUMP")
	current := ""
	if dump != "" {
		inner := run
		run = func(ctx context.Context, req []byte) ([]byte, error) {
			out, err := inner(ctx, req)
			name := strings.NewReplacer("/", "_", " ", "_").Replace(current)
			_ = os.WriteFile(filepath.Join(dump, name+".json"), out, 0o644)
			return out, err
		}
	}

	conns := map[string]Conn{}
	// A throwaway SQLite database, built with the same PHP.
	db := filepath.Join(dir, "app.sqlite")
	build := `$d = new SQLite3($argv[1]);
$d->exec('CREATE TABLE users (id integer primary key autoincrement, name varchar, email varchar not null, status varchar not null, bio text, created_at datetime)');
$d->exec('CREATE UNIQUE INDEX users_email_unique ON users (email)');
$d->exec("WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 5000) INSERT INTO users (name,email,status,bio,created_at) SELECT 'User '||n, 'user'||n||'@example.test', CASE WHEN n%10=0 THEN 'banned' ELSE 'active' END, 'bio', datetime('now') FROM seq");
$d->exec('CREATE TABLE posts (id integer primary key autoincrement, user_id integer not null, title varchar, body text, published tinyint, created_at datetime)');
$d->exec("WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 20000) INSERT INTO posts (user_id,title,body,published,created_at) SELECT 1 + (n*7919) % 5000, 'Post '||n, 'lorem', n%2, datetime('now') FROM seq");`
	if out, err := exec.Command(php, "-r", build, db).CombinedOutput(); err != nil {
		t.Fatalf("building the SQLite database: %v %s", err, out)
	}
	conns["sqlite"] = Conn{Name: "sqlite", Driver: "sqlite", Database: db}
	for driver, env := range map[string]string{"mysql": os.Getenv("MULLION_TEST_MYSQL"), "pgsql": os.Getenv("MULLION_TEST_PGSQL")} {
		if env == "" {
			continue
		}
		parts := strings.Split(env, "/")
		if len(parts) < 3 {
			t.Fatalf("MULLION_TEST_%s: want host:port/db/user", strings.ToUpper(driver))
		}
		host, port, _ := strings.Cut(parts[0], ":")
		p, _ := strconv.Atoi(port)
		if p == 3306 || p == 5432 || p == 1433 {
			t.Fatalf("refusing to test against a default port (%d) — start a scratch server elsewhere", p)
		}
		conns[driver] = Conn{Name: driver, Driver: driver, Host: host, Port: p, Database: parts[1], Username: parts[2]}
	}

	for driver, c := range conns {
		for _, tc := range e2eCases {
			t.Run(driver+"/"+tc.name, func(t *testing.T) {
				current = driver + "-" + tc.name
				in := Input{SQL: forEngine(driver, tc.sql), Bindings: tc.bindings, Analyze: tc.analyze, NPlusOne: tc.nplus1}
				res, err := Explain(context.Background(), run, c, in)
				if tc.analyze && driver == "sqlite" {
					if err == nil {
						t.Fatal("sqlite analyze should be refused")
					}
					return
				}
				if err != nil {
					t.Fatalf("explain: %v", err)
				}
				if dump != "" {
					b, _ := json.MarshalIndent(res, "", "  ")
					_ = os.WriteFile(filepath.Join(dump, current+".result.json"), b, 0o644)
				}
				var rules []string
				for _, f := range res.Findings {
					rules = append(rules, f.Rule+"/"+f.Severity)
				}
				var sugg []string
				for _, s := range res.Suggestions {
					sugg = append(sugg, s.SQL)
				}
				t.Logf("%s %s via %s — %d nodes, findings %v, badges %v, suggestions %v", res.Engine, res.Server, res.Via, len(res.Plan.Nodes), rules, res.Badges, sugg)
				for _, w := range tc.want {
					found := false
					for _, f := range res.Findings {
						if f.Rule == w {
							found = true
						}
					}
					if !found {
						t.Errorf("want a %s finding, got %v", w, rules)
					}
				}
				if tc.wantSugg != "" && (len(sugg) == 0 || !strings.Contains(sugg[0], tc.wantSugg)) {
					t.Errorf("want a suggestion with %s, got %v", tc.wantSugg, sugg)
				}
				if tc.analyze && !res.Plan.Analyzed {
					t.Errorf("analyze: the plan isn't marked analyzed")
				}
			})
		}
		t.Run(driver+"/refuses-ddl", func(t *testing.T) {
			if _, err := Explain(context.Background(), run, c, Input{SQL: "drop table users"}); err == nil {
				t.Fatal("DDL must be refused")
			}
			if _, err := Explain(context.Background(), run, c, Input{SQL: forEngine(driver, `delete from "posts" where "id" = 1`), Analyze: true}); err != ErrAnalyzeSelect {
				t.Fatalf("analyze of a DELETE must be refused, got %v", err)
			}
		})
	}
}
