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
	"io"
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
	"pm/internal/detect"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/gitops"
	"pm/internal/heidisql"
	"pm/internal/mongodb"
	"pm/internal/mongoexpress"
	"pm/internal/mysql"
	"pm/internal/nodever"
	"pm/internal/pgadmin"
	"pm/internal/phpmyadmin"
	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/postgres"
	"pm/internal/sysproc"
	"pm/internal/version"
)

//go:embed index.html
var indexHTML []byte

// Served as a real PNG favicon: Chromium app windows take their
// taskbar icon from it — an SVG data-URI icon gets ignored and the
// window shows the browser's own icon instead.
//
//go:embed favicon.png
var faviconPNG []byte

// bringServicesUp starts the configured stack in the background:
// opening the panel means "I want my stack", so the window/tab (or, in
// window-host mode, the app) appears with things already starting up
// instead of greeting the user with "Stopped" and a button to press.
func bringServicesUp() {
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
}

// startPanel brings the stack up in the background and serves the
// control panel on an ephemeral 127.0.0.1 port. It's shared by Run and
// RunHost; callers are responsible for shutting srv down.
func startPanel() (srv *http.Server, url string, lastSeen *atomic.Int64, err error) {
	go bringServicesUp()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, "", nil, err
	}

	// Track requests so tab-mode (no window to watch) can shut the
	// server down once the page is gone — it polls every 5 seconds.
	lastSeen = &atomic.Int64{}
	lastSeen.Store(time.Now().Unix())
	mux := newMux(token)
	srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastSeen.Store(time.Now().Unix())
		mux.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)

	url = fmt.Sprintf("http://%s/?t=%s", ln.Addr().String(), token)
	return srv, url, lastSeen, nil
}

