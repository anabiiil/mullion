package explain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fixtures in testdata/ are the PHP script's output: the mysql-*,
// pgsql-* and sqlite-* ones recorded from MySQL 8.4, PostgreSQL 17 and
// SQLite 3.45 (scratch servers), the mariadb-*, pgsql16-*, mssql-* and
// sqlite-auto-index ones written from those servers' documented output
// formats (MariaDB 11.4 ANALYZE FORMAT=JSON, PostgreSQL 16 EXPLAIN
// ANALYZE JSON, SQL Server 2022 SHOWPLAN_XML).

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureSQL(t *testing.T, raw []byte) string {
	var r struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r.SQL
}

func rulesOf(res *Result) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range res.Findings {
		if _, ok := out[f.Rule]; !ok {
			out[f.Rule] = f
		}
	}
	return out
}

func TestFixtures(t *testing.T) {
	cases := []struct {
		name     string
		analyze  bool
		nplus1   int
		want     map[string]string // rule → severity ("" = any)
		notWant  []string
		sugg     []string // suggestion SQL, in order
		check    func(t *testing.T, res *Result)
		badgeHas string
	}{
		{name: "mysql-fk-no-index", nplus1: 12,
			want: map[string]string{"full-scan": "high", "n-plus-one": "high"},
			sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}, badgeHas: "no index",
			check: func(t *testing.T, res *Result) {
				f := rulesOf(res)["n-plus-one"]
				if !strings.Contains(f.Fix, "->with('posts')") || f.Suggest != 0 {
					t.Errorf("N+1 fix should eager-load posts and point at the index: %+v", f)
				}
				if res.Suggestions[0].Migration != "Schema::table('posts', function (Blueprint $table) {\n    $table->index('user_id');\n});" {
					t.Errorf("migration = %q", res.Suggestions[0].Migration)
				}
				n := res.Plan.Nodes[0]
				if !n.FullScan || n.Table != "posts" || n.Rows != 196977 || n.Filtered != 10 {
					t.Errorf("tabular node = %+v", n)
				}
			}},
		{name: "mysql-analyze-fk", analyze: true,
			want: map[string]string{"full-scan": "high"}, notWant: []string{"rows-examined"},
			sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"},
			check: func(t *testing.T, res *Result) {
				if !res.Plan.Analyzed || res.Plan.RawAnalyze == "" {
					t.Fatal("not analyzed")
				}
				if !strings.Contains(rulesOf(res)["full-scan"].Why, "Measured: 200k rows") {
					t.Errorf("the scan should carry the measurement: %s", rulesOf(res)["full-scan"].Why)
				}
				tab := res.Plan.Nodes[0]
				if tab.ActualRows != 200000 || tab.Removed != 199996 {
					t.Errorf("merged actual rows = %v removed %v", tab.ActualRows, tab.Removed)
				}
			}},
		{name: "mysql-order-filesort", want: map[string]string{"full-scan": "high", "filesort": "medium"},
			sugg: []string{"CREATE INDEX idx_posts_published_created_at ON posts (published, created_at);"}},
		{name: "mysql-dependent", want: map[string]string{"dependent-subquery": "medium", "full-scan": "high"}, notWant: []string{"server-warning"},
			sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}},
		{name: "mysql-status-low-selectivity", want: map[string]string{"low-selectivity": "medium"}, notWant: []string{"full-scan"}, sugg: []string{}},
		{name: "mysql-update", want: map[string]string{"full-scan": "high"}, sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}},
		{name: "pgsql-fk-no-index", want: map[string]string{"full-scan": "high"}, sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}},
		{name: "pgsql-analyze-fk", analyze: true, want: map[string]string{"full-scan": "high"},
			check: func(t *testing.T, res *Result) {
				if !res.Plan.Analyzed || res.Plan.TimeMs <= 0 {
					t.Errorf("analyzed=%v time=%v", res.Plan.Analyzed, res.Plan.TimeMs)
				}
				if !strings.Contains(rulesOf(res)["full-scan"].Why, "Measured") {
					t.Errorf("no measurement: %s", rulesOf(res)["full-scan"].Why)
				}
			}},
		{name: "pgsql-deep-offset", want: map[string]string{"deep-offset": "high"}, notWant: []string{"low-selectivity", "full-scan"}, sugg: []string{}},
		{name: "pgsql-dependent", want: map[string]string{"dependent-subquery": "medium"}, sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}},
		{name: "pgsql-order-filesort", want: map[string]string{"filesort": "medium", "full-scan": "high"},
			sugg: []string{"CREATE INDEX idx_posts_published_created_at ON posts (published, created_at);"}},
		{name: "pgsql16-analyze-join-spill", analyze: true,
			want: map[string]string{"disk-sort": "high", "disk-hash": "medium", "full-scan": "high", "filesort": ""},
			sugg: []string{"CREATE INDEX idx_orders_status_total ON orders (status, total);"},
			check: func(t *testing.T, res *Result) {
				var cust *Finding
				for i, f := range res.Findings {
					if f.Rule == "full-scan" && f.Table == "customers" {
						cust = &res.Findings[i]
					}
				}
				if cust == nil || cust.Suggest != -1 || !strings.Contains(cust.Fix, "hash join") || cust.Severity != "medium" {
					t.Errorf("customers is joined on its primary key: %+v", cust)
				}
				if res.Plan.TimeMs != 1003.77 {
					t.Errorf("execution time = %v", res.Plan.TimeMs)
				}
			}},
		{name: "mariadb-analyze-filesort", analyze: true,
			want: map[string]string{"full-scan": "medium", "filesort": "medium"},
			sugg: []string{"CREATE INDEX idx_orders_customer_email_created_at ON orders (customer_email, created_at);"},
			check: func(t *testing.T, res *Result) {
				n := res.Plan.Nodes[0]
				if n.ActualRows != 100000 || n.Filtered != 0.003 || !n.Filesort || res.Plan.TimeMs != 61.214 {
					t.Errorf("node = %+v, time %v", n, res.Plan.TimeMs)
				}
				if !strings.Contains(rulesOf(res)["full-scan"].Why, "Measured: 100k rows") {
					t.Errorf("why = %s", rulesOf(res)["full-scan"].Why)
				}
			}},
		{name: "mssql-scan-missing-index",
			want: map[string]string{"missing-index": "high", "implicit-conversion": "medium", "full-scan": "high"},
			sugg: []string{"CREATE INDEX idx_users_email ON users (email) INCLUDE (name, phone);"},
			check: func(t *testing.T, res *Result) {
				n := 0
				for _, f := range res.Findings {
					if f.Rule == "implicit-conversion" {
						n++
					}
				}
				if n != 1 {
					t.Errorf("one conversion finding, got %d", n)
				}
				s := res.Suggestions[0]
				if s.Source != "server" || s.Impact < 98 || !strings.Contains(s.Migration, "INCLUDE (name, phone)") {
					t.Errorf("server suggestion = %+v", s)
				}
				if res.Plan.Nodes[0].Op != "Clustered Index Scan" || res.Plan.Nodes[0].Index != "PK_users" {
					t.Errorf("node = %+v", res.Plan.Nodes[0])
				}
			}},
		{name: "mssql-seek-lookup-sort",
			want: map[string]string{"disk-sort": "high", "filesort": "medium", "key-lookup": "low"},
			sugg: []string{"CREATE INDEX idx_orders_status_created_at ON orders (status, created_at);"},
			check: func(t *testing.T, res *Result) {
				if res.Suggestions[0].Extends != "IX_orders_status" {
					t.Errorf("should extend IX_orders_status: %+v", res.Suggestions[0])
				}
				sort := res.Plan.Nodes[0]
				if sort.Op != "Sort" || len(sort.Children) != 1 || len(sort.Children[0].Children) != 2 || !sort.Children[0].Children[1].Lookup {
					t.Errorf("tree = %+v", sort)
				}
			}},
		{name: "sqlite-fk-no-index", want: map[string]string{"full-scan": "medium"}, sugg: []string{"CREATE INDEX idx_posts_user_id ON posts (user_id);"}},
		{name: "sqlite-order-filesort", want: map[string]string{"filesort": "", "full-scan": ""},
			sugg: []string{"CREATE INDEX idx_posts_published_created_at ON posts (published, created_at);"}},
		{name: "sqlite-dependent", want: map[string]string{"dependent-subquery": "medium"}},
		{name: "sqlite-auto-index", want: map[string]string{"auto-index": "medium", "full-scan": "medium"},
			sugg: []string{"CREATE INDEX idx_posts_published ON posts (published);", "CREATE INDEX idx_comments_post_id ON comments (post_id);"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := loadFixture(t, tc.name)
			sql := fixtureSQL(t, raw)
			in := Input{SQL: sql, Analyze: tc.analyze, NPlusOne: tc.nplus1}
			res, err := Build(raw, Conn{Driver: "x"}, in, AnalyzeSQL(sql, nil))
			if err != nil {
				t.Fatal(err)
			}
			got := rulesOf(res)
			for rule, sev := range tc.want {
				f, ok := got[rule]
				if !ok {
					t.Errorf("missing %s finding; got %v", rule, keys(got))
					continue
				}
				if sev != "" && f.Severity != sev {
					t.Errorf("%s severity = %s, want %s (%s)", rule, f.Severity, sev, f.Why)
				}
				if f.Why == "" || f.Title == "" || f.Fix == "" && rule != "server-warning" {
					t.Errorf("%s is missing text: %+v", rule, f)
				}
			}
			for _, rule := range tc.notWant {
				if _, ok := got[rule]; ok {
					t.Errorf("unexpected %s finding: %+v", rule, got[rule])
				}
			}
			if tc.sugg != nil {
				var sq []string
				for _, s := range res.Suggestions {
					sq = append(sq, s.SQL)
				}
				if len(sq) != len(tc.sugg) || len(sq) > 0 && !reflect.DeepEqual(sq, tc.sugg) {
					t.Errorf("suggestions = %q, want %q", sq, tc.sugg)
				}
			}
			for _, f := range res.Findings {
				if f.Suggest >= len(res.Suggestions) {
					t.Errorf("%s points at suggestion %d of %d", f.Rule, f.Suggest, len(res.Suggestions))
				}
			}
			if tc.badgeHas != "" && !contains(res.Badges, tc.badgeHas) {
				t.Errorf("badges = %v, want %q", res.Badges, tc.badgeHas)
			}
			if res.Plan.Raw == "" {
				t.Error("raw plan missing")
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

func keys(m map[string]Finding) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestAnalyzeSQL(t *testing.T) {
	info := AnalyzeSQL("select `u`.*, `p`.`title` from `users` as `u` inner join `posts` `p` on `p`.`user_id` = `u`.`id` "+
		"where `u`.`status` = ? and `u`.`created_at` >= ? and lower(`u`.`email`) = ? and (`u`.`name` like ? or `u`.`phone` = ?) "+
		"order by `u`.`created_at` desc limit 20 offset 4000", []any{"active", "2026-01-01", "a@b", "%x%", 5.0})
	if info.Verb != "select" || !info.Star || info.Offset != 4000 || !info.HasLim {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Tables) != 2 || info.Tables[0] != (TableRef{Name: "users", Alias: "u", Main: true, depth: 0}) || info.Tables[1].Name != "posts" || info.Tables[1].Alias != "p" {
		t.Fatalf("tables = %+v", info.Tables)
	}
	byCol := map[string]Pred{}
	for _, p := range info.Where {
		byCol[p.Col] = p
	}
	if p := byCol["status"]; p.Op != "eq" || p.Table != "users" || p.Value != "active" {
		t.Errorf("status = %+v", p)
	}
	if p := byCol["created_at"]; p.Op != "range" {
		t.Errorf("created_at = %+v", p)
	}
	if p := byCol["email"]; p.Func != "lower" {
		t.Errorf("email = %+v", p)
	}
	if p := byCol["name"]; p.Op != "like" || !p.InOr {
		t.Errorf("name = %+v", p)
	}
	if p := byCol["phone"]; p.ValueKind != "number" || !p.InOr {
		t.Errorf("phone = %+v", p)
	}
	if len(info.OrCols) != 1 || !reflect.DeepEqual(info.OrCols[0], []string{"users.name", "users.phone"}) {
		t.Errorf("or = %v", info.OrCols)
	}
	if len(info.Joins) != 2 || info.Joins[0].Table != "posts" || info.Joins[0].Col != "user_id" || info.Joins[1].Table != "users" || info.Joins[1].Col != "id" {
		t.Errorf("joins = %+v", info.Joins)
	}
	if !reflect.DeepEqual(info.OrderBy, []ColRef{{Table: "users", Col: "created_at", Desc: true}}) {
		t.Errorf("order = %+v", info.OrderBy)
	}
	// BETWEEN's AND doesn't split; IN lists and IS NULL are equality.
	info = AnalyzeSQL(`select * from "t" where "a" between ? and ? and "b" in (?, ?, ?) and "c" is null and date("d") = ?`, []any{1.0, 2.0, 3.0, 4.0, 5.0, "x"})
	var ops []string
	for _, p := range info.Where {
		ops = append(ops, p.Col+":"+p.Op+":"+p.Func)
	}
	if !reflect.DeepEqual(ops, []string{"a:range:", "b:eq:", "c:eq:", "d:eq:date"}) {
		t.Errorf("ops = %v", ops)
	}
	// MySQL's LIMIT offset, count; ORDER BY RAND(); placeholders inside strings don't count.
	info = AnalyzeSQL("select * from `t` where `x` = '?' and `y` = ? order by rand() limit ?, 10", []any{"v", 25000.0})
	if !info.Random || info.Offset != 25000 || len(info.Where) != 2 || info.Where[1].Value != "v" {
		t.Errorf("info = %+v", info)
	}
	if AnalyzeSQL("delete from `sessions`", nil).NoWhere != true || AnalyzeSQL("delete from `sessions` where `id` = ?", nil).NoWhere {
		t.Error("NoWhere")
	}
	// A correlated subquery's reference to the outer table isn't a filter on it.
	info = AnalyzeSQL(`select "users".*, (select count(*) from "posts" where "users"."id" = "posts"."user_id") as "c" from "users"`, nil)
	for _, p := range info.Where {
		if p.Table == "users" {
			t.Errorf("outer reference read as a filter: %+v", p)
		}
	}
	if len(info.Where) != 1 || info.Where[0].Table != "posts" || info.Where[0].Col != "user_id" || !info.Where[0].Sub {
		t.Errorf("subquery where = %+v", info.Where)
	}
}

func TestExplainable(t *testing.T) {
	for sql, want := range map[string]string{
		"select 1": "select:true",
		"(select * from a) union (select * from b)":             "select:true",
		"with x as (select 1) select * from x":                  "with:true",
		"with d as (delete from a returning *) select * from d": "with:false",
		"select * from a for update":                            "select:true",
		"select * into outfile '/tmp/x' from a":                 "select:false",
		"select replace(name, 'a', 'b') from users":             "select:true",
		"update a set b = 1":                                    "update:false",
		"delete from a where id = 1":                            "delete:false",
		"insert into a (x) select x from b":                     "insert:false",
		"insert into a (x) values (1)":                          "error",
		"drop table a":                                          "error",
		"select 1; drop table a":                                "error",
		"select 1;":                                             "select:true",
		"truncate a":                                            "error",
		"   ":                                                   "error",
		"create index i on t (a)":                               "error",
		"alter table a add column b int":                        "error",
		"set names utf8":                                        "error",
	} {
		verb, ok, err := Explainable(sql)
		got := verb + ":" + map[bool]string{true: "true", false: "false"}[ok]
		if err != nil {
			got = "error"
		}
		if got != want {
			t.Errorf("Explainable(%q) = %s (%v), want %s", sql, got, err, want)
		}
	}
}

func TestIndexSuggestionHeuristicAndCoverage(t *testing.T) {
	info := AnalyzeSQL(`select * from "orders" where "status" = ? and "created_at" > ? and "customer_id" = ? order by "total"`, []any{"paid", "2026-01-01", 5.0})
	cols := IndexColumns("orders", info, purpose{filter: true, order: true})
	// Equality first (in order), then the range; ORDER BY can't follow a range.
	if !reflect.DeepEqual(cols, []string{"status", "customer_id", "created_at"}) {
		t.Fatalf("cols = %v", cols)
	}
	info = AnalyzeSQL(`select * from "orders" where "status" = ? order by "total" desc`, []any{"paid"})
	if cols := IndexColumns("orders", info, purpose{filter: true, order: true}); !reflect.DeepEqual(cols, []string{"status", "total"}) {
		t.Fatalf("cols = %v", cols)
	}
	// Covered: equality columns in any order, the rest in order.
	idx := []Index{{Name: "orders_customer_status", Columns: []string{"customer_id", "status", "created_at", "id"}}}
	if by := coveredBy([]string{"status", "customer_id", "created_at"}, 2, idx); by != "orders_customer_status" {
		t.Errorf("should be covered, got %q", by)
	}
	if by := coveredBy([]string{"status", "created_at", "customer_id"}, 1, idx); by != "" {
		t.Errorf("a different order after the equality columns isn't covered, got %q", by)
	}
	if by := coveredBy([]string{"status", "customer_id", "total"}, 2, []Index{{Name: "short", Columns: []string{"status", "customer_id"}}}); by != "" {
		t.Errorf("a shorter index doesn't cover, got %q", by)
	}

	// End to end through Diagnose: an existing index that covers the
	// suggestion means no suggestion, and the fix says why.
	res := &Result{Engine: "mysql", Tables: map[string]*Table{"posts": {Rows: f64(50000), Indexes: []Index{{Name: "posts_user_id_index", Columns: []string{"user_id"}}}}},
		Plan: Plan{Nodes: []*Node{{Op: "ALL", Table: "posts", FullScan: true, Rows: 50000, ActualRows: -1}}}}
	Diagnose(res, AnalyzeSQL("select * from `posts` where `user_id` = ?", []any{3.0}), Input{})
	if len(res.Suggestions) != 0 {
		t.Fatalf("covered, but suggested %+v", res.Suggestions)
	}
	if f := rulesOf(res)["full-scan"]; !strings.Contains(f.Fix, "posts_user_id_index") || !strings.Contains(f.Fix, "ANALYZE TABLE posts") {
		t.Errorf("fix = %s", f.Fix)
	}
	// Not covered: suggested, with the Laravel migration.
	res = &Result{Engine: "pgsql", Tables: map[string]*Table{"posts": {Rows: f64(50000), Indexes: []Index{{Name: "posts_user_id_index", Columns: []string{"user_id"}}}}},
		Plan: Plan{Nodes: []*Node{{Op: "Seq Scan", Table: "posts", FullScan: true, Rows: 50000, ActualRows: -1}}}}
	Diagnose(res, AnalyzeSQL(`select * from "posts" where "user_id" = ? and "published" = ? order by "created_at" desc`, []any{3.0, true}), Input{})
	if len(res.Suggestions) != 1 {
		t.Fatalf("suggestions = %+v", res.Suggestions)
	}
	s := res.Suggestions[0]
	if s.SQL != "CREATE INDEX idx_posts_user_id_published ON posts (user_id, published);" || s.Extends != "posts_user_id_index" {
		t.Errorf("suggestion = %+v", s)
	}
	if s.Migration != "Schema::table('posts', function (Blueprint $table) {\n    $table->index(['user_id', 'published']);\n});" {
		t.Errorf("migration = %q", s.Migration)
	}
}

func f64(v float64) *float64 { return &v }

func TestIndexNameAndQuoting(t *testing.T) {
	if n := IndexName("public.Order Items", []string{"userId", "createdAt"}); n != "idx_order_items_userid_createdat" {
		t.Errorf("name = %s", n)
	}
	long := IndexName("a_really_long_table_name_for_testing", []string{"first_column_name", "second_column_name", "third"})
	if len(long) > 60 || !strings.HasPrefix(long, "idx_a_really_long") {
		t.Errorf("long name = %s (%d)", long, len(long))
	}
	s := newSuggestion("mysql", "orders", []string{"order", "status"}, nil, "", "heuristic")
	if s.SQL != "CREATE INDEX idx_orders_order_status ON orders (`order`, status);" {
		t.Errorf("sql = %s", s.SQL)
	}
	if s := newSuggestion("sqlsrv", "dbo.Users", []string{"Email"}, []string{"Name"}, "", "server"); s.SQL != "CREATE INDEX idx_users_email ON dbo.[Users] ([Email]) INCLUDE ([Name]);" {
		t.Errorf("sql = %s", s.SQL)
	}
}

func TestSQLRules(t *testing.T) {
	cases := []struct {
		engine, sql string
		bindings    []any
		cols        map[string]string
		rule, sev   string
	}{
		{"mysql", "select * from `users` where `name` like ?", []any{"%bob%"}, nil, "leading-wildcard", "medium"},
		{"pgsql", `select * from "users" where "name" like ?`, []any{"bob%"}, nil, "", ""},
		{"mysql", "select count(*) as aggregate from `orders` where year(`created_at`) = ?", []any{2026.0}, nil, "function-on-column", "medium"},
		{"pgsql", `select * from "users" where lower("email") = ?`, []any{"a@b"}, nil, "function-on-column", "medium"},
		{"mysql", "select * from `users` where `phone` = ?", []any{5551234.0}, map[string]string{"phone": "varchar"}, "implicit-conversion", "medium"},
		{"mysql", "select * from `users` where `id` = ?", []any{"5"}, map[string]string{"id": "bigint"}, "", ""},
		{"mysql", "select * from `users` where `email` = ? or `phone` = ?", []any{"a", "b"}, nil, "or-columns", "medium"},
		{"mysql", "select * from `users` where (`status` = ? or `status` = ?)", []any{"a", "b"}, nil, "", ""},
		{"mysql", "select * from `users` order by rand() limit 5", nil, nil, "order-random", "high"},
		{"pgsql", `select * from "users" order by "id" limit 20 offset 20000`, nil, nil, "deep-offset", "high"},
		{"pgsql", `select * from "users" order by "id" limit 20 offset 2000`, nil, nil, "deep-offset", "medium"},
		{"mysql", "delete from `sessions`", nil, nil, "no-where", "high"},
		{"mysql", "select * from `users` where `id` = ?", []any{1.0}, map[string]string{"id": "bigint", "bio": "text"}, "select-star", "low"},
	}
	for _, tc := range cases {
		res := &Result{Engine: tc.engine, Tables: map[string]*Table{}}
		info := AnalyzeSQL(tc.sql, tc.bindings)
		tbl := info.Tables[0].Name
		res.Tables[tbl] = &Table{Rows: f64(5000), Columns: tc.cols}
		Diagnose(res, info, Input{})
		got := rulesOf(res)
		if tc.rule == "" {
			if len(got) != 0 {
				t.Errorf("%s: want no findings, got %v", tc.sql, keys(got))
			}
			continue
		}
		f, ok := got[tc.rule]
		if !ok || f.Severity != tc.sev {
			t.Errorf("%s: want %s/%s, got %+v", tc.sql, tc.rule, tc.sev, res.Findings)
		}
	}
}

func TestGuessRelation(t *testing.T) {
	for sql, want := range map[string]Relation{
		`select * from "posts" where "posts"."user_id" = ? and "posts"."user_id" is not null`:                                                                                    {Name: "posts", Kind: "hasMany", Key: "user_id"},
		`select * from "post_comments" where "post_comments"."post_id" in (1, 2, 3)`:                                                                                             {Name: "postComments", Kind: "hasMany", Key: "post_id"},
		`select * from "users" where "users"."id" = ? limit 1`:                                                                                                                   {Name: "user", Kind: "belongsTo", Key: "id"},
		`select * from "categories" where "categories"."id" in (1, 2)`:                                                                                                           {Name: "category", Kind: "belongsTo", Key: "id"},
		`select * from "images" where "images"."imageable_type" = ? and "images"."imageable_id" = ?`:                                                                             {Name: "images", Kind: "morphMany", Key: "imageable_id"},
		`select "roles".*, "role_user"."user_id" as "pivot_user_id" from "roles" inner join "role_user" on "roles"."id" = "role_user"."role_id" where "role_user"."user_id" = ?`: {Name: "roles", Kind: "belongsToMany", Key: "user_id"},
	} {
		got, ok := GuessRelation(AnalyzeSQL(sql, []any{"App\\Models\\Post", 1.0}))
		if !ok || got != want {
			t.Errorf("%s\n  = %+v, want %+v", sql, got, want)
		}
	}
	if _, ok := GuessRelation(AnalyzeSQL(`select * from "users" where "email" = ?`, nil)); ok {
		t.Error("an email lookup isn't a relation")
	}
}

func TestMySQLAnalyzeTree(t *testing.T) {
	text := "-> Limit: 20 row(s)  (cost=20475 rows=20) (actual time=45.1..45.1 rows=20 loops=1)\n" +
		"    -> Sort: posts.created_at DESC, limit input to 20 row(s) per chunk  (cost=20475 rows=196977) (actual time=45.1..45.1 rows=20 loops=1)\n" +
		"        -> Filter: (posts.published = 1)  (cost=20475 rows=19698) (actual time=0.05..38.2 rows=100000 loops=1)\n" +
		"            -> Table scan on posts  (cost=20475 rows=196977) (actual time=0.04..30.1 rows=200000 loops=1)\n"
	tree := mysqlAnalyzeTree(text)
	if len(tree) != 1 || len(tree[0].Children) != 1 {
		t.Fatalf("tree = %+v", tree)
	}
	sort := tree[0].Children[0]
	scan := sort.Children[0].Children[0]
	if !sort.Filesort || !scan.FullScan || scan.Table != "posts" || scan.ActualRows != 200000 || scan.Rows != 196977 || tree[0].TimeMs != 45.1 {
		t.Errorf("sort=%+v scan=%+v", sort, scan)
	}
	tab := []*Node{{Op: "ALL", Table: "posts", FullScan: true, ActualRows: -1}}
	mergeActual(tab, tree)
	if tab[0].ActualRows != 200000 || tab[0].Removed != 100000 {
		t.Errorf("merged = %+v", tab[0])
	}
}

func TestConnFromEnv(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"DB_CONNECTION": "mysql", "DB_HOST": "127.0.0.1", "DB_PORT": "3307", "DB_DATABASE": "shop", "DB_USERNAME": "root", "DB_PASSWORD": "s3cret"}
	c, notes, err := ConnFromEnv(env, dir, "")
	if err != nil || c.Driver != "mysql" || c.Port != 3307 || c.Database != "shop" || c.Password != "s3cret" || len(notes) != 0 {
		t.Fatalf("c = %+v notes %v err %v", c, notes, err)
	}
	if strings.Contains(c.Label(), "s3cret") {
		t.Error("label shows the password")
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), "s3cret") {
		t.Error("JSON shows the password")
	}
	// The query ran on another stock connection: same DB_* settings, with a note.
	c, notes, err = ConnFromEnv(env, dir, "pgsql")
	if err != nil || c.Driver != "pgsql" || len(notes) != 1 || !strings.Contains(notes[0], "DB_CONNECTION=mysql") {
		t.Fatalf("pgsql: %+v %v %v", c, notes, err)
	}
	// A custom connection name: unknown without config/database.php…
	if _, _, err := ConnFromEnv(env, dir, "tenant"); err == nil {
		t.Fatal("unknown connection should fail")
	}
	// …known with it.
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "database.php"), []byte("<?php return ['connections' => [\n 'tenant' => [\n  'url' => env('DB_URL'),\n  'driver' => 'pgsql',\n  'host' => env('DB_HOST'),\n ],\n]];"), 0o644)
	c, notes, err = ConnFromEnv(env, dir, "tenant")
	if err != nil || c.Driver != "pgsql" || c.Port != 3307 || len(notes) != 1 {
		t.Fatalf("tenant: %+v %v %v", c, notes, err)
	}
	// DB_URL wins.
	c, _, _ = ConnFromEnv(map[string]string{"DB_CONNECTION": "pgsql", "DB_URL": "postgresql://app:pw%40x@db.test:6543/shop"}, dir, "")
	if c.Driver != "pgsql" || c.Host != "db.test" || c.Port != 6543 || c.Username != "app" || c.Password != "pw@x" || c.Database != "shop" {
		t.Errorf("url: %+v", c)
	}
	// SQLite: default path, and a missing file.
	if _, _, err := ConnFromEnv(map[string]string{"DB_CONNECTION": "sqlite"}, dir, ""); err == nil {
		t.Error("missing sqlite file should fail")
	}
	os.MkdirAll(filepath.Join(dir, "database"), 0o755)
	os.WriteFile(filepath.Join(dir, "database", "database.sqlite"), nil, 0o644)
	c, _, err = ConnFromEnv(map[string]string{}, dir, "")
	if err != nil || c.Driver != "sqlite" || c.Database != filepath.Join(dir, "database", "database.sqlite") {
		t.Errorf("default sqlite: %+v %v", c, err)
	}
}

