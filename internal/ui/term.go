package ui

// The control panel's built-in terminal: shell sessions on
// pseudo-terminals (internal/pty), streamed to the page over one
// Server-Sent Events connection, plus a fast, spawn-free path/command
// completion endpoint for the suggestion popup. The front end is
// terminal.js/terminal.css with a vendored xterm.js (vendor/).

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/pty"
)

//go:embed terminal.js terminal.css vendor
var termAssets embed.FS

const (
	termMaxSessions = 12
	termReplayBytes = 256 << 10
	termCoalesce    = 8 * time.Millisecond
	termBurst       = 4 << 10 // flushes at least this big count as a flood
	termDeadTTL     = 2 * time.Minute
)

// registerTerminal adds the terminal's assets and /api/term/* endpoints.
func registerTerminal(mux *http.ServeMux, token string) {
	serveAsset := func(urlPath, file string) {
		mux.HandleFunc(urlPath, func(w http.ResponseWriter, r *http.Request) {
			data, err := termAssets.ReadFile(file)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			ct := mime.TypeByExtension(filepath.Ext(file))
			if ct == "" {
				ct = "text/plain; charset=utf-8"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		})
	}
	serveAsset("/terminal.js", "terminal.js")
	serveAsset("/terminal.css", "terminal.css")
	vendor, _ := fs.ReadDir(termAssets, "vendor")
	for _, e := range vendor {
		serveAsset("/vendor/"+e.Name(), "vendor/"+e.Name())
	}

	// auth accepts the panel token from the X-Mullion-Token header, or —
	// on the stream endpoint only, because EventSource can't send
	// headers — from the t query parameter.
	auth := func(r *http.Request, allowQuery bool) bool {
		got := r.Header.Get("X-Mullion-Token")
		if got == "" && allowQuery {
			got = r.URL.Query().Get("t")
		}
		return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	api := func(path string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if !auth(r, false) {
				http.Error(w, "bad token", http.StatusForbidden)
				return
			}
			terms.hookShutdown(r)
			data, err := h(r)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
		})
	}

	api("/api/term/open", func(r *http.Request) (any, error) {
		var req struct {
			Cwd   string `json:"cwd"`
			Site  string `json:"site"`
			Title string `json:"title"`
			Cols  uint16 `json:"cols"`
			Rows  uint16 `json:"rows"`
		}
		if err := decodeBody(r, &req); err != nil {
			return nil, err
		}
		s, err := terms.open(req.Cwd, req.Site, req.Title, req.Cols, req.Rows)
		if err != nil {
			return nil, err
		}
		return s.info(), nil
	})
	api("/api/term/input", func(r *http.Request) (any, error) {
		var req struct {
			ID   string `json:"id"`
			Data string `json:"data"`
		}
		if err := decodeBody(r, &req); err != nil {
			return nil, err
		}
		s := terms.get(req.ID)
		if s == nil {
			return nil, errors.New("no such terminal")
		}
		if !s.isAlive() {
			return nil, errors.New("terminal has exited")
		}
		if strings.ContainsAny(req.Data, "\r\n") {
			s.mu.Lock()
			s.cwdAt = time.Time{} // a command ran: the shell may have changed directory
			s.mu.Unlock()
		}
		_, err := s.p.Write([]byte(req.Data))
		return nil, err
	})
	api("/api/term/resize", func(r *http.Request) (any, error) {
		var req struct {
			ID   string `json:"id"`
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if err := decodeBody(r, &req); err != nil {
			return nil, err
		}
		s := terms.get(req.ID)
		if s == nil {
			return nil, errors.New("no such terminal")
		}
		if !s.isAlive() {
			return nil, nil
		}
		return nil, s.p.Resize(req.Cols, req.Rows)
	})
	api("/api/term/close", func(r *http.Request) (any, error) {
		var req struct {
			ID string `json:"id"`
		}
		if err := decodeBody(r, &req); err != nil {
			return nil, err
		}
		terms.close(req.ID)
		return nil, nil
	})
	api("/api/term/list", func(r *http.Request) (any, error) {
		return terms.list(), nil
	})
	api("/api/term/complete", func(r *http.Request) (any, error) {
		var req struct {
			ID    string `json:"id"`
			Cwd   string `json:"cwd"`
			Site  string `json:"site"`
			Input string `json:"input"`
		}
		if err := decodeBody(r, &req); err != nil {
			return nil, err
		}
		cwd, path := req.Cwd, os.Getenv("PATH")
		if s := terms.get(req.ID); s != nil {
			cwd, path = s.currentCwd(), s.path
		}
		if cwd == "" {
			cwd = homeDir()
		}
		return complete(cwd, req.Input, path), nil
	})

	mux.HandleFunc("/api/term/stream", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r, true) {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		terms.hookShutdown(r)
		terms.stream(w, r)
	})
}

func decodeBody(r *http.Request, v any) error {
	if r.Method != http.MethodPost {
		return errors.New("POST required")
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

// ── sessions ─────────────────────────────────────────────────────────

type termSession struct {
	id, title, cwd, site, shell string
	created                     time.Time
	p                           *pty.PTY
	meta                        termMeta
	path                        string // the PATH the shell started with (for command completion)

	mu       sync.Mutex
	ring     []byte // the most recent output, at most termReplayBytes
	end      int64  // total bytes of output ever produced
	alive    bool
	exitCode int
	diedAt   time.Time

	cwdAt     time.Time // cache for currentCwd
	cwdCached string
	tail      string // Windows: recent plain-text output, for prompt scraping
	promptCwd string
}

type termInfo struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Cwd      string `json:"cwd"`
	Site     string `json:"site,omitempty"`
	Shell    string `json:"shell"`
	Alive    bool   `json:"alive"`
	ExitCode int    `json:"exitCode"`
	OS       string `json:"os"`
	Kind     string `json:"kind,omitempty"` // the site's kind: php | node | static
	PHP      string `json:"php,omitempty"`  // PHP version on PATH first
	Node     string `json:"node,omitempty"` // the project's Node version
}

type termMeta struct{ site, kind, php, node string }

func (s *termSession) info() termInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return termInfo{ID: s.id, Title: s.title, Cwd: s.cwd, Site: s.site, Shell: filepath.Base(s.shell),
		Alive: s.alive, ExitCode: s.exitCode, OS: runtime.GOOS,
		Kind: s.meta.kind, PHP: s.meta.php, Node: s.meta.node}
}

