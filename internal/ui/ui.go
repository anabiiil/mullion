// Package ui serves Mullion's control panel — a small embedded web app on
// 127.0.0.1 — and opens it in an app-mode browser window so it looks
// and feels like a desktop program.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	_ "embed"

	"pm/internal/app"
	"pm/internal/autostart"
	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/heidisql"
	"pm/internal/mysql"
	"pm/internal/nodever"
	"pm/internal/phpmyadmin"
	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/sysproc"
)

//go:embed index.html
var indexHTML []byte

// Served as a real PNG favicon: Chromium app windows take their
// taskbar icon from it — an SVG data-URI icon gets ignored and the
// window shows the browser's own icon instead.
//
//go:embed favicon.png
var faviconPNG []byte

// Run serves the control panel and blocks until its window is closed
// (or ctx is cancelled when no app window could be opened).
func Run(ctx context.Context) error {
	// Opening the panel means "I want my stack": bring the services up in
	// the background while the window appears, instead of greeting the
	// user with "Stopped" and a button to press.
	go func() {
		a, err := app.New()
		if err != nil || a.State.Config.GlobalPHP == "" {
			return
		}
		if caddy.Running() {
			return
		}
		_ = caddy.EnsureInstalled(context.Background(), a.Paths)
		_ = a.Apply()
		_ = caddy.Start(a.Paths)
		if v := a.State.Config.MySQL; v != "" {
			_ = mysql.Start(a.Paths, v)
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	token, err := newToken()
	if err != nil {
		return err
	}

	// Track requests so tab-mode (no window to watch) can shut the
	// server down once the page is gone — it polls every 5 seconds.
	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().Unix())
	mux := newMux(token)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().Unix())
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	url := fmt.Sprintf("http://%s/?t=%s", ln.Addr().String(), token)
	done, err := openAppWindow(url)
	if err != nil {
		// The default browser can't do app windows (Firefox etc.):
		// open a normal tab in it and exit once the page stops polling.
		fmt.Println("Control panel:", url)
		openTab(url)
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(10 * time.Second):
				if time.Now().Unix()-lastSeen.Load() > 90 {
					return nil
				}
			}
		}
	}

	select {
	case <-done: // window closed
		return nil
	case <-ctx.Done():
		return nil
	}
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newMux(token string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/favicon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(faviconPNG)
	})

	api := func(path string, h func(a *app.App, r *http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Mullion-Token") != token {
				http.Error(w, "bad token", http.StatusForbidden)
				return
			}
			a, err := app.New()
			if err == nil {
				var data any
				data, err = h(a, r)
				if err == nil {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		})
	}

	api("/api/state", getState)
	api("/api/start", func(a *app.App, r *http.Request) (any, error) {
		if err := caddy.EnsureInstalled(r.Context(), a.Paths); err != nil {
			return nil, err
		}
		if err := a.Apply(); err != nil {
			return nil, err
		}
		return nil, caddy.Start(a.Paths)
	})
	api("/api/stop", func(a *app.App, r *http.Request) (any, error) {
		if err := caddy.Stop(a.Paths); err != nil {
			return nil, err
		}
		if err := fcgi.StopAll(a.Paths); err != nil {
			return nil, err
		}
		if v := a.State.Config.MySQL; v != "" {
			return nil, mysql.Stop(a.Paths, v)
		}
		return nil, nil
	})
	api("/api/php/available", func(a *app.App, r *http.Request) (any, error) {
		releases, err := phpver.FetchCurrent(r.Context())
		if err != nil {
			return nil, err
		}
		versions := make([]string, 0, len(releases))
		for _, rel := range releases {
			versions = append(versions, rel.Version)
		}
		return versions, nil
	})
	api("/api/php/use", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		sel, err := phpver.ParseSelector(in.Version)
		if err != nil {
			return nil, err
		}
		full, err := phpver.FindInstalled(a.Paths, sel)
		if err != nil {
			return nil, err
		}
		if err := a.UseGlobal(full); err != nil {
			return nil, err
		}
		return nil, a.Apply()
	})
	api("/api/php/install", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		sel, err := phpver.ParseSelector(in.Version)
		if err != nil {
			return nil, err
		}
		rel, err := phpver.Resolve(r.Context(), sel)
		if err != nil {
			return nil, err
		}
		_, err = phpver.Install(r.Context(), a.Paths, rel)
		return rel.Version, err
	})
	api("/api/php/ext", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return phpver.ListExtensions(a.Paths, in.Version)
	})
	api("/api/php/ext/get", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Version string
			Name    string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if err := phpver.InstallPeclExtension(r.Context(), a.Paths, in.Version, in.Name); err != nil {
			return nil, err
		}
		return nil, a.RestartPhp(in.Version)
	})
	api("/api/php/ext/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Version string
			Name    string
			Enabled bool
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if err := phpver.SetExtension(a.Paths, in.Version, in.Name, in.Enabled); err != nil {
			return nil, err
		}
		// Restart the version's php-cgi so running sites see the change.
		return nil, a.RestartPhp(in.Version)
	})
	api("/api/mysql/switch", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		version, err := mysql.ResolveVersionArg(r.Context(), in.Version)
		if err != nil {
			return nil, err
		}
		// From the panel there is no prompt: migrating the databases is
		// always the safe choice (everything is backed up first anyway).
		if _, err := app.SwitchDatabase(r.Context(), a, version, true); err != nil {
			return nil, err
		}
		return mysql.Label(version), nil
	})
	api("/api/mysql/start", func(a *app.App, r *http.Request) (any, error) {
		v := a.State.Config.MySQL
		if v == "" {
			return nil, errors.New("MySQL is not installed (run: mullion mysql install)")
		}
		// A foreign MySQL (a resurrected brew service, Laragon...) on the
		// port means every connection hits the WRONG server. The Start
		// click is explicit consent to stop it through its manager.
		if mysql.Running() && len(sysproc.ProcessesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) == 0 {
			pid, name := sysproc.PortOwner(mysql.Port)
			sysproc.StopConflict(sysproc.Conflict{Port: mysql.Port, PID: pid, Name: name})
			for i := 0; i < 20 && mysql.Running(); i++ {
				time.Sleep(500 * time.Millisecond)
			}
			if mysql.Running() {
				return nil, fmt.Errorf("another MySQL server (%s) is still holding port %d — stop it from a terminal (brew services list) and try again", name, mysql.Port)
			}
		}
		if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
			return nil, err
		}
		return nil, mysql.Start(a.Paths, v)
	})
	api("/api/mysql/password", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Password string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		v := a.State.Config.MySQL
		if v == "" {
			return nil, errors.New("MySQL is not installed")
		}
		if err := mysql.Start(a.Paths, v); err != nil {
			return nil, err
		}
		if err := mysql.SetRootPassword(a.Paths, v, in.Password); err != nil {
			return nil, err
		}
		a.State.Config.MySQLPassword = in.Password
		mysql.RootPassword = in.Password
		if err := a.State.Save(); err != nil {
			return nil, err
		}
		if err := phpmyadmin.RefreshConfig(a.Paths, in.Password); err != nil {
			return nil, fmt.Errorf("password changed, but phpMyAdmin's config could not be updated: %w", err)
		}
		return nil, nil
	})
	api("/api/db/list", func(a *app.App, r *http.Request) (any, error) {
		v := a.State.Config.MySQL
		if v == "" || !mysql.Running() {
			return []string{}, nil
		}
		dbs, err := mysql.UserDatabases(a.Paths, v)
		if err != nil {
			return nil, err
		}
		if dbs == nil {
			dbs = []string{}
		}
		return dbs, nil
	})
	api("/api/db/create", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		v := a.State.Config.MySQL
		if v == "" {
			return nil, errors.New("MySQL is not installed")
		}
		if err := mysql.Start(a.Paths, v); err != nil {
			return nil, err
		}
		return nil, mysql.CreateDatabase(a.Paths, v, in.Name)
	})
	api("/api/db/drop", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		v := a.State.Config.MySQL
		if v == "" {
			return nil, errors.New("MySQL is not installed")
		}
		if err := mysql.Start(a.Paths, v); err != nil {
			return nil, err
		}
		return nil, mysql.DropDatabase(a.Paths, v, in.Name)
	})
	api("/api/mysql/stop", func(a *app.App, r *http.Request) (any, error) {
		v := a.State.Config.MySQL
		if v == "" {
			return nil, nil
		}
		return nil, mysql.Stop(a.Paths, v)
	})
	api("/api/sites/link", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Path, Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		// Explorer's "Copy as path" wraps the path in quotes — accept it.
		path := filepath.Clean(strings.Trim(strings.TrimSpace(in.Path), `"`))
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("enter a full path, e.g. %s", examplePath())
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%s is not an existing folder", path)
		}
		name := in.Name
		if name == "" {
			name = filepath.Base(path)
		}
		name = config.Slugify(name)
		if name == "" {
			return nil, fmt.Errorf("could not derive a valid site name; type one")
		}
		if existing := a.State.FindSite(name); existing != nil {
			return nil, fmt.Errorf("site %q already links to %s", name, existing.Path)
		}
		site := config.Site{Name: name, Path: path, Kind: app.DetectProjectKind(path)}
		if site.Kind == "node" {
			site.DevPort = devserver.AssignPort(a.State.Sites)
			if _, err := nodever.Installed(a.Paths); err != nil {
				return nil, err
			}
			if versions, _ := nodever.Installed(a.Paths); len(versions) == 0 {
				return nil, errors.New("this is a frontend project and no Node is installed yet — run `mullion node install lts` first")
			}
		}
		a.State.AddSite(site)
		if err := a.Apply(); err != nil {
			return nil, err
		}
		return "http://" + name + "." + a.State.Config.TLD, nil
	})
	api("/api/sites/secure", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Name   string
			Secure bool
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		site.Secure = in.Secure
		if err := a.Apply(); err != nil {
			return nil, err
		}
		if in.Secure {
			if err := caddy.TrustCA(a.Paths); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	api("/api/sites/unlink", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if !a.State.RemoveSite(in.Name) {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		return nil, a.Apply()
	})
	api("/api/node/install", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		rel, err := nodever.Resolve(r.Context(), in.Version)
		if err != nil {
			return nil, err
		}
		if _, err := nodever.Install(r.Context(), a.Paths, rel); err != nil {
			return nil, err
		}
		if a.State.Config.GlobalNode == "" {
			return rel.Version, a.ActivateNode(rel.Version)
		}
		return rel.Version, nil
	})
	api("/api/node/use", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		full, err := nodever.FindInstalled(a.Paths, in.Version)
		if err != nil {
			return nil, err
		}
		return nil, a.ActivateNode(full)
	})
	api("/api/sites/mode", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name, Mode string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		if site.Kind != "node" {
			return nil, fmt.Errorf("%s is not a frontend site", in.Name)
		}
		switch in.Mode {
		case "build":
			buildDir, err := app.ResolveBuildDir(site.Path, site.BuildDir)
			if err != nil {
				return nil, err
			}
			site.BuildDir = buildDir
			site.Mode = "build"
			devserver.Stop(a.Paths, site.Name)
		case "dev":
			site.Mode = "dev"
			site.DevPaused = false
		default:
			return nil, fmt.Errorf("invalid mode %q", in.Mode)
		}
		return nil, a.Apply()
	})
	api("/api/dev/start", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil || site.Kind != "node" {
			return nil, fmt.Errorf("no frontend site named %q", in.Name)
		}
		site.DevPaused = false
		site.Mode = "dev"
		return nil, a.Apply()
	})
	api("/api/dev/stop", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil || site.Kind != "node" {
			return nil, fmt.Errorf("no frontend site named %q", in.Name)
		}
		site.DevPaused = true
		devserver.Stop(a.Paths, site.Name)
		return nil, a.Apply()
	})
	api("/api/sites/node", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name, Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		if site.Kind != "node" {
			return nil, fmt.Errorf("%s is not a Node site", in.Name)
		}
		if in.Version != "" {
			full, err := nodever.FindInstalled(a.Paths, in.Version)
			if err != nil {
				return nil, err
			}
			site.Node = full
		} else {
			site.Node = ""
		}
		devserver.Stop(a.Paths, site.Name)
		return nil, a.Apply()
	})
	api("/api/sites/isolate", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Name    string
			Version string // "" = follow the global version
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		full := ""
		if in.Version != "" {
			sel, err := phpver.ParseSelector(in.Version)
			if err != nil {
				return nil, err
			}
			if full, err = phpver.FindInstalled(a.Paths, sel); err != nil {
				return nil, err
			}
		}
		site.PHP = full
		return nil, a.Apply()
	})
	api("/api/heidisql/open", func(a *app.App, r *http.Request) (any, error) {
		if runtime.GOOS != "windows" {
			return nil, fmt.Errorf("HeidiSQL is a Windows desktop app — use phpMyAdmin here, or a native client like TablePlus or Sequel Ace")
		}
		if !heidisql.Installed(a.Paths) {
			if err := heidisql.Install(r.Context(), a.Paths); err != nil {
				return nil, err
			}
		}
		return nil, heidisql.Launch(a.Paths)
	})
	api("/api/autostart", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Enabled bool }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if in.Enabled {
			return nil, autostart.Enable(a.Paths)
		}
		return nil, autostart.Disable()
	})
	return mux
}

