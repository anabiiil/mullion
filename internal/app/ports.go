package app

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pm/internal/agent"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/heidisql"
	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/pgadmin"
	"pm/internal/phpver"
	"pm/internal/postgres"
	"pm/internal/sysproc"
)

// Port range a user may move a service to: no privileged ports.
const (
	minUserPort = 1024
	maxUserPort = 65535
)

// Fixed ports of Mullion's own services (not configurable).
const (
	caddyHTTPPort  = 80
	caddyHTTPSPort = 443
	caddyAdminPort = 2019
)

// Seams over the machine, swapped by this package's tests.
var (
	// portOwner resolves the process listening on a TCP port (0 = none
	// found — or one this user may not inspect).
	portOwner = sysproc.PortOwner
	// portBusy reports whether anything accepts connections on the port.
	portBusy = func(port int) bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	// mullionPids lists the processes that are Mullion's own: everything
	// running from the install root (Caddy, php-cgi, the databases, the
	// managed Node that runs dev servers), plus every pid Mullion
	// recorded in its pids directory.
	mullionPids = func(home string) map[int]bool {
		set := map[int]bool{}
		for _, pid := range sysproc.ProcessesUnder(home, "") {
			set[pid] = true
		}
		for _, pid := range recordedPids(filepath.Join(home, "pids")) {
			set[pid] = true
		}
		if runtime.GOOS != "windows" {
			// php-fpm rewrites its process title ("php-fpm: master process
			// (~/.mullion/php/fpm-8.4.conf)"), hiding its executable path.
			for _, pid := range pidsMentioning(home) {
				set[pid] = true
			}
		}
		return set
	}
)

// recordedPids reads every *.pid file in dir.
func recordedPids(dir string) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