func (s *termSession) isAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

// currentCwd is where the shell is now (for completions): the kernel's
// answer on macOS/Linux, cached for a second; on Windows the directory
// in the last PowerShell prompt; else where the session started.
func (s *termSession) currentCwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.cwdAt) < time.Second && s.cwdCached != "" {
		return s.cwdCached
	}
	cwd := s.cwd
	if s.alive {
		if c, err := pty.Cwd(s.p.Pid()); err == nil && c != "" {
			cwd = c
		} else if s.promptCwd != "" {
			cwd = s.promptCwd
		}
	}
	s.cwdAt, s.cwdCached = time.Now(), cwd
	return cwd
}

var ansiRe = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)
var psPromptRe = regexp.MustCompile(`PS ([A-Za-z]:\\[^>\r\n]*|\\\\[^>\r\n]+)> ?`)

func (s *termSession) scrapePrompt(chunk []byte) {
	s.tail += ansiRe.ReplaceAllString(string(chunk), "")
	if len(s.tail) > 2048 {
		s.tail = s.tail[len(s.tail)-2048:]
	}
	if m := psPromptRe.FindAllStringSubmatch(s.tail, -1); len(m) > 0 {
		s.promptCwd = m[len(m)-1][1]
		s.cwdAt = time.Time{}
	}
}

type termManager struct {
	mu       sync.Mutex
	sessions map[string]*termSession
	order    []string
	notify   chan struct{} // closed (and replaced) whenever anything changes
	hooked   map[*http.Server]bool
	stopped  chan struct{}
	stopOnce sync.Once
}

var terms = &termManager{
	sessions: map[string]*termSession{},
	notify:   make(chan struct{}),
	hooked:   map[*http.Server]bool{},
	stopped:  make(chan struct{}),
}

// changed wakes every stream. Callers must hold m.mu.
func (m *termManager) changedLocked() {
	close(m.notify)
	m.notify = make(chan struct{})
}

func (m *termManager) changed() {
	m.mu.Lock()
	m.changedLocked()
	m.mu.Unlock()
}

// hookShutdown makes the http.Server's Shutdown end every stream (an
// open SSE response would otherwise hold Shutdown forever) and hang up
// every shell: sessions live exactly as long as the panel process.
func (m *termManager) hookShutdown(r *http.Request) {
	srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server)
	if !ok || srv == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hooked[srv] {
		return
	}
	m.hooked[srv] = true
	srv.RegisterOnShutdown(m.shutdown)
}

func (m *termManager) shutdown() {
	m.stopOnce.Do(func() { close(m.stopped) })
	m.mu.Lock()
	all := make([]*termSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		go s.p.Close()
	}
}

func (m *termManager) get(id string) *termSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *termManager) list() []termInfo {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	out := []termInfo{}
	for _, id := range ids {
		if s := m.get(id); s != nil {
			out = append(out, s.info())
		}
	}
	return out
}

func (m *termManager) close(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	if s != nil {
		delete(m.sessions, id)
		for i, v := range m.order {
			if v == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		m.changedLocked()
	}
	m.mu.Unlock()
	if s != nil {
		go s.p.Close()
	}
}

func (m *termManager) open(cwd, siteName, title string, cols, rows uint16) (*termSession, error) {
	m.mu.Lock()
	alive := 0
	for _, s := range m.sessions {
		if s.isAlive() {
			alive++
		}
	}
	m.mu.Unlock()
	if alive >= termMaxSessions {
		return nil, fmt.Errorf("too many terminals open (max %d) — close one first", termMaxSessions)
	}

	env, cwd, meta, err := termEnv(cwd, siteName)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = meta.site
		if title == "" {
			title = filepath.Base(cwd)
			if cwd == homeDir() {
				title = "~"
			}
		}
	}
	shell, args := pty.DefaultShell()
	args, env = wrapShell(shell, args, env)
	if cols == 0 || rows == 0 {
		cols, rows = 100, 30
	}
	p, err := pty.Start(shell, args, cwd, env, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", filepath.Base(shell), err)
	}
	s := &termSession{
		id: newTermID(), title: title, cwd: cwd, site: meta.site, shell: shell, meta: meta, path: envPath(env),
		created: time.Now(), p: p, alive: true,
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.order = append(m.order, s.id)
	m.changedLocked()
	m.mu.Unlock()

	go m.pump(s)
	go pathExecutables(s.path) // warm the command-completion cache
	return s, nil
}

// pump copies the shell's output into the session's replay ring and
// wakes the streams, then records the exit and schedules cleanup.
func (m *termManager) pump(s *termSession) {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.p.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.ring = append(s.ring, buf[:n]...)
			if len(s.ring) > 2*termReplayBytes {
				s.ring = append([]byte(nil), s.ring[len(s.ring)-termReplayBytes:]...)
			}
			s.end += int64(n)
			if runtime.GOOS == "windows" {
				s.scrapePrompt(buf[:n])
			}
			s.mu.Unlock()
			m.changed()
		}
		if err != nil {
			break
		}
	}
	code, _ := s.p.Wait()
	s.p.Close()
	s.mu.Lock()
	s.alive, s.exitCode, s.diedAt = false, code, time.Now()
	s.mu.Unlock()
	m.changed()

	time.AfterFunc(termDeadTTL, func() {
		if m.get(s.id) == s {
			m.close(s.id)
		}
	})
}

