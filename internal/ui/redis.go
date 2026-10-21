package ui

// The panel's Redis endpoints: the Databases page's Redis tab, the
// project page's Cache tab (keys under the project's prefix), and the
// live MONITOR stream (Server-Sent Events).

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pm/internal/app"
	"pm/internal/phpver"
	"pm/internal/redis"
	"pm/internal/redisadmin"
)

// redisExtCache remembers whether the global PHP has the redis
// extension: finding out runs `php -m`, too slow for every poll.
var redisExtCache struct {
	sync.Mutex
	php string
	has bool
	at  time.Time
}

func cachedRedisExt(a *app.App) bool {
	v := a.State.Config.GlobalPHP
	if v == "" {
		return false
	}
	redisExtCache.Lock()
	defer redisExtCache.Unlock()
	if redisExtCache.php == v && time.Since(redisExtCache.at) < 20*time.Second {
		return redisExtCache.has
	}
	has := false
	if exts, err := phpver.ListExtensions(a.Paths, v); err == nil {
		for _, e := range exts {
			if e.Name == "redis" && e.Enabled {
				has = true
			}
		}
	}
	redisExtCache.php, redisExtCache.has, redisExtCache.at = v, has, time.Now()
	return has
}

func forgetRedisExt() {
	redisExtCache.Lock()
	redisExtCache.at = time.Time{}
	redisExtCache.Unlock()
}

// redisState is getState's "redis" entry: cheap (a port probe and, when
// up, one INFO keyspace).
func redisState(a *app.App) map[string]any {
	c := a.State.Config
	st := map[string]any{"installed": c.Redis != "", "platform": runtime.GOOS}
	if c.Redis == "" {
		return st
	}
	running := redis.Running()
	st["version"] = c.Redis
	st["label"] = redis.Label(c.Redis)
	st["running"] = running
	st["owned"] = running && redis.Owned(a.Paths)
	st["stopped"] = c.RedisStopped
	st["port"] = redis.Port
	st["env"] = app.RedisEnvSnippet(redis.Port)
	st["adminUrl"] = a.RedisAdminURL(-1)
	st["adminFiles"] = redisadmin.Installed(a.Paths)
	st["globalPhp"] = c.GlobalPHP
	st["phpExt"] = cachedRedisExt(a)
	return st
}

// redisStats is the Redis tab's live numbers (INFO).
type redisStats struct {
	Version     string             `json:"version"`
	Uptime      int64              `json:"uptime"`
	Clients     int64              `json:"clients"`
	UsedMemory  int64              `json:"usedMemory"`
	MemoryHuman string             `json:"memoryHuman"`
	PeakHuman   string             `json:"peakHuman"`
	MaxMemory   int64              `json:"maxMemory"`
	Policy      string             `json:"policy"`
	OpsPerSec   int64              `json:"opsPerSec"`
	Hits        int64              `json:"hits"`
	Misses      int64              `json:"misses"`
	HitRate     float64            `json:"hitRate"` // -1 = no lookups yet
	Commands    int64              `json:"commands"`
	Expired     int64              `json:"expired"`
	Evicted     int64              `json:"evicted"`
	LastSave    int64              `json:"lastSave"`
	Changes     int64              `json:"changes"`
	Keyspace    []redis.KeyspaceDB `json:"keyspace"`
	Databases   int                `json:"databases"`
}

func infoInt(info map[string]string, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(info[k]), 10, 64)
	return n
}

func readRedisStats(a *app.App) (redisStats, error) {
	c, err := a.RedisClient()
	if err != nil {
		return redisStats{}, err
	}
	defer c.Close()
	info, err := c.Info("")
	if err != nil {
		return redisStats{}, err
	}
	s := redisStats{
		Version: info["redis_version"], Uptime: infoInt(info, "uptime_in_seconds"), Clients: infoInt(info, "connected_clients"),
		UsedMemory: infoInt(info, "used_memory"), MemoryHuman: info["used_memory_human"], PeakHuman: info["used_memory_peak_human"],
		MaxMemory: infoInt(info, "maxmemory"), Policy: info["maxmemory_policy"], OpsPerSec: infoInt(info, "instantaneous_ops_per_sec"),
		Hits: infoInt(info, "keyspace_hits"), Misses: infoInt(info, "keyspace_misses"), Commands: infoInt(info, "total_commands_processed"),
		Expired: infoInt(info, "expired_keys"), Evicted: infoInt(info, "evicted_keys"), LastSave: infoInt(info, "rdb_last_save_time"),
		Changes: infoInt(info, "rdb_changes_since_last_save"), Keyspace: redis.ParseKeyspace(info), Databases: redis.Databases,
	}
	s.HitRate = -1
	if s.Hits+s.Misses > 0 {
		s.HitRate = float64(s.Hits) * 100 / float64(s.Hits+s.Misses)
	}
	if s.Keyspace == nil {
		s.Keyspace = []redis.KeyspaceDB{}
	}
	return s, nil
}

