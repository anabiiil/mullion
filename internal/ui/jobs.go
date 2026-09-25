package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// job is a long-running background operation the control panel started
// (an install, a switch, opening pgAdmin...). WKWebView would time out
// a request that blocks for a minute or more, so handlers for these
// operations start a goroutine, hand the browser a job id right away,
// and the page polls /api/job until it reports done.
type job struct {
	mu         sync.Mutex
	done       bool
	err        string
	result     any
	status     string
	finishedAt time.Time
}

func (j *job) setStatus(s string) {
	j.mu.Lock()
	j.status = s
	j.mu.Unlock()
}

func (j *job) finish(result any, err error, now time.Time) {
	j.mu.Lock()
	j.done = true
	j.result = result
	if err != nil {
		j.err = err.Error()
	}
	j.finishedAt = now
	j.mu.Unlock()
}

// JobStatus is the JSON shape /api/job returns for a polled job.
type JobStatus struct {
	Done   bool   `json:"done"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
	Log    string `json:"log"`
}

// jobRegistry tracks in-flight and recently-finished jobs, and
// serializes the heavy ones (installs, downloads) so only one runs at
// a time — a second concurrent install is rejected outright rather
// than queued, since two installs racing on the same files would be
// unsafe and there's nothing useful for the user to do but wait.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*job

	installMu sync.Mutex

	ttd time.Duration
	now func() time.Time
}

func newJobRegistry(ttl time.Duration) *jobRegistry {
	return &jobRegistry{
		jobs: make(map[string]*job),
		ttd:  ttl,
		now:  time.Now,
	}
}

func newJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to a time-based id so callers
		// still get something unique enough for this in-process map.
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

// start runs fn in its own goroutine (with no connection to any HTTP
// request's context — jobs must outlive the request that started
// them) and returns immediately with a job id to poll.
func (r *jobRegistry) start(fn func(setStatus func(string)) (any, error)) string {
	id := newJobID()
	j := &job{}

	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()

	go func() {
		result, err := fn(j.setStatus)
		j.finish(result, err, r.now())
	}()

	r.sweep()
	return id
}

// startInstall is like start, but only one install-class job may run
// at a time. If one is already running, it returns an error
// synchronously instead of starting a second job.
func (r *jobRegistry) startInstall(fn func(setStatus func(string)) (any, error)) (string, error) {
	if !r.installMu.TryLock() {
		return "", errors.New("another install is already in progress — wait for it to finish and try again")
	}
	id := r.start(func(setStatus func(string)) (any, error) {
		defer r.installMu.Unlock()
		return fn(setStatus)
	})
	return id, nil
}

// get reports a job's current status, or false if the id is unknown
// (never started, or already swept away after finishing long ago).
func (r *jobRegistry) get(id string) (JobStatus, bool) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return JobStatus{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobStatus{Done: j.done, Error: j.err, Result: j.result, Log: j.status}, true
}

// sweep drops jobs that finished more than ttd ago, so the map doesn't
// grow forever across a long-running panel session.
func (r *jobRegistry) sweep() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, j := range r.jobs {
		j.mu.Lock()
		expired := j.done && now.Sub(j.finishedAt) > r.ttd
		j.mu.Unlock()
		if expired {
			delete(r.jobs, id)
		}
	}
}

// jobs is the process-wide registry used by every job-backed endpoint.
var jobs = newJobRegistry(10 * time.Minute)