// Run serves the control panel and blocks until its window is closed
// (or ctx is cancelled when no app window could be opened).
func Run(ctx context.Context) error {
	srv, url, lastSeen, err := startPanel()
	if err != nil {
		return err
	}
	defer srv.Shutdown(context.Background())

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

// RunHost serves the same control panel for the native Mullion.app
// window host, which runs `mullion ui --window-host` as a child
// process and drives its own window against the printed URL. Unlike
// Run: no browser/window is opened, exactly one line —
// "MULLION_UI_URL=<url>" — goes to out (stdout; everything else must
// go to stderr), and the panel keeps serving until stdin reaches EOF
// or ctx is cancelled, instead of exiting on idle polling or a closed
// window.
func RunHost(ctx context.Context, stdin io.Reader, out io.Writer) error {
	srv, url, _, err := startPanel()
	if err != nil {
		return err
	}
	defer srv.Shutdown(context.Background())

	fmt.Fprintf(out, "MULLION_UI_URL=%s\n", url)

	stdinClosed := make(chan struct{})
	go func() {
		io.Copy(io.Discard, stdin)
		close(stdinClosed)
	}()

	select {
	case <-stdinClosed:
	case <-ctx.Done():
	}
	return nil
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
	registerTerminal(mux, token)
	registerProjectAssets(mux)

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

	// The project page registers its own endpoints (project.go).
	registerProject(api)
	registerPrefs(api)
	registerMterm(api)

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
	api("/api/php/ini", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return phpver.ListIni(a.Paths, in.Version)
	})
	api("/api/php/ini/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Version string
			Key     string
			Value   string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if err := phpver.SetIni(a.Paths, in.Version, in.Key, in.Value); err != nil {
			return nil, err
		}
		// Restart the version's php-fpm/php-cgi so running sites see the change.
		return nil, a.RestartPhp(in.Version)
	})
	api("/api/php/ini/open", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		iniPath := filepath.Join(a.Paths.PhpVersionDir(in.Version), "php.ini")
		return nil, openIniFile(iniPath)
	})
	api("/api/mysql/switch", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Resolving " + in.Version + "…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			version, err := mysql.ResolveVersionArg(context.Background(), in.Version)
			if err != nil {
				return nil, err
			}
			setStatus("Switching to " + mysql.Label(version) + "… (downloading + migrating)")
			// From the panel there is no prompt: migrating the databases
			// is always the safe choice (everything is backed up first
			// anyway).
			if _, err := app.SwitchDatabase(context.Background(), a2, version, true); err != nil {
				return nil, err
			}
			return mysql.Label(version), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/mysql/start", func(a *app.App, r *http.Request) (any, error) {
		v := a.State.Config.MySQL
		if v == "" {
			return nil, errors.New("MySQL is not installed (run: mullion mysql install)")
		}
		// A foreign MySQL (a resurrected brew service, Laragon...) on the
		// port means every connection hits the WRONG server. Its data
		// deserves the backup flow, which needs a terminal — never
		// replace it silently from a button.
		if mysql.Running() && len(sysproc.ProcessesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) == 0 {
			_, name := sysproc.PortOwner(mysql.Port)
			return nil, fmt.Errorf("another MySQL server (%s) is holding port %d — run `mullion mysql start` in a terminal: it offers to back up that server's databases before taking over", name, mysql.Port)
		}
		if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
			return nil, err
		}
		return nil, a.StartMySQL()
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
		if a.State.Config.MySQL == "" {
			return nil, nil
		}
		return nil, a.StopMySQL()
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
	api("/api/sites/rename", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name, NewName string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		site := a.State.FindSite(in.Name)
		if site == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		// The dev server's pid/log files are keyed by name — stop it
		// under the old name; Apply restarts it under the new one.
		if site.Kind == "node" {
			devserver.Stop(a.Paths, site.Name)
		}
		if err := a.State.RenameSite(in.Name, in.NewName); err != nil {
			return nil, err
		}
		return nil, a.Apply()
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
			buildDir, err := a.EnsureBuildOutput(site.Path, site.BuildDir)
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
		if err := a.Apply(); err != nil {
			return nil, err
		}
		// Apply reports dev-server startup problems only on stdout —
		// which a background panel process cannot show. Verify and
		// surface the reason here instead of leaving a dead domain.
		if devserver.Running(a.Paths, site.Name) == 0 {
			if _, err := a.NodeVersionDirFor(*site); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("the dev server did not come up — last log lines:\n%s", devserver.LogTail(a.Paths, site.Name))
		}
		return nil, nil
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
	api("/api/pick-folder", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Title string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return pickFolder(in.Title)
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
	api("/api/job", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Id string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		st, ok := jobs.get(in.Id)
		if !ok {
			return nil, fmt.Errorf("no such job %q (it may have finished more than 10 minutes ago)", in.Id)
		}
		return st, nil
	})

	api("/api/pg/series", func(a *app.App, r *http.Request) (any, error) {
		return a.PostgresSeries(r.Context())
	})
	api("/api/pg/install", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Downloading PostgreSQL " + in.Version + "…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			version, err := a2.InstallPostgres(context.Background(), in.Version)
			if err != nil {
				return nil, err
			}
			return postgres.Label(version), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/pg/start", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StartPostgres()
	})
	api("/api/pg/stop", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StopPostgres()
	})
	api("/api/pg/password", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Password string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.SetPostgresPassword(in.Password)
	})
	api("/api/pg/db/list", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.Postgres == "" || !postgres.Running() {
			return []string{}, nil
		}
		dbs, err := a.PostgresDatabases()
		if err != nil {
			return nil, err
		}
		if dbs == nil {
			dbs = []string{}
		}
		return dbs, nil
	})
	api("/api/pg/db/create", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.CreatePostgresDB(in.Name)
	})
	api("/api/pg/db/drop", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.DropPostgresDB(in.Name)
	})
	api("/api/pgadmin/open", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.Postgres == "" {
			return nil, errors.New("PostgreSQL is not installed (install it first, above)")
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			if !pgadmin.Installed(a2.Paths) {
				setStatus("Downloading pgAdmin… (~290 MB, about a minute or two)")
				if err := pgadmin.Install(context.Background(), a2.Paths, a2.State.Config.Postgres); err != nil {
					return nil, err
				}
			}
			setStatus("Opening pgAdmin…")
			if err := pgadmin.Launch(a2.Paths); err != nil {
				return nil, err
			}
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})

	api("/api/mongo/series", func(a *app.App, r *http.Request) (any, error) {
		return a.MongoSeries(r.Context())
	})
	api("/api/mongo/install", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Downloading MongoDB " + in.Version + "…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			version, err := a2.InstallMongo(context.Background(), in.Version)
			if err != nil {
				return nil, err
			}
			return mongodb.Label(version), nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/mongo/start", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StartMongo()
	})
	api("/api/mongo/stop", func(a *app.App, r *http.Request) (any, error) {
		return nil, a.StopMongo()
	})
	api("/api/mongo/db/list", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.Mongo == "" || !mongodb.Running() {
			return []string{}, nil
		}
		dbs, err := a.MongoDatabases()
		if err != nil {
			return nil, err
		}
		if dbs == nil {
			dbs = []string{}
		}
		return dbs, nil
	})
	api("/api/mongo/db/create", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.CreateMongoDB(in.Name)
	})
	api("/api/mongo/db/drop", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.DropMongoDB(in.Name)
	})
	api("/api/mongo-express/open", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.Mongo == "" {
			return nil, errors.New("MongoDB is not installed (install it first, above)")
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			// mongo-express can't show anything without a server behind it.
			if !mongodb.Running() {
				setStatus("Starting MongoDB…")
				if err := a2.StartMongo(); err != nil {
					return nil, err
				}
			}
			setStatus("Installing mongo-express…")
			return mongoexpress.Ensure(context.Background(), a2)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})

	api("/api/phpmyadmin/install", func(a *app.App, r *http.Request) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			setStatus("Installing phpMyAdmin…")
			return phpmyadmin.EnsureLinked(context.Background(), a2, "")
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/admintool/uninstall", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Tool string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.UninstallAdminTool(in.Tool)
	})

	api("/api/backups", func(a *app.App, r *http.Request) (any, error) {
		list, err := a.ListBackups()
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []app.BackupInfo{}
		}
		return list, nil
	})
	api("/api/engine/backup", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Engine string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Backing up " + in.Engine + "…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return a2.BackupEngine(context.Background(), in.Engine)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/backups/delete", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Dir string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.DeleteBackup(in.Dir)
	})
	api("/api/backups/restore", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Dir, Db string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Restoring backup…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.RestoreBackup(context.Background(), in.Dir, in.Db)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/engine/uninstall", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Engine      string
			BackupFirst bool
			DeleteData  bool
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			if in.BackupFirst {
				setStatus("Backing up…")
			} else {
				setStatus("Removing…")
			}
			return a2.UninstallEngine(context.Background(), in.Engine, in.BackupFirst, in.DeleteData)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/open-path", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Path string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, openInFileManager(in.Path)
	})
	api("/api/tld", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Tld string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Updating the hosts file and certificates…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.SetTLD(in.Tld)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})

	api("/api/pick-file", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Title string
			Types []string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return pickFile(in.Title, in.Types)
	})
	api("/api/php/uninstall", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.UninstallPhp(in.Version)
	})
	api("/api/node/uninstall", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.UninstallNode(in.Version)
	})
	api("/api/node/npm", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Npm, Node string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Installing npm " + in.Npm + "…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.SetNpmVersion(context.Background(), in.Npm, in.Node)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/node/info", func(a *app.App, r *http.Request) (any, error) {
		return a.NodeInfo(), nil
	})
	api("/api/dev/restart", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.RestartDevServer(in.Name)
	})
	api("/api/restart", func(a *app.App, r *http.Request) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Restarting the stack…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.RestartStack(context.Background())
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/composer/install", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Installing Composer…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return a2.InstallComposer(context.Background(), in.Version)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/mysql/restore", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Path string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Importing backup…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.RestoreMySQL(context.Background(), in.Path)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/doctor", func(a *app.App, r *http.Request) (any, error) {
		// Read-only checks: don't queue behind a running install.
		id := jobs.start(func(setStatus func(string)) (any, error) {
			setStatus("Running diagnostics…")
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return a2.Doctor(context.Background()), nil
		})
		return map[string]string{"job": id}, nil
	})
	registerPanelPages(api)
	return mux
}

