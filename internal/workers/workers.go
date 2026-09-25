// Package workers runs a site's long-lived background commands — queue
// workers, the Laravel scheduler, custom daemons — detached from the
// CLI/panel, logged to ~/.mullion/logs and tracked by pid files in
// ~/.mullion/pids. Supervise keeps them up: auto-starting, restarting
// crashed ones with backoff, and giving up on crash loops.
package workers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

// stopGrace is how long a worker gets to exit after SIGTERM (a queue
// worker finishes its current job) before it is killed.
const stopGrace = 8 * time.Second

// maxLogSize is the size at which a worker log is rotated to .log.1.
const maxLogSize = 20 << 20

// WorkerStatus is a worker's live state, merged from its pid file and
// the supervisor's bookkeeping.
type WorkerStatus struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	Pid     int    `json:"pid,omitempty"`
	// StartedAt is the last start (manual or automatic).
	StartedAt time.Time `json:"startedAt,omitzero"`
	// Restarts counts automatic restarts after crashes.
	Restarts int `json:"restarts"`
	// LastExit describes how the previous run ended ("exit status 1",
	// "stopped", "exited (status unknown)").
	LastExit string `json:"lastExit,omitempty"`
	// CrashLooping: it crashed 5 times within 2 minutes; the supervisor
	// stopped retrying until it is started by hand.
	CrashLooping bool `json:"crashLooping,omitempty"`
	// State is one word for the UI: running | stopped | paused |
	// restarting (waiting out a crash backoff) | crash-looping.
	State string `json:"state"`
}

func workerKey(site, id string) string { return site + "/" + id }

func pidFile(paths pmdir.Paths, site, id string) string {
	return filepath.Join(paths.PidsDir(), "worker-"+site+"-"+id+".pid")
}

// LogPath is the worker's log file (stdout+stderr of every run).
func LogPath(paths pmdir.Paths, site, id string) string {
	return filepath.Join(paths.LogsDir(), "worker-"+site+"-"+id+".log")
}

