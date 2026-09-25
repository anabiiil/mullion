package workers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

const (
	// tickEvery is the supervisor's normal polling period.
	tickEvery = 5 * time.Second
	// crashWindow / crashLoopAfter: this many crashes inside the window
	// marks the worker crash-looping and stops the retries.
	crashWindow    = 2 * time.Minute
	crashLoopAfter = 5
	maxBackoff     = 60 * time.Second
)

// backoff is the wait before the n-th consecutive restart: 1s, 2s, 4s…
// capped at a minute.
func backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 7 {
		return maxBackoff
	}
	d := time.Second << (n - 1)
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// supervisor holds the injectable pieces of the loop (tests swap the
// clock and the process functions).
type supervisor struct {
	now     func() time.Time
	running func(paths pmdir.Paths, site, id string) bool
	start   func(paths pmdir.Paths, site config.Site, w config.Worker, env []string, now time.Time) error
	stop    func(paths pmdir.Paths, site, id string) error
	logf    func(format string, args ...any)
}

func newSupervisor() *supervisor {
	return &supervisor{
		now:     time.Now,
		running: Running,
		start: func(paths pmdir.Paths, site config.Site, w config.Worker, env []string, now time.Time) error {
			return start(paths, site, w, env, false, now)
		},
		stop: Stop,
		logf: func(format string, args ...any) { fmt.Printf("workers: "+format+"\n", args...) },
	}
}

// Supervise keeps every site's workers in the state the config asks
// for, until ctx is cancelled. Each pass (every 5s, sooner when a
// restart is due) re-reads the state through load, then:
//
//   - starts AutoStart && !Paused workers that are not running;
//   - restarts crashed workers — any unpaused worker whose process died
//     without Stop — after a 1s, 2s, 4s… (max 60s) backoff, and marks
//     one crash-looping after 5 crashes within 2 minutes (no more
//     retries until it is started by hand);
//   - stops paused workers and workers no longer in the config.
//
// Cancelling ctx only ends the loop; the workers are left running
// (they are detached and outlive an agent restart). A failing load or
// a panic inside a pass is logged and retried on the next pass, so it
// is safe to run in a goroutine of a long-lived process. envFor may be
// nil (os.Environ()).
func Supervise(ctx context.Context, load func() (*config.State, pmdir.Paths, error), envFor func(config.Site) []string) {
	s := newSupervisor()
	for {
		wait := tickEvery
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.logf("supervisor pass panicked: %v", r)
				}
			}()
			state, paths, err := load()
			if err != nil || state == nil {
				return
			}
			wait = s.tick(state, paths, envFor)
		}()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// tick runs one supervision pass and returns how long to wait before
// the next one.
func (s *supervisor) tick(state *config.State, paths pmdir.Paths, envFor func(config.Site) []string) time.Duration {
	next := tickEvery
	configured := map[string]bool{}
	for _, site := range state.Sites {
		for _, w := range site.Workers {
			if w.ID == "" {
				continue
			}
			configured[workerKey(site.Name, w.ID)] = true
			if d := s.superviseOne(paths, site, w, envFor); d > 0 && d < next {
				next = d
			}
		}
	}
	s.stopOrphans(paths, state, configured)
	if next < time.Second {
		next = time.Second
	}
	return next
}

