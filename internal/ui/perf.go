package ui

// The project page's Performance tab: route stats and baselines, the
// readiness check, load tests (run here, in the panel, streamed over
// /api/perf/load/stream as SSE), production-like mode, and worker
// health (sampled every few seconds by this process while it runs) —
// see internal/perf and internal/app/perf.go.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pm/internal/app"
	"pm/internal/loadtest"
	"pm/internal/perf"
	"pm/internal/workers"
)

// workerSampleEvery is how often running workers are measured.
const workerSampleEvery = 10 * time.Second

var perfSampler struct {
	once sync.Once
	s    *workers.Sampler
}

// startWorkerSampler measures every site's running workers while the
// panel runs (one `ps` per tick, nothing when no worker runs).
func startWorkerSampler(a *app.App) *workers.Sampler {
	perfSampler.once.Do(func() {
		perfSampler.s = workers.NewSampler(a.Paths, workerSampleEvery)
		go perfSampler.s.Run(context.Background(), func() []workers.Target {
			na, err := app.New()
			if err != nil {
				return nil
			}
			return na.WorkerTargets()
		})
	})
	return perfSampler.s
}

// ── load test runs ───────────────────────────────────────────────────

type loadRun struct {
	id     string
	site   string
	cancel context.CancelFunc

	mu      sync.Mutex
	snap    loadtest.Snapshot
	seq     int
	done    bool
	err     string
	result  *perf.LoadResult
	compare *perf.LoadCompare
	ended   time.Time
}

func (r *loadRun) state() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]any{"id": r.id, "site": r.site, "snapshot": r.snap, "done": r.done}
	if r.err != "" {
		out["error"] = r.err
	}
	if r.result != nil {
		out["result"] = r.result
	}
	if r.compare != nil {
		out["compare"] = r.compare
	}
	return out
}