// panelText makes binary-unsafe Redis data JSON-friendly: invalid UTF-8
// becomes \xHH escapes so nothing is silently mangled.
func panelText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func panelPreview(p redis.Preview) redis.Preview {
	p.Key = panelText(p.Key)
	p.Value = panelText(p.Value)
	for i := range p.Items {
		p.Items[i] = panelText(p.Items[i])
	}
	for i := range p.Fields {
		p.Fields[i].Name, p.Fields[i].Value = panelText(p.Fields[i].Name), panelText(p.Fields[i].Value)
	}
	return p
}

func registerRedis(api apiRegistrar) {
	installJob := func(fn func(a2 *app.App, setStatus func(string)) (any, error)) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return fn(a2, setStatus)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	}
	decode := func(r *http.Request, v any) error {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil && err.Error() != "EOF" {
			return err
		}
		return nil
	}

	api("/api/redis/install", func(a *app.App, r *http.Request) (any, error) {
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			v, err := a2.InstallRedis(context.Background(), func(s string) { setStatus(s) })
			if err != nil {
				return nil, err
			}
			return redis.Label(v), nil
		})
	})
	api("/api/redis/update", func(a *app.App, r *http.Request) (any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		return map[string]string{"latest": a.RedisUpdate(ctx)}, nil
	})
	api("/api/redis/start", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StartRedis()
	})
	api("/api/redis/stop", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StopRedis()
	})
	api("/api/redis/restart", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.RestartRedis()
	})
	api("/api/redis/stats", func(a *app.App, r *http.Request) (any, error) {
		return readRedisStats(a)
	})
	api("/api/redis/flush", func(a *app.App, r *http.Request) (any, error) {
		in := struct{ DB *int }{}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		db := -1
		if in.DB != nil {
			db = *in.DB
		}
		return nil, a.FlushRedis(db)
	})
	// /api/redis/admin/open links phpRedisAdmin (downloading it the
	// first time) as a job whose result is its URL, on database db when
	// given (the page opens an already-linked one directly).
	api("/api/redis/admin/open", func(a *app.App, r *http.Request) (any, error) {
		in := struct{ DB *int }{}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		db := -1
		if in.DB != nil {
			db = *in.DB
		}
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Downloading phpRedisAdmin…")
			a2.Progress = setStatus
			return a2.EnsureRedisAdmin(context.Background(), db)
		})
	})
	api("/api/redis/restore-file", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Path string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Restoring the snapshot (Redis restarts)…")
			return nil, a2.RestoreRedisFile(in.Path)
		})
	})
	api("/api/redis/php-ext", func(a *app.App, r *http.Request) (any, error) {
		v := a.State.Config.GlobalPHP
		if v == "" {
			return nil, errors.New("no global PHP — install one on the PHP page first")
		}
		if runtime.GOOS != "windows" {
			return nil, errors.New("this PHP build has the redis extension compiled in")
		}
		return installJob(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Getting the redis extension for PHP " + v + " from PECL…")
			defer forgetRedisExt()
			if err := phpver.InstallPeclExtension(context.Background(), a2.Paths, v, "redis"); err != nil {
				return nil, err
			}
			return nil, a2.RestartPhp(v)
		})
	})

	/* ── project page: Cache tab ── */
	type siteIn struct {
		Site   string
		DB     int
		Key    string
		Q      string
		Cursor string
		Count  int
		Keys   []string
	}
	api("/api/project/cache/info", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return a.ProjectCache(in.Site)
	})
	api("/api/project/cache/stats", func(a *app.App, r *http.Request) (any, error) {
		return readRedisStats(a)
	})
	api("/api/project/cache/keys", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		page, err := a.ProjectKeys(in.Site, in.DB, in.Q, in.Cursor, in.Count)
		if err != nil {
			return nil, err
		}
		for i := range page.Keys {
			page.Keys[i].Key = panelText(page.Keys[i].Key)
		}
		return page, nil
	})
	api("/api/project/cache/key", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		p, err := a.ProjectKeyPreview(in.Site, in.DB, in.Key)
		if err != nil {
			return nil, err
		}
		return panelPreview(p), nil
	})
	api("/api/project/cache/delete", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return nil, a.DeleteProjectKey(in.Site, in.DB, in.Key)
	})
	api("/api/project/cache/flush", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		n, err := a.FlushProjectCache(in.Site)
		return map[string]int64{"deleted": n}, err
	})
	api("/api/project/cache/use-redis", func(a *app.App, r *http.Request) (any, error) {
		var in siteIn
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if len(in.Keys) == 0 {
			return nil, errors.New("pick at least one setting to change")
		}
		backup, err := a.UseRedisInProject(in.Site, in.Keys)
		return map[string]string{"backup": backup}, err
	})
}

/* ── live MONITOR over Server-Sent Events ───────────────────── */

const (
	// monitorDefaultMinutes / monitorMaxMinutes bound a monitor session:
	// MONITOR costs the server a little on every command, so it never
	// runs unattended for long.
	monitorDefaultMinutes = 5
	monitorMaxMinutes     = 30
	// maxMonitors caps concurrent MONITOR connections.
	maxMonitors = 3
	// monitorRate caps events sent per second; the rest are counted.
	monitorRate = 200
)