func getState(a *app.App, r *http.Request) (any, error) {
	installed, err := phpver.Installed(a.Paths)
	if err != nil {
		return nil, err
	}

	type cgi struct {
		Version string `json:"version"`
		Port    int    `json:"port"`
		Running bool   `json:"running"`
	}
	var cgis []cgi
	for _, v := range a.NeededVersions() {
		port, _ := phpver.FcgiPort(v)
		cgis = append(cgis, cgi{
			Version: v,
			Port:    port,
			Running: len(fcgi.RunningVersions([]string{v})) == 1,
		})
	}

	type site struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Path      string `json:"path"`
		Kind      string `json:"kind"`
		PHP       string `json:"php"`
		Node      string `json:"node"`
		BuildDir  string `json:"buildDir"`
		DevPort   int    `json:"devPort"`
		Mode      string `json:"mode"`
		DevPaused bool   `json:"devPaused"`
		Secure    bool   `json:"secure"`
		URL       string `json:"url"`
	}
	sites := make([]site, 0, len(a.State.Sites))
	for _, s := range a.State.Sites {
		scheme := "http"
		if s.Secure {
			scheme = "https"
		}
		kind := s.Kind
		if kind == "" {
			kind = "php"
		}
		devPort := 0
		if kind == "node" {
			devPort = devserver.Running(a.Paths, s.Name)
		}
		mode := s.Mode
		if kind == "node" && mode == "" {
			mode = "dev"
		}
		sites = append(sites, site{
			Name:      s.Name,
			Host:      a.State.Host(s),
			Path:      s.Path,
			Kind:      kind,
			PHP:       s.PHP,
			Node:      s.Node,
			BuildDir:  s.BuildDir,
			DevPort:   devPort,
			Mode:      mode,
			DevPaused: s.DevPaused,
			Secure:    s.Secure,
			URL:       scheme + "://" + a.State.Host(s),
		})
	}

	nodeInstalled, _ := nodever.Installed(a.Paths)
	if nodeInstalled == nil {
		nodeInstalled = []string{}
	}

	mysqlState := map[string]any{"installed": false}
	if v := a.State.Config.MySQL; v != "" {
		mysqlState = map[string]any{
			"hasPassword": a.State.Config.MySQLPassword != "",
			"installed":   true,
			"version":     v,
			"label":       mysql.Label(v),
			"port":        mysql.Port,
			"running":     mysql.Running(),
		}
	}

	return map[string]any{
		"caddy":         caddy.Running(),
		"globalPhp":     a.State.Config.GlobalPHP,
		"phpInstalled":  installed,
		"nodeInstalled": nodeInstalled,
		"globalNode":    a.State.Config.GlobalNode,
		"phpCgi":        cgis,
		"mysql":         mysqlState,
		"sites":         sites,
		"tld":           a.State.Config.TLD,
		"heidisql":      heidisql.Installed(a.Paths),
		"windows":       runtime.GOOS == "windows",
		"homeDir":       homeDir(),
		"backupsDir":    a.Paths.BackupsDir(),
		"autostart":     autostart.Enabled(),
		"phpShadow":     a.PhpShadow(),
		"time":          time.Now().Format("15:04:05"),
	}, nil
}

// examplePath is the platform-appropriate sample project path shown in
// error messages and placeholders.
func examplePath() string {
	if runtime.GOOS == "windows" {
		return `C:\code\myapp`
	}
	return filepath.Join(homeDir(), "code", "myapp")
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return home
}