// pidsMentioning lists (Unix) processes whose command line names a path
// under dir.
func pidsMentioning(dir string) []int {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	needle := filepath.Clean(dir) + string(filepath.Separator)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		pidStr, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.Contains(args, needle) {
			continue
		}
		if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ApplyPortConfig points the engine packages at the configured ports
// (0 = the engine's default). app.New calls it right after loading the
// config, before anything talks to a database.
func ApplyPortConfig(c config.Config) {
	mysql.Port = orDefault(c.MySQLPort, mysql.DefaultPort)
	postgres.Port = orDefault(c.PostgresPort, postgres.DefaultPort)
	mongodb.Port = orDefault(c.MongoPort, mongodb.DefaultPort)
}

func orDefault(port, def int) int {
	if port <= 0 {
		return def
	}
	return port
}

// PortChangeHook runs after an engine ("mysql", "postgres", "mongo")
// moved to a new port and was restarted there — admin tools that bake
// the port into their config follow it.
type PortChangeHook func(a *App, engine string) error

var portChangeHooks []PortChangeHook

// OnEnginePortChange registers a hook. It exists for packages that
// import app and so can't be called from here (phpMyAdmin,
// mongo-express); they register from init().
func OnEnginePortChange(h PortChangeHook) { portChangeHooks = append(portChangeHooks, h) }

// PortInfo is one port Mullion uses, for the Ports page and `mullion ports`.
type PortInfo struct {
	// Service is the human label: "Caddy HTTP", "MySQL", "PHP 8.4
	// FastCGI", "dev: wa-legal-frontend".
	Service string `json:"service"`
	// Kind is caddy|php|mysql|postgres|mongo|dev|agent.
	Kind string `json:"kind"`
	// Site is the node site a dev server belongs to.
	Site string `json:"site,omitempty"`
	Port int    `json:"port"`
	// Configurable marks ports the user can change (engines, dev servers).
	Configurable bool `json:"configurable"`
	// Running: Mullion's own process is listening on the port.
	Running bool `json:"running"`
	// HeldBy is "mullion", or "<name> (pid N)" when a foreign process owns it.
	HeldBy string `json:"heldBy,omitempty"`
	// Conflict: a foreign process holds the port, or two Mullion
	// services want it.
	Conflict bool `json:"conflict"`
	// Note explains something unexpected — e.g. a dev server listening
	// on a different port than the one assigned (Vite's server.port).
	Note string `json:"note,omitempty"`
}

// service is one port Mullion wants; key identifies it for "is this
// port already taken by ANOTHER Mullion service" checks.
type service struct {
	key  string
	info PortInfo
}

// services lists every port Mullion wants, in display order. A running
// dev server that listens somewhere other than its assigned DevPort
// contributes that actual port too (same key).
func (a *App) services() []service {
	out := []service{
		{"caddy-http", PortInfo{Service: "Caddy HTTP", Kind: "caddy", Port: caddyHTTPPort}},
		{"caddy-https", PortInfo{Service: "Caddy HTTPS", Kind: "caddy", Port: caddyHTTPSPort}},
		{"caddy-admin", PortInfo{Service: "Caddy admin API", Kind: "caddy", Port: caddyAdminPort}},
		{"agent", PortInfo{Service: "Wake agent", Kind: "agent", Port: agent.Port}},
	}
	for _, v := range a.NeededVersions() {
		port, err := phpver.FcgiPort(v)
		if err != nil {
			continue
		}
		label := v
		if sel, err := phpver.ParseSelector(v); err == nil {
			label = fmt.Sprintf("%d.%d", sel.Major, sel.Minor)
		}
		out = append(out, service{"php:" + label, PortInfo{Service: "PHP " + label + " FastCGI", Kind: "php", Port: port}})
	}
	c := a.State.Config
	// Engines are listed when installed — but their port is always
	// reserved (see reservedBy), so a later install can't collide.
	if c.MySQL != "" {
		out = append(out, service{"mysql", PortInfo{Service: mysql.Label(c.MySQL), Kind: "mysql", Port: mysql.Port, Configurable: true}})
	}
	if c.Postgres != "" {
		out = append(out, service{"postgres", PortInfo{Service: postgres.Label(c.Postgres), Kind: "postgres", Port: postgres.Port, Configurable: true}})
	}
	if c.Mongo != "" {
		out = append(out, service{"mongo", PortInfo{Service: mongodb.Label(c.Mongo), Kind: "mongo", Port: mongodb.Port, Configurable: true}})
	}
	for _, s := range a.State.Sites {
		if s.Kind != "node" || s.DevPort <= 0 {
			continue
		}
		info := PortInfo{Service: "dev: " + s.Name, Kind: "dev", Site: s.Name, Port: s.DevPort, Configurable: true}
		if s.Mode == "build" {
			info.Note = "serving the production build — no dev server"
		}
		out = append(out, service{"dev:" + s.Name, info})
		if actual := devserver.Running(a.Paths, s.Name); actual > 0 && actual != s.DevPort {
			out[len(out)-1].info.Note = fmt.Sprintf("the dev server ignores this port and listens on %d (see its config)", actual)
			out = append(out, service{"dev:" + s.Name, PortInfo{
				Service: "dev: " + s.Name + " (actual)", Kind: "dev", Site: s.Name, Port: actual,
			}})
		}
	}
	return out
}

// reservedBy reports which OTHER Mullion service (key != self) wants
// the port, by label ("" = none). The engines' ports count even when
// the engine isn't installed yet.
func (a *App) reservedBy(port int, self string) string {
	for _, s := range a.services() {
		if s.key != self && s.info.Port == port {
			return s.info.Service
		}
	}
	engines := []struct {
		key, label string
		port       int
	}{
		{"mysql", "MySQL", mysql.Port},
		{"postgres", "PostgreSQL", postgres.Port},
		{"mongo", "MongoDB", mongodb.Port},
	}
	for _, e := range engines {
		if e.key != self && e.port == port {
			return e.label
		}
	}
	return ""
}

// PortsOverview lists every port Mullion uses, who holds it right now,
// and whether it is in conflict. Read-only.
func (a *App) PortsOverview() []PortInfo {
	svcs := a.services()
	keys := map[int]map[string]bool{} // port -> the services wanting it
	for _, s := range svcs {
		if keys[s.info.Port] == nil {
			keys[s.info.Port] = map[string]bool{}
		}
		keys[s.info.Port][s.key] = true
	}
	var ours map[int]bool // resolved lazily: listing processes isn't free
	out := make([]PortInfo, 0, len(svcs))
	for _, s := range svcs {
		info := s.info
		// The "(actual)" row of a dev server shares its key; a duplicate
		// only counts between different services.
		if len(keys[info.Port]) > 1 {
			info.Conflict = true
		}
		if portBusy(info.Port) {
			pid, name := portOwner(info.Port)
			if ours == nil {
				ours = mullionPids(a.Paths.Home)
			}
			switch {
			case isMullionProcess(pid, name, ours):
				info.Running = true
				info.HeldBy = "mullion"
			case pid > 0:
				info.HeldBy = fmt.Sprintf("%s (pid %d)", name, pid)
				info.Conflict = true
			default:
				// Everything Mullion runs belongs to this user, whose
				// processes the lookup can always see.
				info.HeldBy = "another user's process"
				info.Conflict = true
			}
		}
		out = append(out, info)
	}
	return out
}

// isMullionProcess reports whether a port owner is Mullion's: a process
// running from the install root, or the mullion binary itself (the wake
// agent and the panel run from wherever mullion is installed).
func isMullionProcess(pid int, name string, ours map[int]bool) bool {
	if pid <= 0 {
		return false
	}
	if ours[pid] || pid == os.Getpid() {
		return true
	}
	base := strings.ToLower(filepath.Base(name))
	return base == "mullion" || base == "mullion.exe"
}

// PortInUseError is returned when a foreign process already listens on
// the requested port. Retrying with force=true saves the port anyway
// (the service then fails to start until that process is gone).
type PortInUseError struct {
	Port int
	PID  int
	Name string
}

func (e *PortInUseError) Error() string {
	who := "another process"
	if e.PID > 0 {
		who = fmt.Sprintf("%s (pid %d)", e.Name, e.PID)
	}
	return fmt.Sprintf("port %d is in use by %s — pick another port, or force it", e.Port, who)
}

// checkPort validates a new port for the service identified by self:
// the user range, not wanted by another Mullion service, and not held by
// a foreign process (unless force). allowHeldBySelf lets a service keep
// a port its own running process holds.
func (a *App) checkPort(port int, self string, force bool, allowHeldBySelf func(pid int) bool) error {
	if port < minUserPort || port > maxUserPort {
		return fmt.Errorf("port %d is out of range — use %d-%d", port, minUserPort, maxUserPort)
	}
	if other := a.reservedBy(port, self); other != "" {
		return fmt.Errorf("port %d is already used by Mullion's %s", port, other)
	}
	if !portBusy(port) {
		return nil
	}
	pid, name := portOwner(port)
	if allowHeldBySelf != nil && allowHeldBySelf(pid) {
		return nil
	}
	if isMullionProcess(pid, name, mullionPids(a.Paths.Home)) {
		return fmt.Errorf("port %d is already used by one of Mullion's own processes (%s, pid %d)", port, name, pid)
	}
	if force {
		return nil
	}
	return &PortInUseError{Port: port, PID: pid, Name: name}
}

// normalizeEngine maps the names users type to mysql|postgres|mongo.
func normalizeEngine(engine string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "mysql", "mariadb":
		return "mysql", nil
	case "postgres", "postgresql", "pg":
		return "postgres", nil
	case "mongo", "mongodb":
		return "mongo", nil
	}
	return "", fmt.Errorf("unknown engine %q (use mysql, postgres or mongo)", engine)
}