func getState(a *app.App, r *http.Request) (any, error) {
	installed, err := phpver.Installed(a.Paths)
	if err != nil {
		return nil, err
	}
	if installed == nil {
		installed = []string{} // the page reads .length on a fresh install
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
		// Sites-page extras: detected project info, the pin flag, and
		// every hostname the site answers on (apex first).
		Detect  detect.Info `json:"detect"`
		Pinned  bool        `json:"pinned"`
		Aliases []string    `json:"aliases"`
		Hosts   []string    `json:"hosts"`
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
			Detect:    detect.Project(s.Path),
			Pinned:    s.Pinned,
			Aliases:   nonNilStrings(s.Aliases),
			Hosts:     app.SiteHosts(s, a.State.Config.TLD),
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
			"stopped":     a.State.Config.MySQLStopped,
		}
	}

	postgresState := map[string]any{
		"installed":        false,
		"pgadminInstalled": pgadmin.Installed(a.Paths),
	}
	if v := a.State.Config.Postgres; v != "" {
		postgresState["installed"] = true
		postgresState["version"] = v
		postgresState["label"] = postgres.Label(v)
		postgresState["port"] = postgres.Port
		postgresState["running"] = postgres.Running()
		postgresState["stopped"] = a.State.Config.PostgresStopped
		postgresState["hasPassword"] = a.State.Config.PostgresPassword != ""
	}

	mongoState := map[string]any{
		"installed": false,
		"uiLinked":  a.State.FindSite(mongoexpress.SiteName) != nil,
	}
	if v := a.State.Config.Mongo; v != "" {
		mongoState["installed"] = true
		mongoState["version"] = v
		mongoState["label"] = mongodb.Label(v)
		mongoState["port"] = mongodb.Port
		mongoState["running"] = mongodb.Running()
		mongoState["stopped"] = a.State.Config.MongoStopped
	}

	return map[string]any{
		"version":       version.Number,
		"caddy":         caddy.Running(),
		"globalPhp":     a.State.Config.GlobalPHP,
		"phpInstalled":  installed,
		"nodeInstalled": nodeInstalled,
		"nodeNpm":       nonNilNodeNpm(a.NodeVersionsWithNpm()),
		"globalNode":    a.State.Config.GlobalNode,
		"phpCgi":        cgis,
		"mysql":         mysqlState,
		"postgres":      postgresState,
		"mongo":         mongoState,
		"sites":         sites,
		"tld":           a.State.Config.TLD,
		"heidisql":      heidisql.Installed(a.Paths),
		"windows":       runtime.GOOS == "windows",
		"homeDir":       homeDir(),
		"backupsDir":    a.Paths.BackupsDir(),
		"autostart":     autostart.Enabled(),
		"phpShadow":     a.PhpShadow(),
		"adminTools":    a.AdminTools(),
		"composer":      a.ComposerVersion(),
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

// registerPanelPages adds the endpoints behind the Sites cards, the
// Settings backup-folder card, the Node page's npm picker, and the
// Ports & SSL page. Anything that can take a while — or asks for an
// admin password — runs as a job (see jobs.go).
func registerPanelPages(api func(path string, h func(a *app.App, r *http.Request) (any, error))) {
	/* ── sites ──────────────────────────────────────────────── */
	type siteInfo struct {
		app.SiteInfo
		Aliases []string `json:"aliases"`
		Hosts   []string `json:"hosts"`
		// Git is the card's branch chip (absent outside a repository).
		Git *gitops.Summary `json:"git,omitempty"`
	}
	api("/api/sites/info", func(a *app.App, r *http.Request) (any, error) {
		out := []siteInfo{}
		git := a.GitSummaries(r.Context())
		for _, si := range a.SitesInfo() {
			s := a.State.FindSite(si.Name)
			if s == nil {
				continue
			}
			out = append(out, siteInfo{SiteInfo: si, Aliases: nonNilStrings(s.Aliases), Hosts: app.SiteHosts(*s, a.State.Config.TLD), Git: git[si.Name]})
		}
		return out, nil
	})
	api("/api/sites/pin", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Name   string
			Pinned bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return nil, a.SetSitePinned(in.Name, in.Pinned)
	})
	// Read-only: the cards only show aliases as chips — editing them
	// belongs to the project page.
	api("/api/sites/aliases", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		s := a.State.FindSite(in.Name)
		if s == nil {
			return nil, fmt.Errorf("no site named %q", in.Name)
		}
		return map[string]any{
			"aliases": nonNilStrings(s.Aliases),
			"hosts":   app.SiteHosts(*s, a.State.Config.TLD),
		}, nil
	})

	/* ── settings: backup folder ────────────────────────────── */
	api("/api/settings/backup-dir", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Action string // "" / "get", "set", "reset"
			Dir    string
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		switch in.Action {
		case "", "get":
		case "set":
			if strings.TrimSpace(in.Dir) == "" {
				return nil, errors.New("choose a folder first")
			}
			if err := a.SetBackupDir(in.Dir); err != nil {
				return nil, err
			}
		case "reset":
			if err := a.SetBackupDir(""); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown action %q", in.Action)
		}
		dir, isDefault, free := a.BackupDirInfo()
		return map[string]any{"dir": dir, "isDefault": isDefault, "freeBytes": free}, nil
	})

	/* ── node: npm versions ─────────────────────────────────── */
	api("/api/node/npm-versions", func(a *app.App, r *http.Request) (any, error) {
		versions, err := nodever.NpmVersions(r.Context())
		if err != nil {
			return nil, err
		}
		return nonNilStrings(versions), nil
	})

	/* ── ports ──────────────────────────────────────────────── */
	api("/api/ports", func(a *app.App, r *http.Request) (any, error) {
		list := a.PortsOverview()
		if list == nil {
			list = []app.PortInfo{}
		}
		return list, nil
	})
	api("/api/ports/suggest", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Start int }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return a.SuggestFreePort(in.Start), nil
	})
	// Port changes restart services (and can fail on a foreign
	// listener): jobs. A PortInUseError is not a failure the page should
	// toast — it's a question ("use anyway?") — so it comes back as a
	// result with inUse set.
	portJob := func(label string, set func(a2 *app.App) ([]string, error)) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus(label)
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			hints, err := set(a2)
			var inUse *app.PortInUseError
			if errors.As(err, &inUse) {
				return map[string]any{"inUse": map[string]any{"port": inUse.Port, "pid": inUse.PID, "name": inUse.Name}, "message": inUse.Error()}, nil
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"hints": nonNilStrings(hints)}, nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	}
	api("/api/ports/engine", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Engine string
			Port   int
			Force  bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return portJob(fmt.Sprintf("Moving %s to port %d…", in.Engine, in.Port), func(a2 *app.App) ([]string, error) {
			return a2.SetEnginePort(in.Engine, in.Port, in.Force)
		})
	})
	api("/api/ports/dev", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site  string
			Port  int
			Force bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		return portJob(fmt.Sprintf("Moving %s's dev server to port %d…", in.Site, in.Port), func(a2 *app.App) ([]string, error) {
			return a2.SetSiteDevPort(in.Site, in.Port, in.Force)
		})
	})

	/* ── SSL ────────────────────────────────────────────────── */
	api("/api/ssl", func(a *app.App, r *http.Request) (any, error) {
		return a.SSLInfo(), nil
	})
	api("/api/ssl/trust", func(a *app.App, r *http.Request) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			if runtime.GOOS == "windows" {
				setStatus("Trusting the certificate… (confirm the Windows security prompt)")
			} else {
				setStatus("Trusting the certificate… (macOS asks for your password)")
			}
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.TrustCA()
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/ssl/export", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Dest string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Dest) == "" {
			return nil, errors.New("choose a folder first")
		}
		target := app.ExportCATarget(in.Dest)
		if err := a.ExportCA(in.Dest); err != nil {
			return nil, err
		}
		return target, nil
	})
	api("/api/ssl/secure-all", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Secure bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			if in.Secure {
				setStatus("Switching every site to HTTPS…")
			} else {
				setStatus("Switching every site to plain HTTP…")
			}
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.SecureAll(in.Secure)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})

	/* ── wildcard DNS ───────────────────────────────────────── */
	api("/api/dns", func(a *app.App, r *http.Request) (any, error) {
		enabled, resolver, server, note := a.WildcardDNSStatus()
		return map[string]any{
			"enabled":           enabled,
			"resolverInstalled": resolver,
			"serverRunning":     server,
			"note":              note,
			"tld":               a.State.Config.TLD,
		}, nil
	})
	api("/api/dns/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Enable bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			if in.Enable {
				setStatus("Turning on wildcard DNS… (asks for administrator rights)")
			} else {
				setStatus("Turning off wildcard DNS…")
			}
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return nil, a2.SetWildcardDNS(in.Enable)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
}

// decodeJSON decodes an optional JSON request body: a GET (or an empty
// POST) leaves v at its zero value instead of failing.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// nonNilStrings keeps JSON arrays arrays: the page reads .length.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilNodeNpm(s []app.NodeNpm) []app.NodeNpm {
	if s == nil {
		return []app.NodeNpm{}
	}
	return s
}
