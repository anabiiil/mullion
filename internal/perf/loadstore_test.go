package perf

import (
	"testing"
	"time"

	"pm/internal/loadtest"
	"pm/internal/pmdir"
)

func TestLoadStore(t *testing.T) {
	p := pmdir.Paths{Home: t.TempDir()}
	t0 := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	a, err := SaveLoadResult(p, "blog", LoadResult{At: t0, URL: "http://blog.test/", Method: "GET", Mode: "dev", Result: loadtest.Snapshot{RPS: 100, P95Ms: 50, Timeline: []loadtest.Point{{T: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SaveLoadResult(p, "blog", LoadResult{At: t0.Add(time.Minute), URL: "http://blog.test/", Method: "GET", Mode: "prod", Result: loadtest.Snapshot{RPS: 150, P95Ms: 25}})
	list := LoadResults(p, "blog", false)
	if len(list) != 2 || list[0].ID != b.ID || list[1].Result.Timeline != nil {
		t.Fatalf("%+v", list)
	}
	prev, ok := Previous(list, b)
	if !ok || prev.ID != a.ID {
		t.Fatal("previous")
	}
	c := CompareLoad(b, prev)
	if c.RPSPct != 50 || c.P95Pct != -50 || c.AgainstMode != "dev" {
		t.Fatalf("%+v", c)
	}
	if _, err := LoadResultByID(p, "blog", "../x"); err == nil {
		t.Fatal("path escape")
	}
	if full, err := LoadResultByID(p, "blog", a.ID); err != nil || len(full.Result.Timeline) != 1 {
		t.Fatal(err)
	}
	if DeleteLoadResult(p, "blog", a.ID) != nil || len(LoadResults(p, "blog", false)) != 1 {
		t.Fatal("delete")
	}
}