// engineOps is what SetEnginePort needs to know about one engine.
type engineOps struct {
	label   string
	version string
	port    *int // the package var
	def     int
	cfg     *int // the config field
	running func() bool
	stop    func() error
	start   func() error
	write   func() error // rewrite its config file for the current port
}

func (a *App) engine(name string) engineOps {
	c := &a.State.Config
	switch name {
	case "mysql":
		v := c.MySQL
		return engineOps{label: "MySQL", version: v, port: &mysql.Port, def: mysql.DefaultPort, cfg: &c.MySQLPort,
			running: mysql.Running,
			stop:    func() error { return mysql.Stop(a.Paths, v) },
			start:   func() error { return mysql.Start(a.Paths, v) },
			write:   func() error { return mysql.WriteConfig(a.Paths, v) }}
	case "postgres":
		v := c.Postgres
		return engineOps{label: "PostgreSQL", version: v, port: &postgres.Port, def: postgres.DefaultPort, cfg: &c.PostgresPort,
			running: postgres.Running,
			stop:    func() error { return postgres.Stop(a.Paths, v) },
			start:   func() error { return postgres.Start(a.Paths, v) },
			write:   func() error { return postgres.WriteConfig(a.Paths, v) }}
	default:
		v := c.Mongo
		return engineOps{label: "MongoDB", version: v, port: &mongodb.Port, def: mongodb.DefaultPort, cfg: &c.MongoPort,
			running: mongodb.Running,
			stop:    func() error { return mongodb.Stop(a.Paths, v) },
			start:   func() error { return mongodb.Start(a.Paths, v) },
			write:   func() error { return mongodb.WriteConfig(a.Paths) }}
	}
}

