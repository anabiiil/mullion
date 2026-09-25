package workers

import (
	"os"
	"strconv"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func TestBackoff(t *testing.T) {
	want := []time.Duration{1, 1, 2, 4, 8, 16, 32, 60, 60, 60}
	for n, w := range want {
		if got := backoff(n); got != w*time.Second {
			t.Errorf("backoff(%d) = %v, want %v", n, got, w*time.Second)
		}
	}
}

// fakeWorld simulates processes that die immediately after starting —
// a worker whose command crashes on boot.
type fakeWorld struct {
	now     time.Time
	alive   map[string]bool
	starts  int
	stopped []string
}

func newFakeSupervisor(paths pmdir.Paths, fw *fakeWorld) *supervisor {
	return &supervisor{
		now: func() time.Time { return fw.now },
		running: func(_ pmdir.Paths, site, id string) bool {
			return fw.alive[workerKey(site, id)]
		},
		start: func(p pmdir.Paths, site config.Site, w config.Worker, _ []string, now time.Time) error {
			fw.starts++
			pid := 900000 + fw.starts
			if err := os.WriteFile(pidFile(p, site.Name, w.ID), []byte(strconv.Itoa(pid)), 0o644); err != nil {
				return err
			}
			markStarted(p, workerKey(site.Name, w.ID), pid, false, now)
			return nil
		},
		stop: func(p pmdir.Paths, site, id string) error {
			fw.stopped = append(fw.stopped, workerKey(site, id))
			delete(fw.alive, workerKey(site, id))
			_ = os.Remove(pidFile(p, site, id))
			return nil
		},
		logf: func(string, ...any) {},
	}
}

func testPaths(t *testing.T) pmdir.Paths {
	t.Helper()
	p := pmdir.Paths{Home: t.TempDir()}
	for _, d := range []string{p.PidsDir(), p.LogsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestSuperviseCrashBackoffAndLoop(t *testing.T) {
	paths := testPaths(t)
	fw := &fakeWorld{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), alive: map[string]bool{}}
	s := newFakeSupervisor(paths, fw)
	w := config.Worker{ID: "queue-ab12", Name: "Queue", Command: "false", AutoStart: true}
	site := config.Site{Name: "shop", Path: t.TempDir(), Workers: []config.Worker{w}}
	state := &config.State{Sites: []config.Site{site}}

	// First pass: autostart.
	s.tick(state, paths, nil)
	if fw.starts != 1 {
		t.Fatalf("starts = %d, want 1 (autostart)", fw.starts)
	}

	// The process is dead (never marked alive). Each crash waits out a
	// growing backoff before the next start.
	wantDelays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, delay := range wantDelays {
		fw.now = fw.now.Add(500 * time.Millisecond)
		next := s.tick(state, paths, nil) // detects crash i+1, schedules restart
		if fw.starts != i+1 {
			t.Fatalf("crash %d: restarted before the backoff (starts=%d)", i+1, fw.starts)
		}
		if want := min(delay, tickEvery); next != want {
			t.Errorf("crash %d: next pass in %v, want %v", i+1, next, want)
		}
		st := Status(paths, site, w)
		if st.State != "restarting" {
			t.Errorf("crash %d: state %q, want restarting", i+1, st.State)
		}
		fw.now = fw.now.Add(delay - time.Millisecond)
		s.tick(state, paths, nil)
		if fw.starts != i+1 {
			t.Fatalf("crash %d: restarted %v early", i+1, time.Millisecond)
		}
		fw.now = fw.now.Add(time.Millisecond)
		s.tick(state, paths, nil)
		if fw.starts != i+2 {
			t.Fatalf("crash %d: not restarted after backoff (starts=%d)", i+1, fw.starts)
		}
	}
	if st := Status(paths, site, w); st.Restarts != 4 {
		t.Errorf("restarts = %d, want 4", st.Restarts)
	}

	// Fifth crash inside two minutes: crash-looping, no more restarts.
	fw.now = fw.now.Add(500 * time.Millisecond)
	s.tick(state, paths, nil)
	for i := 0; i < 10; i++ {
		fw.now = fw.now.Add(time.Minute)
		s.tick(state, paths, nil)
	}
	if fw.starts != 5 {
		t.Fatalf("starts = %d after crash loop, want 5", fw.starts)
	}
	st := Status(paths, site, w)
	if !st.CrashLooping || st.State != "crash-looping" {
		t.Fatalf("status = %+v, want crash-looping", st)
	}

	// A manual start clears the mark (markStarted with manual=true).
	markStarted(paths, workerKey(site.Name, w.ID), 1, true, fw.now)
	if st := Status(paths, site, w); st.CrashLooping {
		t.Fatalf("manual start did not clear crash-looping: %+v", st)
	}
}

func TestSuperviseStableRunResetsBackoff(t *testing.T) {
	paths := testPaths(t)
	fw := &fakeWorld{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), alive: map[string]bool{}}
	s := newFakeSupervisor(paths, fw)
	w := config.Worker{ID: "q", Command: "x", AutoStart: true}
	site := config.Site{Name: "shop", Path: t.TempDir(), Workers: []config.Worker{w}}
	state := &config.State{Sites: []config.Site{site}}
	key := workerKey("shop", "q")

	s.tick(state, paths, nil) // start
	fw.now = fw.now.Add(time.Second)
	s.tick(state, paths, nil) // crash 1
	fw.now = fw.now.Add(time.Second)
	s.tick(state, paths, nil) // restart
	fw.alive[key] = true
	fw.now = fw.now.Add(3 * time.Minute)
	s.tick(state, paths, nil) // stable → reset
	if r := loadRecords(paths)[key]; r.Consecutive != 0 {
		t.Fatalf("consecutive = %d after a stable run, want 0", r.Consecutive)
	}
	delete(fw.alive, key)
	fw.now = fw.now.Add(time.Second)
	if next := s.tick(state, paths, nil); next != time.Second {
		t.Fatalf("backoff after a stable run = %v, want 1s", next)
	}
}

func TestSuperviseManualWorkerNotAutostarted(t *testing.T) {
	paths := testPaths(t)
	fw := &fakeWorld{now: time.Now(), alive: map[string]bool{}}
	s := newFakeSupervisor(paths, fw)
	w := config.Worker{ID: "q", Command: "x"} // AutoStart false
	state := &config.State{Sites: []config.Site{{Name: "shop", Path: t.TempDir(), Workers: []config.Worker{w}}}}
	s.tick(state, paths, nil)
	if fw.starts != 0 {
		t.Fatalf("a non-autostart worker was started")
	}
}

func TestSuperviseStopsPausedAndOrphans(t *testing.T) {
	paths := testPaths(t)
	fw := &fakeWorld{now: time.Now(), alive: map[string]bool{}}
	s := newFakeSupervisor(paths, fw)
	paused := config.Worker{ID: "sched-1", Command: "x", AutoStart: true, Paused: true}
	state := &config.State{Sites: []config.Site{
		{Name: "my-shop", Path: t.TempDir(), Workers: []config.Worker{paused}},
	}}
	fw.alive["my-shop/sched-1"] = true
	_ = os.WriteFile(pidFile(paths, "my-shop", "sched-1"), []byte("1"), 0o644)
	// Orphans: a worker id no longer configured, and a removed site.
	fw.alive["my-shop/old-q"] = true
	_ = os.WriteFile(pidFile(paths, "my-shop", "old-q"), []byte("2"), 0o644)
	_ = os.WriteFile(pidFile(paths, "gone", "q-1"), []byte("3"), 0o644)

	s.tick(state, paths, nil)
	want := map[string]bool{"my-shop/sched-1": true, "my-shop/old-q": true, "gone/q-1": true}
	if len(fw.stopped) != len(want) {
		t.Fatalf("stopped %v, want %v", fw.stopped, want)
	}
	for _, k := range fw.stopped {
		if !want[k] {
			t.Errorf("unexpected stop of %s", k)
		}
	}
	if fw.starts != 0 {
		t.Errorf("a paused worker was started")
	}
}

func TestMatchesConfiguredWithDashes(t *testing.T) {
	configured := map[string]bool{"my-shop/queue-ab12": true}
	if !matchesConfigured("my-shop-queue-ab12", configured) {
		t.Error("dashed site + id not matched")
	}
	if matchesConfigured("my-shop-queue-zz", configured) {
		t.Error("unconfigured id matched")
	}
}