func newTermID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ── streaming ────────────────────────────────────────────────────────

// stream serves every session's output (or one session's, with ?id=)
// as Server-Sent Events on a single connection — one connection for all
// tabs, since a browser allows only ~6 per host. Each event's id is the
// vector of per-session offsets ("id:offset,…"), so an EventSource that
// reconnects (it resends it as Last-Event-ID) resumes exactly where it
// left off, while a fresh one replays each session's recent history.
//
// Events: out {s, d: base64}, exit {s, code}, gone {s}. Output is
// coalesced: once output is flowing (a large flush, or flushes landing
// back to back) the next one waits until termCoalesce after the last,
// so a flood (a build log, `seq 1 100000`) becomes a few large events,
// while an isolated small write — a keystroke echo — goes out at once.
func (m *termManager) stream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	only := r.URL.Query().Get("id")
	offsets := map[string]int64{}
	exited := map[string]bool{}
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("last")
	}
	for _, part := range strings.Split(last, ",") {
		id, off, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(off, 10, 64); err == nil {
			offsets[id] = n
		}
	}

	fmt.Fprint(w, "retry: 1000\n\n")
	rc.Flush()

	var lastFlush time.Time
	lastSize, streak := 0, 0
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		m.mu.Lock()
		wake := m.notify
		var list []*termSession
		for _, id := range m.order {
			if only == "" || id == only {
				list = append(list, m.sessions[id])
			}
		}
		m.mu.Unlock()

		var b strings.Builder
		live := map[string]bool{}
		for _, s := range list {
			live[s.id] = true
			s.mu.Lock()
			start := s.end - int64(len(s.ring))
			off, seen := offsets[s.id]
			if !seen || off < start {
				off = start
			}
			if off > s.end { // stale id from an earlier panel process
				off = start
			}
			var chunk []byte
			if off < s.end {
				chunk = s.ring[len(s.ring)-int(s.end-off):]
			}
			offsets[s.id] = s.end
			alive, code := s.alive, s.exitCode
			if len(chunk) > 0 {
				d, _ := json.Marshal(map[string]string{"s": s.id, "d": base64.StdEncoding.EncodeToString(chunk)})
				b.WriteString("event: out\ndata: ")
				b.Write(d)
				b.WriteString("\n")
				b.WriteString("id: " + offsetVector(offsets) + "\n\n")
			}
			s.mu.Unlock()
			if !alive && !exited[s.id] {
				exited[s.id] = true
				fmt.Fprintf(&b, "event: exit\ndata: {\"s\":%q,\"code\":%d}\n\n", s.id, code)
			}
		}
		for id := range offsets {
			if !live[id] {
				delete(offsets, id)
				fmt.Fprintf(&b, "event: gone\ndata: {\"s\":%q}\n\n", id)
			}
		}
		if b.Len() > 0 {
			if _, err := w.Write([]byte(b.String())); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			if time.Since(lastFlush) < termCoalesce {
				streak++
			} else {
				streak = 0
			}
			lastFlush, lastSize = time.Now(), b.Len()
		}

		select {
		case <-wake:
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			rc.Flush()
			continue
		case <-r.Context().Done():
			return
		case <-m.stopped:
			return
		}
		if wait := termCoalesce - time.Since(lastFlush); wait > 0 && (lastSize >= termBurst || streak >= 2) {
			select {
			case <-time.After(wait):
			case <-r.Context().Done():
				return
			case <-m.stopped:
				return
			}
		}
	}
}

func offsetVector(offsets map[string]int64) string {
	ids := make([]string, 0, len(offsets))
	for id := range offsets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ":" + strconv.FormatInt(offsets[id], 10)
	}
	return strings.Join(parts, ",")
}

// ── environment ──────────────────────────────────────────────────────

// termEnv resolves where a new terminal starts and its environment:
// the panel's own environment with Mullion's tools first on PATH — and,
// for a project, that project's PHP and Node versions ahead of them.
// A cwd inside a linked site's folder counts as opening that site.
func termEnv(cwd, siteName string) (env []string, dir string, meta termMeta, err error) {
	a, err := app.New()
	if err != nil {
		return nil, "", meta, err
	}
	var s *config.Site
	for i := range a.State.Sites {
		if siteName != "" && a.State.Sites[i].Name == siteName {
			s = &a.State.Sites[i]
			break
		}
	}
	if siteName != "" && s == nil {
		return nil, "", meta, fmt.Errorf("no linked site named %q", siteName)
	}
	if s != nil && cwd == "" {
		cwd = s.Path
	}
	if cwd == "" {
		cwd = homeDir()
	}
	if strings.HasPrefix(cwd, "~") {
		cwd = filepath.Join(homeDir(), cwd[1:])
	}
	cwd = filepath.Clean(cwd)
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, "", meta, fmt.Errorf("folder not found: %s", cwd)
	}
	if s == nil {
		for i := range a.State.Sites {
			p := filepath.Clean(a.State.Sites[i].Path)
			if cwd == p || strings.HasPrefix(cwd, p+string(filepath.Separator)) {
				s = &a.State.Sites[i]
				break
			}
		}
	}

	var dirs []string
	phprc := ""
	if s != nil {
		meta.site, meta.kind = s.Name, s.Kind
		if meta.kind == "" {
			meta.kind = "php"
		}
		if s.IsPHP() {
			if v := a.SiteVersion(*s); v != "" {
				if d := a.Paths.PhpVersionDir(v); isDir(d) {
					dirs = append(dirs, d)
					phprc = d
					meta.php = v
				}
			}
		}
		if d, err := a.NodeVersionDirFor(*s); err == nil {
			dirs = append(dirs, nodever.BinDir(d))
			meta.node = filepath.Base(d)
		}
	}
	if meta.php == "" {
		meta.php = a.State.Config.GlobalPHP
	}
	dirs = append(dirs, a.Paths.BinDir())
	if d := a.Paths.CurrentPhp(); isDir(d) {
		dirs = append(dirs, d)
		if phprc == "" {
			phprc = d
		}
	}

	env = os.Environ()
	pathKey, pathVal := "PATH", ""
	kept := env[:0:0]
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PATH") {
			pathKey, pathVal = k, v
			continue
		}
		switch strings.ToUpper(k) {
		case "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "MULLION_UI_URL":
			continue
		}
		kept = append(kept, kv)
	}
	prepend := strings.Join(dirs, string(os.PathListSeparator))
	newPath := prepend
	if pathVal != "" {
		newPath += string(os.PathListSeparator) + pathVal
	}
	env = append(kept, pathKey+"="+newPath, "TERM_PROGRAM=Mullion", "MULLION_TERM_PREPEND="+prepend)
	if phprc != "" {
		env = append(env, "PHPRC="+phprc, "MULLION_TERM_PHPRC="+phprc)
	}
	if meta.site != "" {
		env = append(env, "MULLION_SITE="+meta.site)
	}
	return env, cwd, meta, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Login shells rebuild PATH from the user's profile — which puts the