// superviseOne handles one worker; a positive result asks for the next
// pass sooner (a restart is due then).
func (s *supervisor) superviseOne(paths pmdir.Paths, site config.Site, w config.Worker, envFor func(config.Site) []string) time.Duration {
	key := workerKey(site.Name, w.ID)
	now := s.now()
	running := s.running(paths, site.Name, w.ID)

	if w.Paused {
		if running || hasPidFile(paths, site.Name, w.ID) {
			if err := s.stop(paths, site.Name, w.ID); err != nil {
				s.logf("%s: %v", key, err)
			}
		}
		return 0
	}

	if running {
		rotateIfLarge(LogPath(paths, site.Name, w.ID))
		// A run that survived the crash window resets the backoff.
		if r := loadRecords(paths)[key]; r != nil && r.Consecutive > 0 && now.Sub(r.StartedAt) >= crashWindow {
			updateRecords(paths, func(recs map[string]*record) {
				if r := recs[key]; r != nil {
					r.Consecutive = 0
				}
			})
		}
		return 0
	}

	// Not running. A pid file left behind means it died without Stop:
	// a crash (or a reboot) — schedule a restart with backoff.
	if hasPidFile(paths, site.Name, w.ID) {
		_ = os.Remove(pidFile(paths, site.Name, w.ID))
		looping := false
		updateRecords(paths, func(recs map[string]*record) {
			r := recs[key]
			if r == nil {
				r = &record{}
				recs[key] = r
			}
			r.Pid = 0
			if r.LastExit == "" {
				r.LastExit = "exited (status unknown)"
			}
			kept := r.Crashes[:0]
			for _, t := range r.Crashes {
				if now.Sub(t) < crashWindow {
					kept = append(kept, t)
				}
			}
			r.Crashes = append(kept, now)
			r.Consecutive++
			if len(r.Crashes) >= crashLoopAfter {
				r.CrashLoop = true
				r.Pending = false
				r.NextAttempt = time.Time{}
				looping = true
				return
			}
			r.Pending = true
			r.NextAttempt = now.Add(backoff(r.Consecutive))
		})
		if looping {
			s.logf("%s crashed %d times in %s — not restarting it until it is started by hand", key, crashLoopAfter, crashWindow)
			if f, err := os.OpenFile(LogPath(paths, site.Name, w.ID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
				fmt.Fprintf(f, "---- %s: mullion: crashed %d times within %s — not restarting until started by hand ----\n",
					now.Format(time.RFC3339), crashLoopAfter, crashWindow)
				f.Close()
			}
			return 0
		}
	}

	r := loadRecords(paths)[key]
	if r != nil && r.CrashLoop {
		return 0
	}
	pending := r != nil && r.Pending
	if !w.AutoStart && !pending {
		return 0
	}
	if pending && now.Before(r.NextAttempt) {
		return r.NextAttempt.Sub(now)
	}
	var env []string
	if envFor != nil {
		env = envFor(site)
	}
	if err := s.start(paths, site, w, env, now); err != nil {
		s.logf("%s: %v", key, err)
		return 0
	}
	if pending {
		updateRecords(paths, func(recs map[string]*record) {
			if r := recs[key]; r != nil {
				r.Restarts++
			}
		})
	}
	return 0
}

// stopOrphans stops workers whose pid file no longer matches any
// configured worker (the worker or its whole site was removed).
func (s *supervisor) stopOrphans(paths pmdir.Paths, state *config.State, configured map[string]bool) {
	entries, err := os.ReadDir(paths.PidsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "worker-") || !strings.HasSuffix(name, ".pid") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, "worker-"), ".pid")
		// Site names and ids both may contain dashes: the file belongs
		// to a configured worker when SOME split matches one.
		if matchesConfigured(rest, configured) {
			continue
		}
		site, id := splitOrphan(rest, state)
		if err := s.stop(paths, site, id); err != nil {
			s.logf("orphan %s: %v", rest, err)
		}
		_ = os.Remove(filepath.Join(paths.PidsDir(), name))
		updateRecords(paths, func(recs map[string]*record) { delete(recs, workerKey(site, id)) })
	}
}

func matchesConfigured(rest string, configured map[string]bool) bool {
	for i := 0; i < len(rest); i++ {
		if rest[i] == '-' && configured[workerKey(rest[:i], rest[i+1:])] {
			return true
		}
	}
	return false
}

// splitOrphan recovers (site, id) from a pid file name, preferring a
// still-linked site's name as the prefix, else the first dash.
func splitOrphan(rest string, state *config.State) (string, string) {
	best := ""
	for _, site := range state.Sites {
		if strings.HasPrefix(rest, site.Name+"-") && len(site.Name) > len(best) {
			best = site.Name
		}
	}
	if best != "" {
		return best, rest[len(best)+1:]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}