func TestExplainWithFakeRunner(t *testing.T) {
	var sent map[string]any
	fixture := loadFixture(t, "mysql-fk-no-index")
	run := func(ctx context.Context, req []byte) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("no timeout on the run")
		}
		json.Unmarshal(req, &sent)
		return fixture, nil
	}
	c := Conn{Name: "mysql", Driver: "mysql", Host: "127.0.0.1", Port: 3306, Database: "app", Username: "root", Password: "pw"}
	res, err := Explain(context.Background(), run, c, Input{SQL: "select * from `posts` where `posts`.`user_id` = ? and `api_token` = ?", Bindings: []any{42.0, "[redacted]"}})
	if err != nil {
		t.Fatal(err)
	}
	if sent["analyze"] != false || !reflect.DeepEqual(sent["tables"], []any{"posts"}) {
		t.Errorf("request = %v", sent)
	}
	if b := sent["bindings"].([]any); b[1] != "x" {
		t.Errorf("redacted binding not swapped: %v", b)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "redacted") || !res.CanAnalyze {
		t.Errorf("notes = %v, canAnalyze %v", res.Notes, res.CanAnalyze)
	}
	// Analyze is refused for anything but a read, before PHP runs.
	called := false
	refuse := func(ctx context.Context, req []byte) ([]byte, error) { called = true; return nil, nil }
	if _, err := Explain(context.Background(), refuse, c, Input{SQL: "update `posts` set `x` = 1", Analyze: true}); !errors.Is(err, ErrAnalyzeSelect) || called {
		t.Errorf("err = %v, ran = %v", err, called)
	}
	if _, err := Explain(context.Background(), refuse, c, Input{SQL: "drop table `posts`"}); err == nil || called {
		t.Errorf("DDL: err = %v, ran = %v", err, called)
	}
	// The password never shows in errors.
	leak := func(ctx context.Context, req []byte) ([]byte, error) {
		return []byte(`{"ok":false,"error":"Access denied using password pw"}`), nil
	}
	if _, err := Explain(context.Background(), leak, c, Input{SQL: "select 1"}); err == nil || strings.Contains(err.Error(), " pw") {
		t.Errorf("err = %v", err)
	}
	// A run past the deadline reports a timeout.
	slow := func(ctx context.Context, req []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Explain(ctx, slow, c, Input{SQL: "select 1"}); err == nil {
		t.Error("expected a timeout error")
	}
}

func TestEnsureScript(t *testing.T) {
	dir := t.TempDir()
	p, err := EnsureScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "<?php") || filepath.Base(p) != ScriptName {
		t.Errorf("script = %s", p)
	}
	if !strings.Contains(string(b), "READ ONLY") {
		t.Error("the script should wrap Analyze in a read-only transaction")
	}
}

func TestCleanEnv(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "PHP_INI_SCAN_DIR=:/x", "MULLION_WATCH=/w", "HOME=/h"})
	if !reflect.DeepEqual(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Errorf("env = %v", got)
	}
}
