package profiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseE2EReports parses every report in MULLION_PROFILER_E2E_DIR
// (a real spx.data_dir) and logs its flame-graph summary.
func TestParseE2EReports(t *testing.T) {
	dir := os.Getenv("MULLION_PROFILER_E2E_DIR")
	if dir == "" {
		t.Skip("manual e2e helper")
	}
	metas, err := List(dir)
	if err != nil || len(metas) == 0 {
		t.Fatalf("no reports in %s: %v", dir, err)
	}
	for _, m := range metas {
		_, p, err := LoadProfile(dir, m.ID)
		if err != nil {
			t.Fatalf("%s: %v", m.ID, err)
		}
		before := p.Root.count()
		p.Prune(0.001)
		t.Logf("%s %s %s wall=%dus calls=%d events=%d root=%.0fns nodes=%d pruned=%d", m.ID, m.Method, m.URI, m.WallUs, m.Calls, p.Events, p.Root.Total, before, p.Pruned)
		if p.Events != 2*m.Recorded {
			t.Errorf("events %d != 2 x recorded calls %d", p.Events, m.Recorded)
		}
		if d := p.Root.Total/1000 - float64(m.WallUs); d > float64(m.WallUs)/20 || d < -float64(m.WallUs)/20 {
			t.Errorf("root %.0fus vs wall %dus", p.Root.Total/1000, m.WallUs)
		}
		for i, f := range p.Funcs {
			if i >= 5 {
				break
			}
			t.Logf("  top self: %-60s self=%.2fms incl=%.2fms calls=%d", f.Name, f.Self/1e6, f.Inclusive/1e6, f.Calls)
		}
		n := p.Root
		path := []string{}
		for len(n.Children) > 0 && len(path) < 8 {
			n = n.Children[0]
			path = append(path, filepath.Base(n.Name))
		}
		t.Logf("  hottest path: %s", strings.Join(path, " > "))
	}
}