// GLOBAL php (Mullion's own profile block) back in front of the
// project's version we prepended. So zsh and bash start through tiny
// wrapper startup files that load the user's real ones and then
// re-apply MULLION_TERM_PREPEND last (the approach VS Code uses for
// its shell integration). History, prompts and plugins are untouched.
const zshEnvWrapper = `# Mullion terminal: load the user's zsh config, then re-apply the project PATH.
MULLION_ZDOTDIR="$ZDOTDIR"
ZDOTDIR="${MULLION_USER_ZDOTDIR:-$HOME}"
[ -f "$ZDOTDIR/.zshenv" ] && source "$ZDOTDIR/.zshenv"
MULLION_USER_ZDOTDIR="$ZDOTDIR"
ZDOTDIR="$MULLION_ZDOTDIR"
`

func zshWrapper(name string, last bool) string {
	s := `ZDOTDIR="$MULLION_USER_ZDOTDIR"
`
	if name == ".zshrc" {
		// /etc/zshrc derived HISTFILE from our ZDOTDIR; point it home.
		s += `[[ "$HISTFILE" == "$MULLION_ZDOTDIR/"* ]] && HISTFILE="$MULLION_USER_ZDOTDIR/.zsh_history"
`
	}
	s += `[ -f "$ZDOTDIR/` + name + `" ] && source "$ZDOTDIR/` + name + `"
`
	if last {
		s += `[ -n "$MULLION_TERM_PREPEND" ] && export PATH="$MULLION_TERM_PREPEND:$PATH"
[ -n "$MULLION_TERM_PHPRC" ] && export PHPRC="$MULLION_TERM_PHPRC"
unset MULLION_ZDOTDIR MULLION_USER_ZDOTDIR MULLION_TERM_PREPEND MULLION_TERM_PHPRC
`
	} else {
		s += `MULLION_USER_ZDOTDIR="$ZDOTDIR"
ZDOTDIR="$MULLION_ZDOTDIR"
`
	}
	return s
}

const bashWrapper = `# Mullion terminal: act as a login shell, then re-apply the project PATH.
[ -f /etc/profile ] && . /etc/profile
for f in ~/.bash_profile ~/.bash_login ~/.profile; do [ -f "$f" ] && { . "$f"; break; }; done
[ -n "$MULLION_TERM_PREPEND" ] && export PATH="$MULLION_TERM_PREPEND:$PATH"
[ -n "$MULLION_TERM_PHPRC" ] && export PHPRC="$MULLION_TERM_PHPRC"
unset MULLION_TERM_PREPEND MULLION_TERM_PHPRC
`

var wrapOnce sync.Once
var wrapDir string

func wrapShell(shell string, args, env []string) ([]string, []string) {
	if runtime.GOOS == "windows" {
		return args, env
	}
	wrapOnce.Do(func() {
		a, err := app.New()
		if err != nil {
			return
		}
		dir := filepath.Join(a.Paths.TmpDir(), "term-shell")
		if os.MkdirAll(dir, 0o700) != nil {
			return
		}
		files := map[string]string{
			".zshenv":   zshEnvWrapper,
			".zprofile": zshWrapper(".zprofile", false),
			".zshrc":    zshWrapper(".zshrc", false),
			// Login zsh reads .zlogin last — the PATH goes on there.
			".zlogin": zshWrapper(".zlogin", true),
			"bashrc":  bashWrapper,
		}
		for name, body := range files {
			if os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600) != nil {
				return
			}
		}
		wrapDir = dir
	})
	if wrapDir == "" {
		return args, env
	}
	switch filepath.Base(shell) {
	case "zsh":
		user := ""
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "ZDOTDIR="); ok {
				user = v
			}
		}
		env = append(env, "ZDOTDIR="+wrapDir)
		if user != "" {
			env = append(env, "MULLION_USER_ZDOTDIR="+user)
		}
		return []string{"-l"}, env
	case "bash":
		return []string{"--rcfile", filepath.Join(wrapDir, "bashrc"), "-i"}, env
	}
	return args, env
}

// ── completion ───────────────────────────────────────────────────────

type complItem struct {
	Label  string `json:"label"`
	Insert string `json:"insert"`
	Kind   string `json:"kind"` // dir | file | cmd | script
	Detail string `json:"detail,omitempty"`
	common bool   // a built-in / everyday command: ranked ahead of the rest
}

type complResult struct {
	ReplaceFrom int         `json:"replaceFrom"` // in UTF-16 code units, like a JS string index
	Token       string      `json:"token"`
	Items       []complItem `json:"items"`
}

const complMax = 50

type shellToken struct {
	raw   string // as typed
	value string // with quotes/escapes removed
	start int    // byte offset in the input
}

