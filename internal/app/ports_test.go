package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/pmdir"
	"pm/internal/postgres"
)

// fakeMachine replaces the port seams: busy maps a port to its owner
// (pid 0 = busy, owner not visible), ours lists Mullion's pids.
type fakeMachine struct {
	busy map[int]struct {
		pid  int
		name string
	}
	ours map[int]bool
}

func (f *fakeMachine) hold(port, pid int, name string) {
	f.busy[port] = struct {
		pid  int
		name string
	}{pid, name}
}

// portsTestApp is an App over a sandbox home with the machine seams
// faked and the engine ports restored afterwards. Engines are not
// installed, so nothing is ever stopped or started.
func portsTestApp(t *testing.T) (*App, *fakeMachine) {
	t.Helper()
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMachine{busy: map[int]struct {
		pid  int
		name string
	}{}, ours: map[int]bool{}}
	oldOwner, oldBusy, oldPids := portOwner, portBusy, mullionPids
	oldMy, oldPg, oldMongo := mysql.Port, postgres.Port, mongodb.Port
	oldHooks := portChangeHooks
	portOwner = func(p int) (int, string) { o := f.busy[p]; return o.pid, o.name }
	portBusy = func(p int) bool { _, ok := f.busy[p]; return ok }
	mullionPids = func(string) map[int]bool { return f.ours }
	portChangeHooks = nil
	t.Cleanup(func() {
		portOwner, portBusy, mullionPids = oldOwner, oldBusy, oldPids
		mysql.Port, postgres.Port, mongodb.Port = oldMy, oldPg, oldMongo
		portChangeHooks = oldHooks
	})
	return &App{Paths: paths, State: state, skipApply: true}, f
}

func TestApplyPortConfig(t *testing.T) {
	portsTestApp(t)
	ApplyPortConfig(config.Config{MySQLPort: 3307, MongoPort: 27018})
	if mysql.Port != 3307 || postgres.Port != 5432 || mongodb.Port != 27018 {
		t.Errorf("ports = %d %d %d", mysql.Port, postgres.Port, mongodb.Port)
	}
	ApplyPortConfig(config.Config{})
	if mysql.Port != 3306 || postgres.Port != 5432 || mongodb.Port != 27017 {
		t.Errorf("defaults = %d %d %d", mysql.Port, postgres.Port, mongodb.Port)
	}
}

func TestCheckPort(t *testing.T) {
	a, f := portsTestApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	a.State.Sites = []config.Site{{Name: "web", Kind: "node", DevPort: 42001}}
	f.hold(5555, 321, "httpd")
	f.hold(6666, 77, "node")
	f.ours[77] = true

	cases := []struct {
		port  int
		self  string
		force bool
		want  string // "" = ok, else a substring of the error
	}{
		{80, "mysql", false, "out of range"},
		{70000, "mysql", false, "out of range"},
		{5432, "mysql", false, "PostgreSQL"},  // another engine's port, even uninstalled
		{27017, "mysql", false, "MongoDB"},    //
		{42001, "mysql", false, "dev: web"},   // a dev server's port
		{9084, "mysql", false, "PHP 8.4"},     // php-cgi of the global PHP
		{42999, "mysql", false, "Wake agent"}, //
		{3306, "mysql", false, ""},            // its own current port
		{3307, "mysql", false, ""},
		{5555, "mysql", false, "httpd (pid 321)"},
		{5555, "mysql", true, ""}, // forced over a foreign holder
		{6666, "mysql", true, "Mullion's own"},
		{42001, "dev:web", false, ""},
	}
	for _, c := range cases {
		err := a.checkPort(c.port, c.self, c.force, nil)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%d/%s: unexpected %v", c.port, c.self, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%d/%s: err = %v, want %q", c.port, c.self, err, c.want)
		}
	}
	var inUse *PortInUseError
	if err := a.checkPort(5555, "mysql", false, nil); !errors.As(err, &inUse) || inUse.PID != 321 || inUse.Name != "httpd" {
		t.Errorf("want a *PortInUseError, got %#v", err)
	}
}