var monitors struct {
	sync.Mutex
	n      int
	cancel map[int]context.CancelFunc
	next   int
	hooked map[*http.Server]bool
}

// monitorFilter decides which MONITOR events a stream shows.
type monitorFilter struct {
	dbs    map[int]bool // empty = all
	prefix string       // "" = every key
}

// match reports whether an event belongs to the stream: not one of the
// panel's own connections, in one of the databases, and — with a
// prefix — touching a key that starts with it (or a command with no
// key at all, like FLUSHDB, on one of those databases).
func (f monitorFilter) match(ev redis.MonitorEvent) bool {
	if redis.IsPanelAddr(ev.Addr) || len(ev.Args) == 0 {
		return false
	}
	if len(f.dbs) > 0 && !f.dbs[ev.DB] {
		return false
	}
	if f.prefix == "" {
		return true
	}
	cmd := strings.ToUpper(ev.Args[0])
	switch cmd {
	case "SELECT", "PING", "AUTH", "CLIENT", "HELLO", "INFO", "MULTI", "EXEC", "DISCARD", "WATCH", "UNWATCH", "QUIT":
		return false
	case "FLUSHDB", "FLUSHALL":
		return true
	}
	for _, a := range ev.Args[1:] {
		if strings.HasPrefix(a, f.prefix) {
			return true
		}
	}
	return false
}

func registerRedisMonitor(mux *http.ServeMux, token string) {
	mux.HandleFunc("/api/redis/monitor", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Mullion-Token")
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		serveMonitor(w, r)
	})
}

func serveMonitor(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minutes, _ := strconv.Atoi(q.Get("minutes"))
	if minutes <= 0 {
		minutes = monitorDefaultMinutes
	}
	if minutes > monitorMaxMinutes {
		minutes = monitorMaxMinutes
	}
	a, err := app.New()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f := monitorFilter{dbs: map[int]bool{}}
	if site := q.Get("site"); site != "" {
		set, err := a.ProjectRedisSettings(site)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.prefix = set.Prefix
		f.dbs[set.DB], f.dbs[set.CacheDB] = true, true
	}
	if s := q.Get("db"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.dbs = map[int]bool{n: true}
		}
	}

	monitors.Lock()
	if monitors.n >= maxMonitors {
		monitors.Unlock()
		http.Error(w, "too many live monitors are open — close one first", http.StatusTooManyRequests)
		return
	}
	if monitors.cancel == nil {
		monitors.cancel = map[int]context.CancelFunc{}
		monitors.hooked = map[*http.Server]bool{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(minutes)*time.Minute)
	defer cancel()
	monitors.n++
	monitors.next++
	id := monitors.next
	monitors.cancel[id] = cancel
	// The panel's Shutdown must not wait on an open stream.
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok && srv != nil && !monitors.hooked[srv] {
		monitors.hooked[srv] = true
		srv.RegisterOnShutdown(func() {
			monitors.Lock()
			defer monitors.Unlock()
			for _, c := range monitors.cancel {
				c()
			}
		})
	}
	monitors.Unlock()
	defer func() {
		monitors.Lock()
		monitors.n--
		delete(monitors.cancel, id)
		monitors.Unlock()
	}()

	c, err := a.RedisClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer c.Close()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		rc.Flush()
	}
	// No automatic reconnect: a monitor that ended (time limit, Redis
	// stopped) stays ended until the user starts it again.
	fmt.Fprint(w, "retry: 86400000\n\n")
	send("start", map[string]any{"minutes": minutes, "prefix": panelText(f.prefix), "until": time.Now().Add(time.Duration(minutes) * time.Minute).Unix()})

	type outEvent struct {
		Time float64 `json:"time"`
		DB   int     `json:"db"`
		Addr string  `json:"addr"`
		Cmd  string  `json:"cmd"`
		Text string  `json:"text"`
	}
	var (
		mu      sync.Mutex
		pending []outEvent
		dropped int
	)
	flushTick := time.NewTicker(250 * time.Millisecond)
	defer flushTick.Stop()
	done := make(chan error, 1)
	go func() {
		done <- c.Monitor(ctx, func(ev redis.MonitorEvent) bool {
			if !f.match(ev) {
				return true
			}
			mu.Lock()
			if len(pending) >= monitorRate/4 {
				dropped++
			} else {
				pending = append(pending, outEvent{Time: ev.Time, DB: ev.DB, Addr: ev.Addr,
					Cmd: strings.ToUpper(ev.Args[0]), Text: panelText(redis.FormatArgs(ev.Args, 300))})
			}
			mu.Unlock()
			return true
		})
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case err := <-done:
			reason := "time limit reached"
			if r.Context().Err() != nil {
				return
			}
			if err != nil {
				reason = "Redis connection closed: " + err.Error()
			}
			send("end", map[string]string{"reason": reason})
			return
		case <-flushTick.C:
			mu.Lock()
			batch, d := pending, dropped
			pending, dropped = nil, 0
			mu.Unlock()
			if len(batch) > 0 || d > 0 {
				send("cmds", map[string]any{"events": batch, "dropped": d})
			}
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			rc.Flush()
		}
	}
}
