//go:build !windows

package workers

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartStatusStopLive(t *testing.T) {
	paths := testPaths(t)
	dir := t.TempDir()
	site := config.Site{Name: "demo-app", Path: dir}
	// A child that spawns a grandchild: Stop must take the whole tree.
	w := config.Worker{ID: "loop-x1", Name: "Loop", Command: `echo "hello from $PWD $MULLION_T"; sleep 30 & sleep 30; wait`}
	env := append(os.Environ(), "MULLION_T=envok")

	if err := Start(paths, site, w, env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Stop(paths, site.Name, w.ID) })
	if !Running(paths, site.Name, w.ID) {
		t.Fatal("not running right after Start")
	}
	st := Status(paths, site, w)
	if !st.Running || st.Pid == 0 || st.State != "running" || st.StartedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	// Starting again is a no-op (same pid).
	if err := Start(paths, site, w, env); err != nil {
		t.Fatal(err)
	}
	if st2 := Status(paths, site, w); st2.Pid != st.Pid {
		t.Fatalf("second Start spawned a new process: %d vs %d", st2.Pid, st.Pid)
	}
	waitFor(t, "log output", func() bool {
		return strings.Contains(LogTail(paths, site.Name, w.ID, 10), "envok")
	})
	tail := LogTail(paths, site.Name, w.ID, 10)
	if !strings.Contains(tail, "mullion starting worker") || !strings.Contains(tail, dir[len(dir)-8:]) {
		t.Errorf("log tail missing header or cwd:\n%s", tail)
	}

	pid := st.Pid
	if err := Stop(paths, site.Name, w.ID); err != nil {
		t.Fatal(err)
	}
	if Running(paths, site.Name, w.ID) || groupAlive(pid) {
		t.Fatal("worker tree still alive after Stop")
	}
	st = Status(paths, site, w)
	if st.Running || st.State != "stopped" || st.LastExit != "stopped" {
		t.Fatalf("status after stop = %+v", st)
	}
}

func TestExitRecordedLive(t *testing.T) {
	paths := testPaths(t)
	site := config.Site{Name: "demo", Path: t.TempDir()}
	w := config.Worker{ID: "boom", Command: "exit 3"}
	if err := Start(paths, site, w, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit recorded", func() bool {
		return Status(paths, site, w).LastExit == "exit status 3"
	})
	if !strings.Contains(LogTail(paths, site.Name, w.ID, 5), "exit status 3") {
		t.Error("exit footer missing from log")
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	paths := testPaths(t)
	if err := Start(paths, config.Site{Name: "a", Path: t.TempDir()}, config.Worker{ID: "x", Command: "  "}, nil); err == nil {
		t.Fatal("empty command accepted")
	}
}

func TestTailFile(t *testing.T) {
	p := t.TempDir() + "/x.log"
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	_ = os.WriteFile(p, []byte(b.String()), 0o644)
	got := strings.Split(tailFile(p, 3), "\n")
	if len(got) != 3 || got[2] != "line "+strings.Repeat("x", 99%7) {
		t.Fatalf("tail = %q", got)
	}
	if tailFile(p+".missing", 3) != "" {
		t.Fatal("missing file should give empty tail")
	}
}

func TestSuperviseLiveAutostartAndRemove(t *testing.T) {
	paths := testPaths(t)
	w := config.Worker{ID: "sl-1", Command: "sleep 30", AutoStart: true}
	state := &config.State{Sites: []config.Site{{Name: "live", Path: t.TempDir(), Workers: []config.Worker{w}}}}
	var mu sync.Mutex
	load := func() (*config.State, pmdir.Paths, error) {
		mu.Lock()
		defer mu.Unlock()
		return state, paths, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Supervise(ctx, load, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done; _ = Stop(paths, "live", "sl-1") })

	waitFor(t, "autostart", func() bool { return Running(paths, "live", "sl-1") })
	pid := readPid(paths, "live", "sl-1")

	// Removing it from the config makes the next pass stop it.
	mu.Lock()
	state = &config.State{Sites: []config.Site{{Name: "live", Path: state.Sites[0].Path}}}
	mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatal("removed worker still running")
	}
}