// tokenize splits a command line roughly the way a shell would: on
// unquoted whitespace and the control operators ; | &, honouring quotes
// and (off Windows) backslash escapes. A trailing separator yields a
// final empty token, which is the one being completed.
func tokenize(line string) []shellToken {
	var toks []shellToken
	var cur shellToken
	var val strings.Builder
	in := false
	var quote byte
	flush := func(end int) {
		if in {
			cur.raw = line[cur.start:end]
			cur.value = val.String()
			toks = append(toks, cur)
		}
		in, val = false, strings.Builder{}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				val.WriteByte(c)
			}
			continue
		}
		switch {
		case c == ' ' || c == '\t':
			flush(i)
		case c == ';' || c == '|' || c == '&':
			flush(i)
			toks = append(toks, shellToken{raw: string(c), value: string(c), start: i})
		default:
			if !in {
				in, cur = true, shellToken{start: i}
			}
			if c == '\'' || c == '"' {
				quote = c
			} else if c == '\\' && runtime.GOOS != "windows" && i+1 < len(line) {
				i++
				val.WriteByte(line[i])
			} else {
				val.WriteByte(c)
			}
		}
	}
	wasIn := in
	flush(len(line))
	if !wasIn {
		toks = append(toks, shellToken{start: len(line)})
	}
	return toks
}

func isOperator(s string) bool { return s == ";" || s == "|" || s == "&" }

// complete answers the suggestion popup for the line typed so far,
// relative to cwd. It never spawns anything: filesystem listings,
// package.json/composer.json and static command lists only.
func complete(cwd, input, path string) complResult {
	toks := tokenize(input)
	last := toks[len(toks)-1]
	// The command's words: those after the last control operator.
	var words []string
	for _, t := range toks[:len(toks)-1] {
		if isOperator(t.raw) {
			words = words[:0]
			continue
		}
		words = append(words, t.value)
	}
	for len(words) > 0 && (words[0] == "sudo" || words[0] == "time" || words[0] == "env") {
		words = words[1:]
	}

	res := complResult{
		ReplaceFrom: len(utf16.Encode([]rune(input[:last.start]))),
		Token:       last.raw,
		Items:       []complItem{},
	}
	if strings.TrimSpace(input) == "" {
		return res
	}
	proj := findProject(cwd)
	tok := last.value

	var items []complItem
	switch {
	case len(words) == 0:
		if looksLikePath(tok) {
			items = completePath(cwd, last, false)
		} else if tok != "" { // never for an empty command word
			items = rankCommands(commandCandidates(proj, path), tok)
		}
	default:
		items = argCandidates(cwd, proj, words, last)
	}
	res.Items = finishItems(items, last.raw)
	return res
}

// finishItems puts an item that is exactly what's typed first (so it's
// the highlighted one, and Enter just runs the line), and drops the list
// entirely when that exact match is all there is — `clear`, `ls`, `pwd`
// then never show a popup at all.
func finishItems(items []complItem, typed string) []complItem {
	exact := -1
	for i, it := range items {
		if it.Insert == typed || strings.TrimRight(it.Insert, " ") == typed {
			exact = i
			break
		}
	}
	if exact >= 0 {
		if len(items) == 1 {
			return []complItem{}
		}
		it := items[exact]
		items = append([]complItem{it}, append(items[:exact:exact], items[exact+1:]...)...)
	}
	if len(items) > complMax {
		items = items[:complMax]
	}
	if items == nil {
		items = []complItem{}
	}
	return items
}

// rankCommands orders command-word candidates: an exact match first,
// then prefix matches — built-ins/common commands before the rest, then
// shorter before longer — then word-start fuzzy matches (dropped when
// the word is already an exact command).
func rankCommands(list []complItem, tok string) []complItem {
	lt := strings.ToLower(tok)
	var exact, pre, fz []complItem
	seen := map[string]bool{}
	for _, it := range list {
		if seen[it.Insert] {
			continue
		}
		ll := strings.ToLower(it.Label)
		switch {
		case ll == lt:
			exact = append(exact, it)
		case strings.HasPrefix(ll, lt):
			pre = append(pre, it)
		case fuzzy(ll, lt):
			fz = append(fz, it)
		default:
			continue
		}
		seen[it.Insert] = true
	}
	sort.SliceStable(pre, func(i, j int) bool {
		a, b := pre[i], pre[j]
		if a.common != b.common {
			return a.common
		}
		return len(a.Label) < len(b.Label)
	})
	if len(exact) > 0 {
		fz = nil // the word is already a command: loose matches are only noise
	}
	return append(append(exact, pre...), fz...)
}