// SetEnginePort moves a database engine (mysql|postgres|mongo) to a new
// port: validates it (1024-65535, not another Mullion service's, not
// held by a foreign process unless force — a *PortInUseError says who
// holds it), stops the engine if Mullion's copy is running, saves the
// config, rewrites the engine's config file, restarts it on the new port
// (rolling back to the old port if it can't start there), and points
// the admin tools (phpMyAdmin, HeidiSQL, pgAdmin, mongo-express) at it.
//
// Projects' .env files are the user's and are never edited: the hints
// name every site whose .env still points at the old port, plus any
// admin tool that could not be updated.
func (a *App) SetEnginePort(engine string, port int, force bool) (hints []string, err error) {
	name, err := normalizeEngine(engine)
	if err != nil {
		return nil, err
	}
	e := a.engine(name)
	old := *e.port
	if port == old {
		return nil, nil
	}
	if err := a.checkPort(port, name, force, nil); err != nil {
		return nil, err
	}

	// Only stop Mullion's OWN server: a foreign one answering on the old
	// port must never receive our shutdown command.
	wasRunning := false
	if e.version != "" && e.running() {
		pid, pname := portOwner(old)
		wasRunning = isMullionProcess(pid, pname, mullionPids(a.Paths.Home))
	}
	if wasRunning {
		if err := e.stop(); err != nil {
			return nil, fmt.Errorf("stopping %s: %w", e.label, err)
		}
	}

	setPort := func(p int) error {
		*e.cfg = p
		if p == e.def {
			*e.cfg = 0
		}
		if err := a.State.Save(); err != nil {
			return err
		}
		ApplyPortConfig(a.State.Config)
		return e.write()
	}
	if err := setPort(port); err != nil {
		return nil, err
	}
	if wasRunning {
		if startErr := e.start(); startErr != nil {
			// Roll back so the engine keeps working where it was.
			if err := setPort(old); err != nil {
				return nil, fmt.Errorf("%s did not start on port %d (%v), and restoring port %d failed: %w", e.label, port, startErr, old, err)
			}
			if err := e.start(); err != nil {
				return nil, fmt.Errorf("%s did not start on port %d (%v); back on port %d but it did not start there either: %w", e.label, port, startErr, old, err)
			}
			return nil, fmt.Errorf("%s did not start on port %d — kept port %d: %w", e.label, port, old, startErr)
		}
	}

	hints = append(hints, a.syncAdminTools(name)...)
	hints = append(hints, envPortHints(a.State.Sites, old, port)...)
	return hints, nil
}

// syncAdminTools points the engine's admin tools at its current port;
// failures come back as hints (the port change itself succeeded).
func (a *App) syncAdminTools(engine string) []string {
	var hints []string
	note := func(tool string, err error) {
		if err != nil {
			hints = append(hints, fmt.Sprintf("could not update %s for the new port: %v", tool, err))
		}
	}
	switch engine {
	case "mysql":
		note("HeidiSQL", heidisql.SyncPort(a.Paths))
	case "postgres":
		note("pgAdmin", pgadmin.SyncPort(a.Paths))
	}
	for _, h := range portChangeHooks {
		note("an admin tool", h(a, engine))
	}
	return hints
}

// envPortRe finds a loopback host:port in an .env value (DATABASE_URL,
// MONGODB_URI, ...).
var envPortRe = regexp.MustCompile(`(?:127\.0\.0\.1|localhost|\[::1\]):(\d+)\b`)

// envPortHints scans each site's .env (read-only) for settings still
// pointing at the old port: DB_PORT / *_PORT equal to it, or a loopback
// URL on it. One hint per site.
func envPortHints(sites []config.Site, oldPort, newPort int) []string {
	var hints []string
	old := strconv.Itoa(oldPort)
	for _, s := range sites {
		f, err := os.Open(filepath.Join(s.Path, ".env"))
		if err != nil {
			continue
		}
		var keys []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.Trim(strings.TrimSpace(val), `"'`)
			hit := strings.HasSuffix(strings.ToUpper(key), "PORT") && val == old
			for _, m := range envPortRe.FindAllStringSubmatch(val, -1) {
				if m[1] == old {
					hit = true
				}
			}
			if hit {
				keys = append(keys, key)
			}
		}
		f.Close()
		if len(keys) > 0 {
			verb := "points"
			if len(keys) > 1 {
				verb = "point"
			}
			hints = append(hints, fmt.Sprintf("%s: .env %s still %s at port %d — change it to %d",
				s.Name, strings.Join(keys, ", "), verb, oldPort, newPort))
		}
	}
	return hints
}

