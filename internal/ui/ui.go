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
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "embed"

	"pm/internal/app"
	"pm/internal/autostart"
	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/fcgi"
	"pm/internal/heidisql"
	"pm/internal/mysql"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

//go:embed index.html
var indexHTML []byte

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
		_ = exec.Command("cmd", "/c", "start", "", url).Start()
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

// chromiumBrowsers are the browsers that support --app windows.
var chromiumBrowsers = map[string]bool{
	"msedge.exe": true, "chrome.exe": true, "brave.exe": true,
	"vivaldi.exe": true, "opera.exe": true, "chromium.exe": true,
}

// defaultBrowser resolves the exe the user's https links open with.
func defaultBrowser() (string, bool) {
	out, _ := exec.Command("powershell", "-NoProfile", "-Command",
		`$p = (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice' -ErrorAction SilentlyContinue).ProgId; if ($p) { (Get-ItemProperty ("Registry::HKEY_CLASSES_ROOT\" + $p + "\shell\open\command") -ErrorAction SilentlyContinue).'(default)' }`).Output()
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return "", false
	}
	exe := cmdline
	if strings.HasPrefix(cmdline, `"`) {
		if end := strings.Index(cmdline[1:], `"`); end > 0 {
			exe = cmdline[1 : 1+end]
		}
	} else if i := strings.Index(cmdline, ".exe"); i > 0 {
		exe = cmdline[:i+4]
	}
	if _, err := os.Stat(exe); err != nil {
		return "", false
	}
	return exe, true
}

// openAppWindow launches the panel as a standalone app-mode window — in
// the user's DEFAULT browser when it's Chromium-based, falling back to
// Edge/Chrome. The dedicated profile dir forces a separate process whose
// lifetime matches the window, so we know when it closes. Errors when no
// app-capable browser fits (e.g. the default is Firefox) — the caller
// then opens a plain tab in the default browser instead.
func openAppWindow(url string) (<-chan struct{}, error) {
	paths, err := pmdir.New()
	if err != nil {
		return nil, err
	}

	// A lingering browser from a previous panel session swallows the new
	// launch: Chromium single-instances per user-data-dir, so the fresh
	// process delegates to the old one and exits — which looks like
	// "nothing opened". Clear any old instance first.
	profile := strings.ReplaceAll(paths.Home+`\ui-profile`, "'", "''")
	_ = exec.Command("powershell", "-NoProfile", "-Command", fmt.Sprintf(
		`Get-CimInstance Win32_Process | Where-Object { ('msedge.exe','chrome.exe','brave.exe','vivaldi.exe','opera.exe','chromium.exe') -contains $_.Name -and $_.CommandLine -like '*%s*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		profile)).Run()

	var candidates []string
	if exe, ok := defaultBrowser(); ok {
		if !chromiumBrowsers[strings.ToLower(filepath.Base(exe))] {
			// The user's browser can't do app windows — respect their
			// choice with a normal tab rather than forcing Edge.
			return nil, errors.New("default browser has no app mode")
		}
		candidates = append(candidates, exe)
	}
	if v := os.Getenv("ProgramFiles(x86)"); v != "" {
		candidates = append(candidates, v+`\Microsoft\Edge\Application\msedge.exe`)
	}
	if v := os.Getenv("ProgramFiles"); v != "" {
		candidates = append(candidates,
			v+`\Microsoft\Edge\Application\msedge.exe`,
			v+`\Google\Chrome\Application\chrome.exe`)
	}
	if v := os.Getenv("LocalAppData"); v != "" {
		candidates = append(candidates, v+`\Google\Chrome\Application\chrome.exe`)
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		// The sign-in/sync flags matter: on a fresh profile Edge silently
		// signs the Windows account in and shows a "syncing your data"
		// splash — the panel must be a plain window, nothing more.
		cmd := exec.Command(exe,
			"--app="+url,
			"--user-data-dir="+paths.Home+`\ui-profile`,
			"--window-size=1080,780",
			"--no-first-run", "--no-default-browser-check",
			"--disable-sync",
			"--disable-features=msImplicitSignin,msSeamlessWebToBrowserSignIn,msSyncPromoAfterImplicitSignIn,msFirstRunExperience,msEdgeWelcomePage,SyncPromo,SigninInterceptBubble")
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		return done, nil
	}
	return nil, errors.New("no app-mode browser found")
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
		if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
			return nil, err
		}
		return nil, mysql.Start(a.Paths, v)
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
			return nil, fmt.Errorf("enter a full path, e.g. C:\\code\\myapp")
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
		a.State.AddSite(config.Site{Name: name, Path: path})
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
		Name   string `json:"name"`
		Host   string `json:"host"`
		Path   string `json:"path"`
		PHP    string `json:"php"`
		Secure bool   `json:"secure"`
		URL    string `json:"url"`
	}
	sites := make([]site, 0, len(a.State.Sites))
	for _, s := range a.State.Sites {
		scheme := "http"
		if s.Secure {
			scheme = "https"
		}
		sites = append(sites, site{
			Name:   s.Name,
			Host:   a.State.Host(s),
			Path:   s.Path,
			PHP:    s.PHP,
			Secure: s.Secure,
			URL:    scheme + "://" + a.State.Host(s),
		})
	}

	mysqlState := map[string]any{"installed": false}
	if v := a.State.Config.MySQL; v != "" {
		mysqlState = map[string]any{
			"installed": true,
			"version":   v,
			"label":     mysql.Label(v),
			"port":      mysql.Port,
			"running":   mysql.Running(),
		}
	}

	return map[string]any{
		"caddy":        caddy.Running(),
		"globalPhp":    a.State.Config.GlobalPHP,
		"phpInstalled": installed,
		"phpCgi":       cgis,
		"mysql":        mysqlState,
		"sites":        sites,
		"tld":          a.State.Config.TLD,
		"heidisql":     heidisql.Installed(a.Paths),
		"autostart":    autostart.Enabled(),
		"phpShadow":    a.PhpShadow(),
		"time":         time.Now().Format("15:04:05"),
	}, nil
}