func looksLikePath(tok string) bool {
	return strings.ContainsAny(tok, `/`) || (runtime.GOOS == "windows" && strings.Contains(tok, `\`)) ||
		strings.HasPrefix(tok, ".") || strings.HasPrefix(tok, "~")
}

// Commands whose arguments are (mostly) paths.
var pathCommands = map[string]bool{
	"cd": true, "pushd": true, "ls": true, "ll": true, "la": true, "cat": true, "less": true, "more": true,
	"head": true, "tail": true, "bat": true, "php": true, "node": true, "code": true, "open": true,
	"vim": true, "vi": true, "nvim": true, "nano": true, "rm": true, "cp": true, "mv": true, "mkdir": true,
	"rmdir": true, "touch": true, "tree": true, "source": true, ".": true, "subl": true, "chmod": true,
	"du": true, "stat": true, "file": true, "tar": true, "unzip": true, "zip": true, "diff": true,
	"grep": true, "rg": true, "find": true, "wc": true, "explorer": true, "start": true, "ni": true,
	"Set-Location": true, "sl": true, "Get-Content": true, "gc": true, "type": true, "del": true,
	"copy": true, "move": true, "ren": true, "md": true, "rd": true, "dir": true, "tsx": true,
	"ts-node": true, "deno": true, "bun": true, "python": true, "python3": true, "sh": true, "bash": true, "zsh": true,
}

func argCandidates(cwd string, proj project, words []string, last shellToken) []complItem {
	cmd := words[0]
	args := words[1:]
	tok := last.value
	sub := func(list []complItem) []complItem { return rankItems(list, tok) }
	if runtime.GOOS == "windows" {
		cmd = strings.TrimSuffix(strings.ToLower(cmd), ".exe")
	}

	switch cmd {
	case "cd", "pushd", "Set-Location", "sl", "rmdir", "rd":
		return completePath(cwd, last, true)
	case "php":
		if len(args) == 0 && !looksLikePath(tok) && proj.artisan && strings.HasPrefix("artisan", strings.ToLower(tok)) {
			return []complItem{{Label: "artisan", Insert: "artisan", Kind: "cmd", Detail: "Laravel"}}
		}
		if len(args) == 1 && args[0] == "artisan" && proj.artisan {
			return sub(artisanItems())
		}
	case "artisan":
		if len(args) == 0 {
			return sub(artisanItems())
		}
	case "npm", "pnpm", "yarn", "bun":
		if len(args) == 0 {
			var list []complItem
			if cmd != "npm" { // yarn/pnpm/bun run scripts by name
				list = scriptItems(proj)
			}
			return sub(append(list, simpleItems(npmSubcommands, "cmd", "")...))
		}
		if len(args) == 1 && (args[0] == "run" || args[0] == "run-script") {
			return sub(scriptItems(proj))
		}
	case "npx", "pnpx", "bunx":
		if len(args) == 0 {
			return sub(binItems(proj))
		}
	case "composer":
		if len(args) == 0 {
			return sub(append(composerScriptItems(proj), simpleItems(composerSubcommands, "cmd", "")...))
		}
		if len(args) == 1 && (args[0] == "run" || args[0] == "run-script") {
			return sub(composerScriptItems(proj))
		}
	case "git":
		if len(args) == 0 {
			return sub(simpleItems(gitSubcommands, "cmd", ""))
		}
		if len(args) == 1 && (args[0] == "checkout" || args[0] == "switch" || args[0] == "merge" || args[0] == "rebase" || args[0] == "branch") && !looksLikePath(tok) {
			if b := gitBranches(proj.gitDir); len(b) > 0 {
				return sub(simpleItems(b, "script", "branch"))
			}
		}
	case "mullion":
		if len(args) == 0 {
			return sub(simpleItems(mullionSubcommands, "cmd", ""))
		}
	}
	if strings.HasPrefix(tok, "-") {
		return nil
	}
	if looksLikePath(tok) || pathCommands[cmd] || cmd == "git" && len(args) > 0 && (args[0] == "add" || args[0] == "restore" || args[0] == "diff" || args[0] == "rm") {
		return completePath(cwd, last, false)
	}
	return nil
}

// ── path completion ──────────────────────────────────────────────────

func completePath(cwd string, last shellToken, dirsOnly bool) []complItem {
	value, raw := last.value, last.raw
	seps := "/"
	if runtime.GOOS == "windows" {
		seps = `/\`
	}
	// Split both forms at the last separator; separators are never
	// escaped, so the raw prefix is safe to reuse verbatim.
	vi := strings.LastIndexAny(value, seps)
	ri := strings.LastIndexAny(raw, seps)
	dirPart, base, rawDir := "", value, ""
	if vi >= 0 {
		dirPart, base = value[:vi+1], value[vi+1:]
	}
	if ri >= 0 {
		rawDir = raw[:ri+1]
	}
	// A leading quote belongs to the token, not the directory.
	quote := ""
	if len(raw) > 0 && (raw[0] == '\'' || raw[0] == '"') {
		quote = raw[:1]
		if ri < 0 {
			rawDir = ""
		}
		rawDir = strings.TrimPrefix(rawDir, quote)
	}

	dir := dirPart
	switch {
	case dir == "" && value == "~":
		// "~" alone: offer "~/" itself.
		return []complItem{{Label: "~/", Insert: "~/", Kind: "dir"}}
	case strings.HasPrefix(dir, "~"):
		dir = filepath.Join(homeDir(), dir[1:])
	case dir == "":
		dir = cwd
	case !filepath.IsAbs(dir) && !(runtime.GOOS == "windows" && strings.HasPrefix(dir, `\`)):
		dir = filepath.Join(cwd, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	showHidden := strings.HasPrefix(base, ".")
	lb := strings.ToLower(base)
	type cand struct {
		name  string
		isDir bool
		rank  int // 0 = prefix, 1 = fuzzy
	}
	var cands []cand
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !showHidden {
			continue
		}
		ln := strings.ToLower(name)
		rank := -1
		switch {
		case strings.HasPrefix(ln, lb):
			rank = 0
		case fuzzy(ln, lb):
			rank = 1
		}
		if rank < 0 {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
				isDir = true
			}
		}
		if dirsOnly && !isDir {
			continue
		}
		cands = append(cands, cand{name, isDir, rank})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.isDir != b.isDir {
			return a.isDir
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	if len(cands) > complMax {
		cands = cands[:complMax]
	}
	sep := "/"
	if runtime.GOOS == "windows" && strings.Contains(rawDir, `\`) {
		sep = `\`
	}
	items := make([]complItem, 0, len(cands))
	for _, c := range cands {
		label := c.name
		kind := "file"
		if c.isDir {
			label += sep
			kind = "dir"
		}
		insert := ""
		switch {
		case quote != "":
			insert = quote + rawDir + c.name + sepIf(c.isDir, sep)
			if !c.isDir {
				insert += quote
			}
		case runtime.GOOS == "windows":
			insert = rawDir + c.name + sepIf(c.isDir, sep)
			if strings.ContainsAny(insert, " &()'") {
				insert = `'` + strings.ReplaceAll(insert, `'`, `''`) + `'`
			}
		default:
			insert = rawDir + shellEscape(c.name) + sepIf(c.isDir, sep)
		}
		items = append(items, complItem{Label: label, Insert: insert, Kind: kind})
	}
	return items
}