var loadRuns = struct {
	sync.Mutex
	m map[string]*loadRun
}{m: map[string]*loadRun{}}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// startLoad runs one load test in the background. One at a time per
// site: two would only measure each other.
func startLoad(a *app.App, site string, in app.LoadInput) (string, error) {
	c, err := a.LoadConfig(site, in)
	if err != nil {
		return "", err
	}
	loadRuns.Lock()
	for id, r := range loadRuns.m {
		r.mu.Lock()
		running, old := !r.done, r.done && time.Since(r.ended) > 15*time.Minute
		r.mu.Unlock()
		if running && r.site == site {
			loadRuns.Unlock()
			return "", errors.New("a load test is already running for this site — stop it first")
		}
		if old {
			delete(loadRuns.m, id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &loadRun{id: newRunID(), site: site, cancel: cancel, snap: loadtest.Snapshot{Phase: "starting"}}
	loadRuns.m[run.id] = run
	loadRuns.Unlock()

	go func() {
		defer cancel()
		snap, err := loadtest.Run(ctx, c, func(s loadtest.Snapshot) {
			run.mu.Lock()
			run.snap = s
			run.seq++
			run.mu.Unlock()
		})
		var res *perf.LoadResult
		var cmp *perf.LoadCompare
		if err == nil && snap.Requests > 0 {
			if na, aerr := app.New(); aerr == nil {
				if saved, serr := na.SaveLoad(site, in, c, snap); serr == nil {
					res = &saved
					if prev, ok := perf.Previous(perf.LoadResults(na.Paths, site, false), saved); ok {
						v := perf.CompareLoad(saved, prev)
						cmp = &v
					}
				}
			}
		}
		run.mu.Lock()
		run.snap = snap
		run.seq++
		run.done = true
		run.ended = time.Now()
		run.result, run.compare = res, cmp
		if err != nil {
			run.err = err.Error()
		}
		run.mu.Unlock()
	}()
	return run.id, nil
}

func findLoad(id string) *loadRun {
	loadRuns.Lock()
	defer loadRuns.Unlock()
	return loadRuns.m[id]
}

func registerPerfLoadStream(mux *http.ServeMux, token string) {
	mux.HandleFunc("/api/perf/load/stream", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Mullion-Token")
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		run := findLoad(r.URL.Query().Get("id"))
		if run == nil {
			http.Error(w, "no such load test", http.StatusNotFound)
			return
		}
		rc := http.NewResponseController(w)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		send := func(event string, v any) bool {
			b, err := json.Marshal(v)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		fmt.Fprint(w, "retry: 2000\n\n")
		last := -1
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		// The panel's Shutdown must not wait for this stream.
		ctx := r.Context()
		if srv, ok := ctx.Value(http.ServerContextKey).(*http.Server); ok && srv != nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			srv.RegisterOnShutdown(cancel)
		}
		for {
			run.mu.Lock()
			seq, done := run.seq, run.done
			run.mu.Unlock()
			if seq != last || done {
				last = seq
				ev := "progress"
				if done {
					ev = "done"
				}
				if !send(ev, run.state()) || done {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	})
}

// registerPerf adds the Performance tab's endpoints.
func registerPerf(api apiRegistrar, mux *http.ServeMux, token string) {
	type siteReq struct {
		Site string `json:"site"`
	}
	site := func(r *http.Request) (string, error) {
		var in siteReq
		err := decodeBody(r, &in)
		return in.Site, err
	}
	registerPerfLoadStream(mux, token)

	api("/api/project/perf/info", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		prod, err := a.ProdModeStatus(s)
		if err != nil {
			return nil, err
		}
		startWorkerSampler(a)
		return map[string]any{"prod": prod, "baselines": perf.ListBaselines(a.Paths, s), "readiness": a.LastReadiness(s)}, nil
	})

	// ── routes ──
	api("/api/project/perf/routes", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		res, err := a.PerfRoutes(s)
		if err != nil {
			return nil, err
		}
		return map[string]any{"routes": res, "baselines": perf.ListBaselines(a.Paths, s)}, nil
	})
	api("/api/project/perf/budgets", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site    string       `json:"site"`
			Budgets perf.Budgets `json:"budgets"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetPerfBudgets(in.Site, in.Budgets)
	})
	api("/api/project/perf/reset", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		return nil, a.PerfResetRoutes(s)
	})
	type baselineReq struct {
		Site string `json:"site"`
		Name string `json:"name"`
	}
	api("/api/project/perf/baseline/save", func(a *app.App, r *http.Request) (any, error) {
		var in baselineReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		res, err := a.PerfRoutes(in.Site)
		if err != nil {
			return nil, err
		}
		note := ""
		if res.Prod {
			note = "production-like mode"
		}
		b, err := perf.SaveBaseline(a.Paths, in.Site, in.Name, note, res.Routes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": b.Name, "baselines": perf.ListBaselines(a.Paths, in.Site)}, nil
	})
	api("/api/project/perf/baseline/compare", func(a *app.App, r *http.Request) (any, error) {
		var in baselineReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		b, err := perf.LoadBaseline(a.Paths, in.Site, in.Name)
		if err != nil {
			return nil, err
		}
		res, err := a.PerfRoutes(in.Site)
		if err != nil {
			return nil, err
		}
		return map[string]any{"baseline": perf.BaselineInfo{Name: b.Name, SavedAt: b.SavedAt, Note: b.Note, Routes: len(b.Routes)},
			"deltas": perf.Diff(res.Routes, b.Routes)}, nil
	})
	api("/api/project/perf/baseline/delete", func(a *app.App, r *http.Request) (any, error) {
		var in baselineReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if err := perf.DeleteBaseline(a.Paths, in.Site, in.Name); err != nil {
			return nil, err
		}
		return map[string]any{"baselines": perf.ListBaselines(a.Paths, in.Site)}, nil
	})

	// ── readiness ──
	api("/api/project/perf/check", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		return a.CheckReadiness(ctx, s)
	})
	api("/api/project/perf/ignore", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			ID   string `json:"id"`
			On   bool   `json:"on"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetPerfIgnored(in.Site, in.ID, in.On)
	})

	// ── production-like mode ──
	api("/api/project/perf/prod/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site     string `json:"site"`
			On       bool   `json:"on"`
			Optimize bool   `json:"optimize"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if a.State.FindSite(in.Site) == nil {
			return nil, fmt.Errorf("no site named %q", in.Site)
		}
		id := startCancellable(func(ctx context.Context, setStatus func(string), appendLog func([]byte)) (any, error) {
			if in.On {
				setStatus("Turning production-like mode on…")
			} else {
				setStatus("Turning production-like mode off…")
			}
			na, err := app.New()
			if err != nil {
				return nil, err
			}
			if err := na.SetProdMode(ctx, in.Site, in.On, in.Optimize, appendLog); err != nil {
				return nil, err
			}
			return na.ProdModeStatus(in.Site)
		})
		return map[string]string{"job": id}, nil
	})

	// ── load tests ──
	api("/api/project/perf/load/start", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			app.LoadInput
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		id, err := startLoad(a, in.Site, in.LoadInput)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	})
	api("/api/project/perf/load/stop", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		run := findLoad(in.ID)
		if run == nil {
			return nil, errors.New("that load test isn't running")
		}
		run.cancel()
		return nil, nil
	})
	api("/api/project/perf/load/status", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		run := findLoad(in.ID)
		if run == nil {
			return nil, errors.New("no such load test")
		}
		return run.state(), nil
	})
	api("/api/project/perf/load/history", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		list := perf.LoadResults(a.Paths, s, false)
		type row struct {
			perf.LoadResult
			Compare *perf.LoadCompare `json:"compare,omitempty"`
		}
		out := make([]row, 0, len(list))
		for _, res := range list {
			rw := row{LoadResult: res}
			if prev, ok := perf.Previous(list, res); ok {
				c := perf.CompareLoad(res, prev)
				rw.Compare = &c
			}
			out = append(out, rw)
		}
		// A test still running for the site (the page was reloaded).
		running := ""
		loadRuns.Lock()
		for id, run := range loadRuns.m {
			run.mu.Lock()
			if run.site == s && !run.done {
				running = id
			}
			run.mu.Unlock()
		}
		loadRuns.Unlock()
		return map[string]any{"results": out, "running": running}, nil
	})
	api("/api/project/perf/load/get", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			ID   string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return perf.LoadResultByID(a.Paths, in.Site, in.ID)
	})
	api("/api/project/perf/load/delete", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			ID   string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return nil, perf.DeleteLoadResult(a.Paths, in.Site, in.ID)
	})

	// ── workers ──
	api("/api/project/perf/workers", func(a *app.App, r *http.Request) (any, error) {
		s, err := site(r)
		if err != nil {
			return nil, err
		}
		smp := startWorkerSampler(a)
		// A worker the sampler hasn't measured yet (the panel just
		// started, the worker just did): measure it now rather than show
		// nothing for the first interval.
		var fresh []workers.Target
		for _, t := range a.WorkerTargets() {
			if t.Site == s && len(smp.History(t.Site, t.ID)) == 0 {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) > 0 {
			smp.SampleNow(fresh)
		}
		list, err := a.WorkersHealth(s, smp)
		if err != nil {
			return nil, err
		}
		return map[string]any{"workers": list, "intervalS": int(workerSampleEvery / time.Second)}, nil
	})
}