// SetSiteDevPort assigns a node site's dev server a new port (passed to
// it as PORT): validated like SetEnginePort — range, not another site's
// or Mullion service's, not held by a foreign process unless force
// (*PortInUseError) — saved, and the dev server restarted if running.
//
// Frameworks that read PORT follow it. Vite does NOT: it ignores the
// PORT variable, and a vite.config with a hard-coded server.port (or a
// `--port` in the dev script) wins over anything Mullion passes. Mullion
// still proxies correctly — it detects whichever port the dev server
// actually opens — but the assigned port then only matters if the
// project's config is changed to read it. The returned hints say so
// when such a setting is found.
func (a *App) SetSiteDevPort(site string, port int, force bool) (hints []string, err error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, fmt.Errorf("no site named %q", site)
	}
	if s.Kind != "node" {
		return nil, fmt.Errorf("%s is not a frontend site — only node sites have a dev server port", s.Name)
	}
	hints = devPortHints(s.Path)
	if port == s.DevPort {
		return hints, nil
	}
	actual := devserver.Running(a.Paths, s.Name)
	// The site's own running dev server may already sit on that port.
	ownListener := func(int) bool { return actual > 0 && actual == port }
	if err := a.checkPort(port, "dev:"+s.Name, force, ownListener); err != nil {
		return nil, err
	}
	s.DevPort = port
	if actual > 0 {
		devserver.Stop(a.Paths, s.Name)
		return hints, a.applyIfEnabled()
	}
	return hints, a.State.Save()
}

var (
	viteConfigPortRe = regexp.MustCompile(`\bport\s*:\s*(\d+)`)
	scriptPortRe     = regexp.MustCompile(`--port[= ](\d+)|-p\s+(\d+)`)
)

// devPortHints looks for port settings the dev server honors over PORT:
// server.port in vite.config.*, or --port in the dev script.
func devPortHints(dir string) []string {
	var hints []string
	for _, ext := range []string{"ts", "js", "mts", "mjs", "cts", "cjs"} {
		name := "vite.config." + ext
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if m := viteConfigPortRe.FindSubmatch(b); m != nil {
			hints = append(hints, fmt.Sprintf("%s sets port %s — Vite ignores PORT, so the dev server keeps listening on %s "+
				"(Mullion still proxies to it). To use the assigned port, set server.port to Number(process.env.PORT) there.",
				name, m[1], m[1]))
		}
		break
	}
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		if m := devScriptPort(b); m != "" {
			hints = append(hints, fmt.Sprintf("package.json's dev script passes a fixed port (%s), which wins over PORT", m))
		}
	}
	return hints
}

// devScriptPort extracts a fixed --port/-p from the dev (or serve/start)
// script, "" when none.
func devScriptPort(pkgJSON []byte) string {
	for _, key := range []string{`"dev"`, `"serve"`, `"start"`} {
		i := strings.Index(string(pkgJSON), key+":")
		if i < 0 {
			i = strings.Index(string(pkgJSON), key+" :")
		}
		if i < 0 {
			continue
		}
		line := string(pkgJSON[i:])
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		if m := scriptPortRe.FindStringSubmatch(line); m != nil {
			if m[1] != "" {
				return m[1]
			}
			return m[2]
		}
		return ""
	}
	return ""
}

// SuggestFreePort returns the first TCP port from start (at least 1024)
// that nothing listens on at 127.0.0.1 and no Mullion service wants,
// or 0 when there is none.
func (a *App) SuggestFreePort(start int) int {
	if start < minUserPort {
		start = minUserPort
	}
	taken := map[int]bool{mysql.Port: true, postgres.Port: true, mongodb.Port: true}
	for _, s := range a.services() {
		taken[s.info.Port] = true
	}
	for _, s := range a.State.Sites {
		taken[s.DevPort] = true
	}
	for p := start; p <= maxUserPort; p++ {
		if taken[p] || portBusy(p) {
			continue
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			ln.Close()
			return p
		}
	}
	return 0
}
