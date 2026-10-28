package perf

import (
	"reflect"
	"strings"
	"testing"

	"pm/internal/pmdir"
	"pm/internal/watch"
)

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"/":                        "/",
		"":                         "/",
		"/posts/42":                "/posts/{id}",
		"/posts/42/":               "/posts/{id}",
		"/posts/42/comments/7?x=1": "/posts/{id}/comments/{id}",
		"/u/0b6b8f4e-2a52-4c5e-9a0e-3c8f1d2e4b5a": "/u/{id}",
		"/o/01HZX3K9Q8R7T6Y5V4W3N2M1P0":           "/o/{id}",
		"/t/5f2b9c0a7d3e4f61":                     "/t/{id}",
		"/posts/hello-world":                      "/posts/hello-world",
		"/about":                                  "/about",
	} {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPercentile(t *testing.T) {
	v := []float64{}
	for i := 1; i <= 100; i++ {
		v = append(v, float64(i))
	}
	if Percentile(v, 50) != 50 || Percentile(v, 95) != 95 || Percentile(v, 99) != 99 || Percentile(v, 100) != 100 {
		t.Fatal("nearest rank on 1..100")
	}
	if Percentile([]float64{7}, 99) != 7 || Percentile(nil, 50) != 0 {
		t.Fatal("edge cases")
	}
}

// Caddy lines joined with the PHP side's request events and analyses.
func TestRequestsFromItemsAndAggregate(t *testing.T) {
	var items []watch.Item
	for i := 0; i < 20; i++ {
		rid := "r" + string(rune('a'+i))
		ms := float64(10 + i*10) // 10..200
		status := 200
		if i == 19 {
			status = 500
		}
		items = append(items,
			watch.Item{"type": "request", "src": "caddy", "t": float64(100 + i), "rid": rid, "method": "GET", "path": "/posts/" + string(rune('1'+i%9)), "status": float64(status), "ms": ms},
			watch.Item{"type": "request", "src": "php", "t": float64(100 + i), "rid": rid, "method": "GET", "uri": "/posts/1", "status": float64(status), "ms": ms - 2,
				"memory": float64(8 << 20), "queries": float64(3 + i), "queryMs": float64(2),
				"route": map[string]any{"uri": "posts/{post}", "name": "posts.show", "action": "App\\Http\\Controllers\\PostController@show"}},
		)
		if i%5 == 0 {
			items = append(items, watch.Item{"type": "analysis", "src": "php", "t": float64(100 + i), "rid": rid, "queries": float64(3 + i), "ms": 2.0,
				"issues": []any{map[string]any{"kind": "n+1", "sql": "select * from users where id = ?", "count": 6.0}}})
		}
	}
	// A static asset and a caddy-only request (no PHP: Watch's basic mode).
	items = append(items,
		watch.Item{"type": "request", "src": "caddy", "t": 130.0, "method": "GET", "path": "/build/app.js", "status": 200.0, "ms": 1.0, "static": true},
		watch.Item{"type": "request", "src": "caddy", "t": 131.0, "method": "GET", "path": "/users/12", "status": 200.0, "ms": 400.0},
		watch.Item{"type": "request", "src": "caddy", "t": 132.0, "method": "HEAD", "path": "/users/13", "status": 200.0, "ms": 500.0},
	)
	reqs := RequestsFromItems(items)
	if len(reqs) != 22 {
		t.Fatalf("got %d requests, want 22 (static left out)", len(reqs))
	}
	if reqs[0].Ms != 10 || reqs[0].Route != "posts/{post}" || !reqs[0].Deep || reqs[0].Queries != 3 || reqs[0].NPlusOne != 1 {
		t.Fatalf("join: %+v", reqs[0])
	}
	rows := Aggregate(reqs, Budgets{MaxMs: 150, MaxQueries: 20, MaxMemoryMB: 64})
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	var posts, users RouteStat
	for _, r := range rows {
		switch r.Key {
		case "GET /posts/{post}":
			posts = r
		case "GET /users/{id}":
			users = r
		}
	}
	if posts.Count != 20 || posts.Name != "posts.show" || posts.P50Ms != 100 || posts.P95Ms != 190 || posts.P99Ms != 200 || posts.MaxMs != 200 {
		t.Fatalf("posts percentiles: %+v", posts)
	}
	if posts.Errors != 1 || posts.ErrorRate != 0.05 || posts.NPlusOne != 4 || posts.MaxQueries != 22 || posts.AvgQueries != 12.5 || posts.PeakMemory != 8<<20 {
		t.Fatalf("posts stats: %+v", posts)
	}
	if len(posts.Over) != 2 || !strings.Contains(posts.Over[0], "p95 190 ms") || !strings.Contains(posts.Over[1], "22 queries") {
		t.Fatalf("budgets: %v", posts.Over)
	}
	if users.Count != 2 || users.DeepCount != 0 || len(users.Over) != 1 { // HEAD counts as GET
		t.Fatalf("users: %+v", users)
	}
	if rows[0].Key != "GET /posts/{post}" { // most total time first
		t.Fatalf("order: %s", rows[0].Key)
	}
	if len(posts.Recent) != 20 || posts.Recent[19] != 200 {
		t.Fatalf("recent: %v", posts.Recent)
	}
}

func TestBudgetsDefaults(t *testing.T) {
	b := Budgets{}.WithDefaults()
	if b != (Budgets{300, 30, 64}) {
		t.Fatal(b)
	}
	over := OverBudget(RouteStat{P95Ms: 100, DeepCount: 1, MaxQueries: 10, PeakMemory: 100 << 20}, Budgets{})
	if !reflect.DeepEqual(over, []string{"peak memory 100 MB (budget 64 MB)"}) {
		t.Fatal(over)
	}
}

func TestBaselineDiff(t *testing.T) {
	base := []RouteStat{
		{Key: "GET /a", Count: 10, P95Ms: 100, AvgMs: 80, AvgQueries: 5},
		{Key: "GET /b", Count: 10, P95Ms: 100, AvgMs: 80},
		{Key: "GET /c", Count: 3, P95Ms: 50},
		{Key: "GET /gone", Count: 3, P95Ms: 50},
	}
	cur := []RouteStat{
		{Key: "GET /b", Count: 12, P95Ms: 50, AvgMs: 40},
		{Key: "GET /a", Count: 12, P95Ms: 140, AvgMs: 90, AvgQueries: 5},
		{Key: "GET /c", Count: 3, P95Ms: 52},
		{Key: "GET /new", Count: 1, P95Ms: 10},
	}
	d := Diff(cur, base)
	var keys []string
	for _, x := range d {
		keys = append(keys, x.Key+":"+x.Status)
	}
	if !reflect.DeepEqual(keys, []string{"GET /a:changed", "GET /new:new", "GET /c:changed", "GET /b:changed", "GET /gone:gone"}) {
		t.Fatalf("order: %v", keys)
	}
	if !d[0].Regression || d[0].P95Pct != 40 || d[3].Regression || !d[3].Improvement || d[3].P95Pct != -50 || d[2].Regression {
		t.Fatalf("deltas: %+v", d)
	}
}

func TestBaselineStore(t *testing.T) {
	p := pmdir.Paths{Home: t.TempDir()}
	if _, err := SaveBaseline(p, "blog", "../x", "", []RouteStat{{Key: "GET /"}}); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := SaveBaseline(p, "blog", "before cache", "", []RouteStat{{Key: "GET /", Recent: []float64{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBaseline(p, "blog", "before cache")
	if err != nil || len(b.Routes) != 1 || b.Routes[0].Recent != nil {
		t.Fatalf("%+v %v", b, err)
	}
	if l := ListBaselines(p, "blog"); len(l) != 1 || l[0].Name != "before cache" {
		t.Fatal(l)
	}
	if err := DeleteBaseline(p, "blog", "before cache"); err != nil || len(ListBaselines(p, "blog")) != 0 {
		t.Fatal(err)
	}
	if ResetAt(p, "blog") != 0 || Reset(p, "blog") != nil || ResetAt(p, "blog") == 0 {
		t.Fatal("reset")
	}
	reqs := Since([]Request{{T: 1}, {T: 5}}, 3)
	if len(reqs) != 1 || reqs[0].T != 5 {
		t.Fatal(reqs)
	}
}

// The feed builds analysis items in-process with Go values.
func TestIssuesOfGoValues(t *testing.T) {
	it := watch.Item{"type": "analysis", "rid": "r1", "issues": []watch.QueryIssue{{Kind: "n+1", SQL: "select ?", Count: 7}}}
	reqs := RequestsFromItems([]watch.Item{{"type": "request", "src": "caddy", "rid": "r1", "method": "GET", "path": "/", "status": 200.0, "ms": 5.0}, it})
	if len(reqs) != 1 || reqs[0].NPlusOne != 1 {
		t.Fatalf("%+v", reqs)
	}
	if qs := SummarizeQueries([]watch.Item{it}, 0, 5); len(qs) != 1 || qs[0].MaxMs != 7 {
		t.Fatalf("%+v", qs)
	}
}