func sepIf(ok bool, sep string) string {
	if ok {
		return sep
	}
	return ""
}

func shellEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t'\"\\$&;|<>()*?!{}[]#`", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fuzzy reports whether pat (2+ characters) appears in s in order,
// starting at the beginning of s or of one of its words ("art" matches
// "php artisan", "mfs" matches "migrate:fresh --seed"), which keeps
// one-letter noise out of the list.
func fuzzy(s, pat string) bool {
	if len(pat) < 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (i == 0 || strings.IndexByte(" -_.:/", s[i-1]) >= 0) && s[i] == pat[0] && subsequence(s[i:], pat) {
			return true
		}
	}
	return false
}

func subsequence(s, pat string) bool {
	for _, r := range pat {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false
		}
		s = s[i+len(string(r)):]
	}
	return true
}

// rankItems keeps the items whose label matches tok: case-insensitive
// prefix matches first (in list order), then fuzzy ones.
func rankItems(list []complItem, tok string) []complItem {
	lt := strings.ToLower(tok)
	var pre, fz []complItem
	seen := map[string]bool{}
	for _, it := range list {
		if seen[it.Insert] {
			continue
		}
		ll := strings.ToLower(it.Label)
		switch {
		case strings.HasPrefix(ll, lt):
			pre = append(pre, it)
			seen[it.Insert] = true
		case fuzzy(ll, lt):
			fz = append(fz, it)
			seen[it.Insert] = true
		}
	}
	return append(pre, fz...)
}

// ── project-aware candidates ─────────────────────────────────────────

type project struct {
	root            string // nearest folder with package.json / composer.json / artisan
	pkg             map[string]string
	bins            []string
	artisan         bool
	gitDir          string
	composerScripts []string
}