func TestSetEnginePort(t *testing.T) {
	a, f := portsTestApp(t)
	site := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(site, 0o755)
	os.WriteFile(filepath.Join(site, ".env"), []byte("APP_NAME=shop\nDB_CONNECTION=mysql\nDB_PORT=3306\n# DB_PORT=3306\n"), 0o644)
	a.State.Sites = []config.Site{{Name: "shop", Path: site}}
	f.hold(3400, 99, "mysqld")

	var hooked []string
	OnEnginePortChange(func(_ *App, engine string) error { hooked = append(hooked, engine); return nil })

	if _, err := a.SetEnginePort("oracle", 1521, false); err == nil {
		t.Error("unknown engine accepted")
	}
	if _, err := a.SetEnginePort("mysql", 3400, false); err == nil {
		t.Error("foreign-held port accepted without force")
	}
	hints, err := a.SetEnginePort("mariadb", 3307, false)
	if err != nil {
		t.Fatal(err)
	}
	if mysql.Port != 3307 || a.State.Config.MySQLPort != 3307 {
		t.Errorf("port = %d, config = %d", mysql.Port, a.State.Config.MySQLPort)
	}
	if len(hints) != 1 || !strings.Contains(hints[0], "shop: .env DB_PORT") || !strings.Contains(hints[0], "3307") {
		t.Errorf("hints = %q", hints)
	}
	if len(hooked) != 1 || hooked[0] != "mysql" {
		t.Errorf("hooks ran for %v", hooked)
	}
	// Saved: a fresh load sees it.
	st, _ := config.Load(a.Paths)
	if st.Config.MySQLPort != 3307 {
		t.Errorf("saved mysqlPort = %d", st.Config.MySQLPort)
	}
	// Back to the default stores 0.
	if _, err := a.SetEnginePort("mysql", 3306, false); err != nil {
		t.Fatal(err)
	}
	if a.State.Config.MySQLPort != 0 || mysql.Port != 3306 {
		t.Errorf("default: config = %d, port = %d", a.State.Config.MySQLPort, mysql.Port)
	}
	// Forced over a foreign holder.
	if _, err := a.SetEnginePort("postgres", 3400, true); err != nil || postgres.Port != 3400 {
		t.Errorf("forced: %v, port %d", err, postgres.Port)
	}
}

func TestEnvPortHints(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, env string) config.Site {
		p := filepath.Join(dir, name)
		os.MkdirAll(p, 0o755)
		if env != "" {
			os.WriteFile(filepath.Join(p, ".env"), []byte(env), 0o644)
		}
		return config.Site{Name: name, Path: p}
	}
	sites := []config.Site{
		mk("laravel", "DB_PORT=5432\nREDIS_PORT=6379\n"),
		mk("prisma", `DATABASE_URL="postgresql://postgres@127.0.0.1:5432/app?schema=public"`+"\n"),
		mk("other", "DB_PORT=3306\nAPP_URL=http://localhost:5432x\n"),
		mk("none", ""),
		mk("quoted", "export PG_PORT='5432'\n"),
	}
	hints := envPortHints(sites, 5432, 5433)
	got := strings.Join(hints, "\n")
	for _, want := range []string{"laravel: .env DB_PORT", "prisma: .env DATABASE_URL", "quoted: .env PG_PORT"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(hints) != 3 {
		t.Errorf("want 3 hints, got:\n%s", got)
	}
}

