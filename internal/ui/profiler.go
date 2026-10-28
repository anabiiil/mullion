package ui

// The project page's Performance → Profiler sub-tab (profiler.js,
// mounted by project.js through window.MullionProfilerPanel): SPX on/off
// per site, its stored profiles, and one profile as a flame graph plus
// a top-functions table — see internal/profiler and app/profiler.go.

import (
	"context"
	_ "embed"
	"net/http"
	"time"

	"pm/internal/app"
	"pm/internal/phpver"
	"pm/internal/profiler"
)

//go:embed profiler.js
var profilerJS []byte

//go:embed profiler.css
var profilerCSS []byte

// profilerMinFrac is the share of a profile's time below which a
// flame-graph frame is dropped from the JSON (narrower than a pixel at
// any sensible zoom) — a big Laravel request has ~10k frames, most tiny.
const profilerMinFrac = 0.0002

func registerProfiler(api apiRegistrar, mux *http.ServeMux) {
	serve := func(path, contentType string, data []byte) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		})
	}
	serve("/profiler.js", "text/javascript; charset=utf-8", profilerJS)
	serve("/profiler.css", "text/css; charset=utf-8", profilerCSS)

	type siteReq struct {
		Site string `json:"site"`
	}
	api("/api/project/profiler/info", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return a.ProfilerStatus(in.Site)
	})
	// set runs as a job: turning profiling on may build SPX first.
	api("/api/project/profiler/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site     string        `json:"site"`
			On       bool          `json:"on"`
			Settings profiler.Site `json:"settings"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		build := false
		if in.On {
			if info, err := a.ProfilerStatus(in.Site); err == nil && !info.SpxLoaded {
				build = true
			}
		}
		run := func(setStatus func(string)) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			return a.SetProfiler(ctx, in.Site, in.On, in.Settings, setStatus)
		}
		if build {
			id, err := jobs.startInstall(run)
			if err != nil {
				return nil, err
			}
			return map[string]any{"job": id, "build": true, "spxVersion": phpver.SpxVersion}, nil
		}
		return map[string]any{"job": jobs.start(run)}, nil
	})
	api("/api/project/profiler/list", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return a.ProfilerReports(in.Site)
	})
	api("/api/project/profiler/show", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			ID   string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		m, p, err := a.ProfilerReport(in.Site, in.ID, profilerMinFrac)
		if err != nil {
			return nil, err
		}
		return map[string]any{"meta": m, "profile": p}, nil
	})
	// delete: id "" clears every profile of the site.
	api("/api/project/profiler/delete", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			ID   string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		n, err := a.DeleteProfilerReport(in.Site, in.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"deleted": n}, nil
	})
	api("/api/project/profiler/limits", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string          `json:"site"`
			Lim  profiler.Limits `json:"limits"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if err := a.SetProfilerLimits(in.Lim); err != nil {
			return nil, err
		}
		return a.ProfilerStatus(in.Site)
	})
}
