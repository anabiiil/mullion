package ui

// The project page's Watch tab: settings, Clear, open-in-editor, the
// Mail list, and the live stream (/api/watch/stream, SSE) of a site's
// requests, logs and PHP events — see internal/watch.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/watch"
)

func registerWatch(api apiRegistrar) {
	type siteReq struct {
		Site string `json:"site"`
	}
	api("/api/project/watch/info", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return a.WatchStatus(in.Site)
	})
	api("/api/project/watch/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site  string       `json:"site"`
			Watch config.Watch `json:"watch"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if err := a.SetWatch(in.Site, in.Watch); err != nil {
			return nil, err
		}
		return a.WatchStatus(in.Site)
	})
	// clear: types set = one view's kinds ("Clear this view"); none =
	// everything, and logs also empties the project's storage/logs/*.log.
	api("/api/project/watch/clear", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site  string   `json:"site"`
			Types []string `json:"types"`
			Logs  bool     `json:"logs"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		emptied := []string{}
		if len(in.Types) > 0 {
			if err := a.ClearWatchTypes(in.Site, in.Types); err != nil {
				return nil, err
			}
		} else {
			files, err := a.ClearWatch(in.Site, in.Logs)
			if err != nil {
				return nil, err
			}
			emptied = append(emptied, files...)
		}
		info, err := a.WatchStatus(in.Site)
		if err != nil {
			return nil, err
		}
		return map[string]any{"emptied": emptied, "info": info}, nil
	})
	api("/api/project/watch/open", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site string `json:"site"`
			File string `json:"file"`
			Line int    `json:"line"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		editor, err := a.WatchOpenFile(in.Site, in.File, in.Line)
		if err != nil {
			return nil, err
		}
		return map[string]string{"editor": editor}, nil
	})
	api("/api/project/watch/mail", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		return a.WatchMail(r.Context(), in.Site)
	})
}

// Live streams: a few at once (each polls a handful of files).
const (
	maxWatchStreams = 8
	watchBacklog    = 300
	watchPoll       = 400 * time.Millisecond
	watchBatchMax   = 500
)

var watchStreams struct {
	sync.Mutex
	n      int
	next   int
	cancel map[int]context.CancelFunc
	hooked map[*http.Server]bool
}

func registerWatchStream(mux *http.ServeMux, token string) {
	mux.HandleFunc("/api/watch/stream", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Mullion-Token")
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		serveWatchStream(w, r)
	})
}

func serveWatchStream(w http.ResponseWriter, r *http.Request) {
	a, err := app.New()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	site := r.URL.Query().Get("site")
	feed, err := a.WatchFeed(site)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	watchStreams.Lock()
	if watchStreams.n >= maxWatchStreams {
		watchStreams.Unlock()
		http.Error(w, "too many live watchers are open — close one first", http.StatusTooManyRequests)
		return
	}
	if watchStreams.cancel == nil {
		watchStreams.cancel = map[int]context.CancelFunc{}
		watchStreams.hooked = map[*http.Server]bool{}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	watchStreams.n++
	watchStreams.next++
	id := watchStreams.next
	watchStreams.cancel[id] = cancel
	// The panel's Shutdown must not wait on an open stream.
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok && srv != nil && !watchStreams.hooked[srv] {
		watchStreams.hooked[srv] = true
		srv.RegisterOnShutdown(func() {
			watchStreams.Lock()
			defer watchStreams.Unlock()
			for _, c := range watchStreams.cancel {
				c()
			}
		})
	}
	watchStreams.Unlock()
	defer func() {
		watchStreams.Lock()
		watchStreams.n--
		delete(watchStreams.cancel, id)
		watchStreams.Unlock()
	}()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	send := func(event string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return rc.Flush()
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	sendItems := func(event string, items []watch.Item) error {
		for len(items) > 0 {
			n := len(items)
			if n > watchBatchMax {
				n = watchBatchMax
			}
			if err := send(event, items[:n]); err != nil {
				return err
			}
			items = items[n:]
			event = "items"
		}
		return nil
	}
	backlog := feed.Backlog(watchBacklog)
	if backlog == nil {
		backlog = []watch.Item{}
	}
	if err := send("start", map[string]any{"settings": feed.Settings(), "count": len(backlog)}); err != nil {
		return
	}
	if err := sendItems("backlog", backlog); err != nil {
		return
	}
	cleared := watch.LoadClear(a.Paths, site)
	tick := time.NewTicker(watchPoll)
	defer tick.Stop()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Another tab (or the CLI) pressed Clear.
			if c := watch.LoadClear(a.Paths, site); c.Stamp() != cleared.Stamp() {
				all := c.At != cleared.At
				cleared = c
				feed.Cleared()
				ev := map[string]any{"at": c.At, "types": c.Types, "all": all}
				if send("cleared", ev) != nil {
					return
				}
			}
			if items := feed.Poll(); len(items) > 0 {
				if sendItems("items", items) != nil {
					return
				}
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			if rc.Flush() != nil {
				return
			}
		}
	}
}