func TestSetSiteDevPort(t *testing.T) {
	a, f := portsTestApp(t)
	dir := t.TempDir()
	vite := filepath.Join(dir, "vite-app")
	os.MkdirAll(vite, 0o755)
	os.WriteFile(filepath.Join(vite, "vite.config.ts"), []byte("export default defineConfig({\n  server: { port: 5173, strictPort: true },\n})\n"), 0o644)
	os.WriteFile(filepath.Join(vite, "package.json"), []byte("{\n  \"scripts\": {\n    \"dev\": \"vite --port 5174\"\n  }\n}\n"), 0o644)
	a.State.Sites = []config.Site{
		{Name: "vite-app", Path: vite, Kind: "node", DevPort: 42001},
		{Name: "other", Path: dir, Kind: "node", DevPort: 42002},
		{Name: "blog", Path: dir},
	}
	f.hold(43000, 500, "java")

	if _, err := a.SetSiteDevPort("nope", 43001, false); err == nil {
		t.Error("unknown site accepted")
	}
	if _, err := a.SetSiteDevPort("blog", 43001, false); err == nil {
		t.Error("PHP site accepted")
	}
	if _, err := a.SetSiteDevPort("vite-app", 42002, false); err == nil || !strings.Contains(err.Error(), "dev: other") {
		t.Errorf("another site's port: %v", err)
	}
	if _, err := a.SetSiteDevPort("vite-app", 43000, false); err == nil {
		t.Error("foreign-held port accepted")
	}
	hints, err := a.SetSiteDevPort("vite-app", 43001, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.State.FindSite("vite-app").DevPort != 43001 {
		t.Error("DevPort not saved")
	}
	got := strings.Join(hints, "\n")
	if !strings.Contains(got, "vite.config.ts sets port 5173") || !strings.Contains(got, "(5174)") {
		t.Errorf("hints = %s", got)
	}
	st, _ := config.Load(a.Paths)
	if st.FindSite("vite-app").DevPort != 43001 {
		t.Error("DevPort not persisted")
	}
}

func TestPortsOverview(t *testing.T) {
	a, f := portsTestApp(t)
	a.State.Config.MySQL = "8.4.5"
	a.State.Config.Mongo = "8.0.4"
	a.State.Config.GlobalPHP = "8.4.1"
	a.State.Sites = []config.Site{
		{Name: "a", Kind: "node", DevPort: 42001},
		{Name: "b", Kind: "node", DevPort: 42001}, // duplicate
		{Name: "c", Kind: "node", DevPort: 42003},
	}
	f.hold(80, 10, "caddy")
	f.ours[10] = true
	f.hold(3306, 20, "mysqld") // a foreign MySQL
	f.hold(42999, 30, "mullion")
	f.hold(42003, 0, "") // someone this user can't see

	byService := map[string]PortInfo{}
	for _, p := range a.PortsOverview() {
		byService[p.Service] = p
	}
	check := func(service string, running, conflict bool, heldBy string) {
		t.Helper()
		p, ok := byService[service]
		if !ok {
			t.Errorf("%s missing from %v", service, byService)
			return
		}
		if p.Running != running || p.Conflict != conflict || p.HeldBy != heldBy {
			t.Errorf("%s = running %v, conflict %v, heldBy %q", service, p.Running, p.Conflict, p.HeldBy)
		}
	}
	check("Caddy HTTP", true, false, "mullion")
	check("Caddy HTTPS", false, false, "")
	check("Wake agent", true, false, "mullion")
	check("PHP 8.4 FastCGI", false, false, "")
	check("MySQL 8.4.5", false, true, "mysqld (pid 20)")
	check("MongoDB 8.0.4", false, false, "")
	check("dev: a", false, true, "")
	check("dev: b", false, true, "")
	check("dev: c", false, true, "another user's process")
	if p := byService["MySQL 8.4.5"]; !p.Configurable || p.Kind != "mysql" || p.Port != 3306 {
		t.Errorf("mysql row = %+v", p)
	}
	if _, ok := byService["PostgreSQL"]; ok {
		t.Error("an uninstalled engine is listed")
	}
}

func TestSuggestFreePort(t *testing.T) {
	a, f := portsTestApp(t)
	a.State.Sites = []config.Site{{Name: "a", Kind: "node", DevPort: 42001}}
	f.hold(42002, 1, "x")
	p := a.SuggestFreePort(42001)
	if p < 42003 {
		t.Errorf("suggested %d (42001 is a dev port, 42002 is busy)", p)
	}
	if a.SuggestFreePort(80) < minUserPort {
		t.Error("suggested a privileged port")
	}
}