// findProject walks up from cwd (a few levels, never past home) to the
// nearest project folder and reads what completions need from it.
func findProject(cwd string) project {
	var p project
	home := homeDir()
	dir := cwd
	for i := 0; i < 8 && dir != ""; i++ {
		if p.gitDir == "" && isDir(filepath.Join(dir, ".git")) {
			p.gitDir = filepath.Join(dir, ".git")
		}
		if p.root == "" && (fileExists(filepath.Join(dir, "package.json")) || fileExists(filepath.Join(dir, "composer.json")) || fileExists(filepath.Join(dir, "artisan"))) {
			p.root = dir
		}
		if (p.root != "" && p.gitDir != "") || dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if p.root == "" {
		return p
	}
	// `php artisan` only works from the folder that has it.
	p.artisan = fileExists(filepath.Join(cwd, "artisan"))
	if data, err := os.ReadFile(filepath.Join(p.root, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			p.pkg = pkg.Scripts
		}
	}
	if entries, err := os.ReadDir(filepath.Join(p.root, "node_modules", ".bin")); err == nil {
		for _, e := range entries {
			name := e.Name()
			if runtime.GOOS == "windows" {
				if !strings.HasSuffix(strings.ToLower(name), ".cmd") {
					continue
				}
				name = name[:len(name)-4]
			}
			p.bins = append(p.bins, name)
		}
	}
	if data, err := os.ReadFile(filepath.Join(p.root, "composer.json")); err == nil {
		var c struct {
			Scripts map[string]json.RawMessage `json:"scripts"`
		}
		if json.Unmarshal(data, &c) == nil {
			for k := range c.Scripts {
				if !strings.HasPrefix(k, "pre-") && !strings.HasPrefix(k, "post-") {
					p.composerScripts = append(p.composerScripts, k)
				}
			}
			sort.Strings(p.composerScripts)
		}
	}
	return p
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func commandCandidates(p project, path string) []complItem {
	exes := pathExecutables(path)
	list := commonCommands(exes)
	if p.artisan {
		list = append(list, complItem{Label: "php artisan", Insert: "php artisan ", Kind: "cmd", Detail: "Laravel"})
	}
	for _, name := range sortedKeys(p.pkg) {
		list = append(list, complItem{Label: "npm run " + name, Insert: "npm run " + name, Kind: "script", Detail: p.pkg[name]})
	}
	for _, b := range p.bins {
		list = append(list, complItem{Label: "npx " + b, Insert: "npx " + b, Kind: "cmd", Detail: "node_modules/.bin"})
	}
	for _, name := range exes {
		list = append(list, complItem{Label: name, Insert: name, Kind: "cmd"})
	}
	return list
}

func scriptItems(p project) []complItem {
	var list []complItem
	for _, name := range sortedKeys(p.pkg) {
		list = append(list, complItem{Label: name, Insert: name, Kind: "script", Detail: p.pkg[name]})
	}
	return list
}

func binItems(p project) []complItem {
	return simpleItems(p.bins, "cmd", "node_modules/.bin")
}

func composerScriptItems(p project) []complItem {
	return simpleItems(p.composerScripts, "script", "composer.json")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func simpleItems(names []string, kind, detail string) []complItem {
	out := make([]complItem, len(names))
	for i, n := range names {
		out[i] = complItem{Label: n, Insert: n, Kind: kind, Detail: detail}
	}
	return out
}

func gitBranches(gitDir string) []string {
	if gitDir == "" {
		return nil
	}
	set := map[string]bool{}
	heads := filepath.Join(gitDir, "refs", "heads")
	filepath.WalkDir(heads, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if rel, err := filepath.Rel(heads, path); err == nil {
				set[filepath.ToSlash(rel)] = true
			}
		}
		return nil
	})
	if data, err := os.ReadFile(filepath.Join(gitDir, "packed-refs")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if _, ref, ok := strings.Cut(line, " refs/heads/"); ok {
				set[strings.TrimSpace(ref)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// commonCommands are the shell built-ins and everyday tools, always
// offered (and ranked first) at the command position.
func commonCommands(exes []string) []complItem {
	var names []string
	if runtime.GOOS == "windows" {
		names = []string{"cls", "clear", "ls", "dir", "gci", "cd", "pwd", "cat", "type", "mkdir", "rm", "cp", "mv",
			"echo", "exit", "where", "php", "composer", "node", "npm", "npx", "pnpm", "yarn", "git", "mullion", "code",
			"explorer", "ni", "Get-ChildItem", "Set-Location"}
	} else {
		names = []string{"ls", "cd", "pwd", "clear", "cat", "less", "more", "head", "tail", "grep", "find", "which",
			"whereis", "echo", "printf", "mkdir", "rmdir", "rm", "cp", "mv", "touch", "ln", "chmod", "chown", "code",
			"vim", "nano", "top", "htop", "ps", "kill", "killall", "history", "exit", "source", "export", "env", "curl",
			"wget", "ssh", "scp", "tar", "zip", "unzip", "du", "df", "sort", "uniq", "wc", "xargs", "sed", "awk", "man",
			"sudo", "git", "php", "composer", "npm", "npx", "pnpm", "yarn", "bun", "node", "mullion", "make", "docker"}
		if runtime.GOOS == "darwin" {
			names = append(names, "open")
		}
		for _, e := range exes {
			if e == "brew" { // only where Homebrew is actually installed
				names = append(names, "brew")
				break
			}
		}
	}
	items := simpleItems(names, "cmd", "")
	for i := range items {
		items[i].common = true
	}
	return items
}

// pathExecutables lists the programs in the PATH directories (names
// only, Windows extensions stripped), cached per PATH for a minute so
// completion stays fast. On macOS the Homebrew prefixes are included
// even when the panel's own PATH lacks them — the login shell adds them.
func pathExecutables(path string) []string {
	pathCache.Lock()
	defer pathCache.Unlock()
	if e, ok := pathCache.m[path]; ok && time.Since(e.at) < time.Minute {
		return e.names
	}
	dirs := filepath.SplitList(path)
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	}
	seen := map[string]bool{}
	var names []string
	for _, dir := range dirs {
		if dir == "" || seen["\x00"+dir] {
			continue
		}
		seen["\x00"+dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if runtime.GOOS == "windows" {
				ext := strings.ToLower(filepath.Ext(name))
				if ext != ".exe" && ext != ".cmd" && ext != ".bat" && ext != ".ps1" && ext != ".com" {
					continue
				}
				name = strings.ToLower(name[:len(name)-len(ext)])
			} else {
				if e.IsDir() || strings.HasPrefix(name, ".") {
					continue
				}
				if e.Type().IsRegular() {
					if info, err := e.Info(); err != nil || info.Mode()&0o111 == 0 {
						continue
					}
				}
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			if len(names) >= 5000 {
				break
			}
		}
	}
	sort.Strings(names)
	if len(pathCache.m) > 16 {
		pathCache.m = map[string]pathCacheEntry{}
	}
	pathCache.m[path] = pathCacheEntry{names, time.Now()}
	return names
}

type pathCacheEntry struct {
	names []string
	at    time.Time
}

var pathCache = struct {
	sync.Mutex
	m map[string]pathCacheEntry
}{m: map[string]pathCacheEntry{}}

// envPath is the PATH value in env (any case, for Windows' "Path").
func envPath(env []string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
			return v
		}
	}
	return ""
}

var npmSubcommands = []string{"run", "install", "i", "ci", "uninstall", "update", "outdated", "audit", "test", "start",
	"init", "exec", "list", "ls", "view", "publish", "link", "cache", "version", "doctor"}

var composerSubcommands = []string{"install", "update", "require", "remove", "dump-autoload", "create-project",
	"run-script", "show", "outdated", "validate", "why", "global", "self-update", "diagnose", "audit", "init", "config", "exec"}

var gitSubcommands = []string{"status", "add", "commit", "push", "pull", "fetch", "checkout", "switch", "branch", "log",
	"diff", "stash", "merge", "rebase", "restore", "reset", "tag", "remote", "clone", "init", "show", "cherry-pick", "blame", "clean"}

var mullionSubcommands = []string{"status", "start", "stop", "restart", "link", "unlink", "links", "use", "isolate",
	"unisolate", "secure", "unsecure", "php", "node", "composer", "dev", "mysql", "postgres", "mongo", "tld", "doctor",
	"ui", "update", "setup", "rename", "backups", "which", "phpmyadmin", "pgadmin", "heidisql"}

var artisanCommands = []string{"serve", "migrate", "migrate:fresh", "migrate:fresh --seed", "migrate:rollback",
	"migrate:refresh", "migrate:reset", "migrate:status", "db:seed", "db:wipe", "db:show", "tinker", "test", "about",
	"list", "route:list", "route:cache", "route:clear", "config:cache", "config:clear", "cache:clear", "view:cache",
	"view:clear", "event:cache", "event:clear", "event:list", "optimize", "optimize:clear", "key:generate",
	"storage:link", "queue:work", "queue:listen", "queue:restart", "queue:failed", "queue:retry", "schedule:run",
	"schedule:work", "schedule:list", "vendor:publish", "down", "up", "model:show", "pail", "install:api",
	"install:broadcasting", "make:model", "make:controller", "make:migration", "make:seeder", "make:factory",
	"make:middleware", "make:request", "make:resource", "make:command", "make:job", "make:event", "make:listener",
	"make:mail", "make:notification", "make:policy", "make:provider", "make:rule", "make:test", "make:component",
	"make:view", "make:cast", "make:enum", "make:class", "make:interface", "make:trait", "make:observer", "make:scope",
	"make:channel", "make:exception"}

func artisanItems() []complItem { return simpleItems(artisanCommands, "cmd", "artisan") }