func readPid(paths pmdir.Paths, site, id string) int {
	data, err := os.ReadFile(pidFile(paths, site, id))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

func hasPidFile(paths pmdir.Paths, site, id string) bool {
	_, err := os.Stat(pidFile(paths, site, id))
	return err == nil
}

// Running reports whether the worker's process is alive.
func Running(paths pmdir.Paths, site, id string) bool {
	return processAlive(readPid(paths, site, id))
}

// startMu serializes starts within one process, so the panel and an
// in-process supervisor can't launch the same worker twice.
var startMu sync.Mutex

// Start launches a worker by hand: it also clears any crash-loop mark
// and backoff, so a user's explicit start always gets a fresh chance.
// No-op when it is already running. env nil = os.Environ().
func Start(paths pmdir.Paths, site config.Site, w config.Worker, env []string) error {
	return start(paths, site, w, env, true, time.Now())
}

func start(paths pmdir.Paths, site config.Site, w config.Worker, env []string, manual bool, now time.Time) error {
	if strings.TrimSpace(w.Command) == "" {
		return fmt.Errorf("worker %q has no command", w.ID)
	}
	if w.ID == "" {
		return errors.New("worker has no id")
	}
	if site.Path == "" {
		return fmt.Errorf("site %s has no path", site.Name)
	}
	startMu.Lock()
	defer startMu.Unlock()
	if Running(paths, site.Name, w.ID) {
		return nil
	}
	for _, dir := range []string{paths.LogsDir(), paths.PidsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	logPath := LogPath(paths, site.Name, w.ID)
	rotateIfLarge(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	how := "starting"
	if !manual {
		how = "restarting"
	}
	fmt.Fprintf(logFile, "\n---- %s: mullion %s worker %q: `%s` ----\n",
		now.Format(time.RFC3339), how, w.Name, w.Command)

	if env == nil {
		env = os.Environ()
	}
	cmd := detachedShell(w.Command)
	cmd.Dir = site.Path
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logFile, "---- could not start: %v ----\n", err)
		return fmt.Errorf("starting worker %s of %s: %w", w.ID, site.Name, err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFile(paths, site.Name, w.ID), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		terminate(pid, 0)
		_ = cmd.Wait()
		return err
	}
	markStarted(paths, workerKey(site.Name, w.ID), pid, manual, now)

	// Reap it in the background: a long-lived caller (the agent, the
	// panel) would otherwise keep a zombie around that still looks
	// alive to kill(pid, 0). A short-lived CLI just exits; the orphan
	// is then reaped by init/launchd.
	go func() {
		err := cmd.Wait()
		exit := "exited (status 0)"
		if err != nil {
			exit = err.Error()
		}
		if f, ferr := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			fmt.Fprintf(f, "---- %s: %s ----\n", time.Now().Format(time.RFC3339), exit)
			f.Close()
		}
		updateRecords(paths, func(recs map[string]*record) {
			if r := recs[workerKey(site.Name, w.ID)]; r != nil && r.Pid == pid && r.LastExit == "" {
				r.LastExit = exit
			}
		})
	}()
	return nil
}

// markStarted records a fresh run in the bookkeeping file.
func markStarted(paths pmdir.Paths, key string, pid int, manual bool, now time.Time) {
	updateRecords(paths, func(recs map[string]*record) {
		r := recs[key]
		if r == nil {
			r = &record{}
			recs[key] = r
		}
		r.Pid = pid
		r.StartedAt = now
		r.LastExit = ""
		r.Pending = false
		r.NextAttempt = time.Time{}
		if manual {
			r.CrashLoop = false
			r.Crashes = nil
			r.Consecutive = 0
		}
	})
}

// Stop terminates a worker's whole process tree (no-op when it is not
// running) and forgets its pid, so the supervisor treats it as stopped
// on purpose rather than crashed.
func Stop(paths pmdir.Paths, site, id string) error {
	pid := readPid(paths, site, id)
	// Drop the pid file first: a supervisor ticking meanwhile must see a
	// deliberate stop, not a crash to restart.
	_ = os.Remove(pidFile(paths, site, id))
	if processAlive(pid) {
		terminate(pid, stopGrace)
		if processAlive(pid) {
			return fmt.Errorf("worker %s of %s (pid %d) did not stop", id, site, pid)
		}
	}
	updateRecords(paths, func(recs map[string]*record) {
		if r := recs[workerKey(site, id)]; r != nil {
			r.Pending = false
			r.NextAttempt = time.Time{}
			if pid > 0 && r.Pid == pid {
				r.LastExit = "stopped"
			}
			r.Pid = 0
		}
	})
	return nil
}

// Forget stops a worker and drops its pid file, bookkeeping and logs —
// used when the worker is deleted from the config.
func Forget(paths pmdir.Paths, site, id string) error {
	err := Stop(paths, site, id)
	updateRecords(paths, func(recs map[string]*record) { delete(recs, workerKey(site, id)) })
	logPath := LogPath(paths, site, id)
	_ = os.Remove(logPath)
	_ = os.Remove(logPath + ".1")
	return err
}

// Status merges the live process state with the supervisor's
// bookkeeping for one configured worker.
func Status(paths pmdir.Paths, site config.Site, w config.Worker) WorkerStatus {
	st := WorkerStatus{ID: w.ID}
	pid := readPid(paths, site.Name, w.ID)
	st.Running = processAlive(pid)
	if st.Running {
		st.Pid = pid
	}
	r := loadRecords(paths)[workerKey(site.Name, w.ID)]
	if r != nil {
		st.StartedAt = r.StartedAt
		st.Restarts = r.Restarts
		st.LastExit = r.LastExit
		st.CrashLooping = r.CrashLoop
	}
	switch {
	case st.Running:
		st.State = "running"
	case st.CrashLooping:
		st.State = "crash-looping"
	case w.Paused:
		st.State = "paused"
	case r != nil && r.Pending:
		st.State = "restarting"
	default:
		st.State = "stopped"
	}
	if !st.Running && st.LastExit == "" && pid > 0 {
		st.LastExit = "exited (status unknown)"
	}
	return st
}

// LogTail returns the last `lines` lines of the worker's log.
func LogTail(paths pmdir.Paths, site, id string, lines int) string {
	return tailFile(LogPath(paths, site, id), lines)
}

func tailFile(path string, lines int) string {
	if lines <= 0 {
		lines = 50
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	// Enough bytes for the requested lines of a typical log, capped.
	want := int64(lines)*400 + 4096
	if want > 1<<20 {
		want = 1 << 20
	}
	offset := info.Size() - want
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	text := strings.TrimRight(string(data), "\n")
	if offset > 0 {
		// Drop the (probably partial) first line.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	all := strings.Split(text, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// rotateIfLarge moves an oversized log's content to <log>.1 and empties
// it in place. Truncating (rather than renaming) keeps a running
// worker's append-mode handle writing into the live file.
func rotateIfLarge(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxLogSize {
		return
	}
	src, err := os.Open(path)
	if err != nil {
		return
	}
	dst, err := os.Create(path + ".1")
	if err != nil {
		src.Close()
		return
	}
	_, cerr := io.Copy(dst, src)
	src.Close()
	dst.Close()
	if cerr == nil {
		_ = os.Truncate(path, 0)
	}
}

// ---- bookkeeping ----------------------------------------------------

// record is the supervisor's memory of one worker, kept across agent
// restarts in pids/workers.json.
type record struct {
	Pid       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Restarts  int       `json:"restarts,omitempty"`
	LastExit  string    `json:"lastExit,omitempty"`
	// Crashes within the crash-loop window (older ones are pruned).
	Crashes []time.Time `json:"crashes,omitempty"`
	// Consecutive crashes without a stable run in between (drives the
	// backoff; reset after running crashWindow without dying).
	Consecutive int `json:"consecutive,omitempty"`
	// Pending: it crashed and is due for a restart at NextAttempt.
	Pending     bool      `json:"pending,omitempty"`
	NextAttempt time.Time `json:"nextAttempt,omitzero"`
	CrashLoop   bool      `json:"crashLoop,omitempty"`
}

var recordsMu sync.Mutex

func recordsFile(paths pmdir.Paths) string {
	return filepath.Join(paths.PidsDir(), "workers.json")
}

func loadRecords(paths pmdir.Paths) map[string]*record {
	recordsMu.Lock()
	defer recordsMu.Unlock()
	return readRecords(paths)
}

func readRecords(paths pmdir.Paths) map[string]*record {
	recs := map[string]*record{}
	data, err := os.ReadFile(recordsFile(paths))
	if err == nil {
		_ = json.Unmarshal(data, &recs)
	}
	for k, r := range recs {
		if r == nil {
			delete(recs, k)
		}
	}
	return recs
}

// updateRecords is a read-modify-write of the bookkeeping file (atomic
// rename, so a reader never sees half a file).
func updateRecords(paths pmdir.Paths, fn func(map[string]*record)) {
	recordsMu.Lock()
	defer recordsMu.Unlock()
	recs := readRecords(paths)
	fn(recs)
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(paths.PidsDir(), 0o755); err != nil {
		return
	}
	tmp := recordsFile(paths) + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, recordsFile(paths)); err != nil {
		_ = os.Remove(tmp)
	}
}
