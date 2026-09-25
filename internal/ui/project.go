package ui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"pm/internal/app"
	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/detect"
	"pm/internal/devserver"
	"pm/internal/gitops"
	"pm/internal/nodever"
	"pm/internal/phpver"
	"pm/internal/pkgsearch"
)

// apiRegistrar registers one token-checked JSON endpoint (see newMux).
type apiRegistrar = func(path string, h func(a *app.App, r *http.Request) (any, error))

//go:embed project.js project.css
var projectAssets embed.FS

// registerProjectAssets serves the project page's static files. They
// are same-origin and carry no secrets, so no token is needed.
func registerProjectAssets(mux *http.ServeMux) {
	serve := func(file, contentType string) {
		mux.HandleFunc("/"+file, func(w http.ResponseWriter, r *http.Request) {
			data, err := projectAssets.ReadFile(file)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		})
	}
	serve("project.js", "text/javascript; charset=utf-8")
	serve("project.css", "text/css; charset=utf-8")
}

// projectRuns tracks the cancel functions of running command/package
// jobs, so the page's Stop button can kill them.
var projectRuns = struct {
	sync.Mutex
	m map[string]context.CancelFunc
}{m: map[string]context.CancelFunc{}}

// startCancellable starts a streaming job whose context /api/project/cancel
// can cancel. The entry is dropped once the job finishes.
func startCancellable(fn func(ctx context.Context, setStatus func(string), appendLog func([]byte)) (any, error)) string {
	ctx, cancel := context.WithCancel(context.Background())
	idCh := make(chan string, 1)
	id := jobs.startStream(func(setStatus func(string), appendLog func([]byte)) (any, error) {
		defer func() {
			cancel()
			id := <-idCh
			projectRuns.Lock()
			delete(projectRuns.m, id)
			projectRuns.Unlock()
		}()
		res, err := fn(ctx, setStatus, appendLog)
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			err = errors.New("stopped")
		}
		return res, err
	})
	projectRuns.Lock()
	projectRuns.m[id] = cancel
	projectRuns.Unlock()
	idCh <- id
	return id
}

// packageOps allows one Composer/npm operation per site at a time: two
// installs racing on the same vendor/ or node_modules/ would corrupt it.
var packageOps = struct {
	sync.Mutex
	busy map[string]bool
}{busy: map[string]bool{}}

func claimPackageOp(site string) bool {
	packageOps.Lock()
	defer packageOps.Unlock()
	if packageOps.busy[site] {
		return false
	}
	packageOps.busy[site] = true
	return true
}

func releasePackageOp(site string) {
	packageOps.Lock()
	delete(packageOps.busy, site)
	packageOps.Unlock()
}

// projectSite looks a site up by the request's name.
func projectSite(a *app.App, name string) (*config.Site, error) {
	site := a.State.FindSite(name)
	if site == nil {
		return nil, fmt.Errorf("no site named %q", name)
	}
	return site, nil
}

func siteKind(s config.Site) string {
	if s.Kind == "" {
		return "php"
	}
	return s.Kind
}

func fileExistsAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// projectInfo is everything the project page's header, Domains and
// Settings tabs need about one site.
func projectInfo(a *app.App, s config.Site) map[string]any {
	kind := siteKind(s)
	host := a.State.Host(s)
	scheme := "http"
	if s.Secure {
		scheme = "https"
	}
	info := detect.Project(s.Path)

	phpInstalled, _ := phpver.Installed(a.Paths)
	if phpInstalled == nil {
		phpInstalled = []string{}
	}
	nodeInstalled, _ := nodever.Installed(a.Paths)
	if nodeInstalled == nil {
		nodeInstalled = []string{}
	}

	out := map[string]any{
		"name":          s.Name,
		"path":          s.Path,
		"kind":          kind,
		"host":          host,
		"url":           scheme + "://" + host,
		"secure":        s.Secure,
		"tld":           a.State.Config.TLD,
		"aliases":       nonNil(s.Aliases),
		"hosts":         app.SiteHosts(s, a.State.Config.TLD),
		"detect":        info,
		"workers":       len(s.Workers),
		"pinned":        s.Pinned,
		"phpPinned":     s.PHP,
		"globalPhp":     a.State.Config.GlobalPHP,
		"phpInstalled":  phpInstalled,
		"nodePinned":    s.Node,
		"globalNode":    a.State.Config.GlobalNode,
		"nodeInstalled": nodeInstalled,
		"hasComposer":   fileExistsAt(filepath.Join(s.Path, "composer.json")),
		"hasPackage":    fileExistsAt(filepath.Join(s.Path, "package.json")),
		"windows":       runtime.GOOS == "windows",
		"caddy":         caddy.Running(),
		"wildcardDNS":   map[string]any{"enabled": a.State.Config.WildcardDNS},
	}
	if kind == "php" {
		out["php"] = a.SiteVersion(s)
	}
	if kind == "node" {
		mode := s.Mode
		if mode == "" {
			mode = "dev"
		}
		out["mode"] = mode
		out["buildDir"] = s.BuildDir
		out["devPort"] = s.DevPort
		out["devRunning"] = devserver.Running(a.Paths, s.Name)
		out["devPaused"] = s.DevPaused
		if dir, err := a.NodeVersionDirFor(s); err == nil {
			out["node"] = filepath.Base(dir)
		}
	}
	// The full wildcard check probes the DNS server, so it only runs when
	// the site actually uses the "*" alias.
	for _, l := range s.Aliases {
		if l == app.WildcardAlias {
			enabled, resolver, server, note := a.WildcardDNSStatus()
			out["wildcardDNS"] = map[string]any{
				"enabled": enabled, "resolverInstalled": resolver,
				"serverRunning": server, "note": note,
			}
			break
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// registerProject adds the project page's endpoints.
func registerProject(api apiRegistrar) {
	registerProjectGit(api)

	type siteReq struct {
		Site string `json:"site"`
	}

	api("/api/project/info", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		s, err := projectSite(a, in.Site)
		if err != nil {
			return nil, err
		}
		return projectInfo(a, *s), nil
	})

	// ── commands ────────────────────────────────────────────────────
	api("/api/project/commands", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if _, err := projectSite(a, in.Site); err != nil {
			return nil, err
		}
		cmds, err := a.ProjectCommands(r.Context(), in.Site)
		if cmds == nil {
			cmds = []app.ProjectCommand{}
		}
		// A source that failed to list (an app that can't boot) is a
		// warning next to the rest, not a failed request.
		out := map[string]any{"commands": cmds}
		if err != nil {
			out["error"] = err.Error()
		}
		return out, nil
	})
	api("/api/project/run", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site    string `json:"site"`
			Command string `json:"command"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if _, err := projectSite(a, in.Site); err != nil {
			return nil, err
		}
		command := strings.TrimSpace(in.Command)
		if command == "" {
			return nil, errors.New("type a command to run")
		}
		if strings.ContainsAny(command, "\r\n") {
			return nil, errors.New("a command must be a single line")
		}
		id := startCancellable(func(ctx context.Context, setStatus func(string), appendLog func([]byte)) (any, error) {
			setStatus("Running " + command + "…")
			start := time.Now()
			code, err := a.RunProjectCommand(ctx, in.Site, command, appendLog)
			if err != nil {
				return nil, err
			}
			return map[string]any{"exitCode": code, "ms": time.Since(start).Milliseconds()}, nil
		})
		return map[string]string{"job": id}, nil
	})
	api("/api/project/cancel", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		projectRuns.Lock()
		cancel := projectRuns.m[in.ID]
		projectRuns.Unlock()
		if cancel == nil {
			return nil, errors.New("that job is not running any more")
		}
		cancel()
		return nil, nil
	})

	// ── workers ─────────────────────────────────────────────────────
	type workerReq struct {
		Site      string `json:"site"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Command   string `json:"command"`
		AutoStart bool   `json:"autoStart"`
		Lines     int    `json:"lines"`
	}
	decodeWorker := func(a *app.App, r *http.Request) (workerReq, error) {
		var in workerReq
		if err := decodeBody(r, &in); err != nil {
			return in, err
		}
		_, err := projectSite(a, in.Site)
		return in, err
	}
	api("/api/project/workers", func(a *app.App, r *http.Request) (any, error) {
		in, err := decodeWorker(a, r)
		if err != nil {
			return nil, err
		}
		list := a.WorkersStatus(in.Site)
		if list == nil {
			list = []app.WorkerView{}
		}
		return map[string]any{"workers": list}, nil
	})
	api("/api/project/workers/templates", func(a *app.App, r *http.Request) (any, error) {
		in, err := decodeWorker(a, r)
		if err != nil {
			return nil, err
		}
		s := a.State.FindSite(in.Site)
		fw := detect.Project(s.Path).Framework
		t := app.WorkerTemplates(fw, s.Path)
		if t == nil {
			t = []config.Worker{}
		}
		return map[string]any{"templates": t, "framework": fw}, nil
	})
	api("/api/project/workers/add", func(a *app.App, r *http.Request) (any, error) {
		in, err := decodeWorker(a, r)
		if err != nil {
			return nil, err
		}
		w, err := a.AddWorker(in.Site, config.Worker{
			Name: in.Name, Kind: in.Kind, Command: in.Command, AutoStart: in.AutoStart,
		})
		if err != nil && w.ID != "" {
			return nil, fmt.Errorf("added %s, but it failed to start: %w", w.Name, err)
		}
		return w, err
	})
	api("/api/project/workers/update", func(a *app.App, r *http.Request) (any, error) {
		in, err := decodeWorker(a, r)
		if err != nil {
			return nil, err
		}
		return nil, a.UpdateWorker(in.Site, config.Worker{
			ID: in.ID, Name: in.Name, Kind: in.Kind, Command: in.Command, AutoStart: in.AutoStart,
		})
	})
	workerAction := func(path string, fn func(a *app.App, site, id string) error) {
		api(path, func(a *app.App, r *http.Request) (any, error) {
			in, err := decodeWorker(a, r)
			if err != nil {
				return nil, err
			}
			if in.ID == "" {
				return nil, errors.New("no worker id")
			}
			return nil, fn(a, in.Site, in.ID)
		})
	}
	workerAction("/api/project/workers/remove", func(a *app.App, site, id string) error { return a.RemoveWorker(site, id) })
	workerAction("/api/project/workers/start", func(a *app.App, site, id string) error { return a.StartWorker(site, id) })
	workerAction("/api/project/workers/stop", func(a *app.App, site, id string) error { return a.StopWorker(site, id) })
	workerAction("/api/project/workers/restart", func(a *app.App, site, id string) error { return a.RestartWorker(site, id) })
	api("/api/project/workers/log", func(a *app.App, r *http.Request) (any, error) {
		in, err := decodeWorker(a, r)
		if err != nil {
			return nil, err
		}
		lines := in.Lines
		if lines <= 0 {
			lines = 200
		}
		if lines > 2000 {
			lines = 2000
		}
		return map[string]string{"log": a.WorkerLog(in.Site, in.ID, lines)}, nil
	})

	// ── packages ────────────────────────────────────────────────────
	api("/api/project/packages", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		s, err := projectSite(a, in.Site)
		if err != nil {
			return nil, err
		}
		composerPkgs, npmPkgs, err := a.InstalledPackages(in.Site)
		if err != nil {
			return nil, err
		}
		if composerPkgs == nil {
			composerPkgs = []app.InstalledPkg{}
		}
		if npmPkgs == nil {
			npmPkgs = []app.InstalledPkg{}
		}
		return map[string]any{
			"composer":       composerPkgs,
			"npm":            npmPkgs,
			"hasComposer":    fileExistsAt(filepath.Join(s.Path, "composer.json")),
			"hasPackage":     fileExistsAt(filepath.Join(s.Path, "package.json")),
			"packageManager": detect.Project(s.Path).PackageManager,
		}, nil
	})
	api("/api/project/packages/search", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Manager string `json:"manager"`
			Q       string `json:"q"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		q := strings.TrimSpace(in.Q)
		if q == "" {
			return []pkgsearch.Package{}, nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		var res []pkgsearch.Package
		var err error
		switch in.Manager {
		case "composer":
			res, err = pkgsearch.SearchComposer(ctx, q, 20)
		case "npm":
			res, err = pkgsearch.SearchNpm(ctx, q, 20)
		default:
			return nil, fmt.Errorf("unknown package source %q (composer or npm)", in.Manager)
		}
		if res == nil {
			res = []pkgsearch.Package{}
		}
		return res, err
	})
	type pkgReq struct {
		Site    string `json:"site"`
		Manager string `json:"manager"`
		Name    string `json:"name"`
		Version string `json:"version"`
		Dev     bool   `json:"dev"`
	}
	packageJob := func(install bool) func(a *app.App, r *http.Request) (any, error) {
		return func(a *app.App, r *http.Request) (any, error) {
			var in pkgReq
			if err := decodeBody(r, &in); err != nil {
				return nil, err
			}
			if _, err := projectSite(a, in.Site); err != nil {
				return nil, err
			}
			if in.Manager != "composer" && in.Manager != "npm" {
				return nil, fmt.Errorf("unknown package manager %q (composer or npm)", in.Manager)
			}
			in.Name = strings.TrimSpace(in.Name)
			in.Version = strings.TrimSpace(in.Version)
			if in.Name == "" {
				return nil, errors.New("no package name")
			}
			if !claimPackageOp(in.Site) {
				return nil, fmt.Errorf("another package operation is still running for %s — wait for it to finish", in.Site)
			}
			id := startCancellable(func(ctx context.Context, setStatus func(string), appendLog func([]byte)) (any, error) {
				defer releasePackageOp(in.Site)
				if install {
					setStatus("Installing " + in.Name + "…")
					return nil, a.InstallPackage(ctx, in.Site, in.Manager, in.Name, in.Version, in.Dev, appendLog)
				}
				setStatus("Removing " + in.Name + "…")
				return nil, a.RemovePackage(ctx, in.Site, in.Manager, in.Name, appendLog)
			})
			return map[string]string{"job": id}, nil
		}
	}
	api("/api/project/packages/install", packageJob(true))
	api("/api/project/packages/remove", packageJob(false))

	// ── domains ─────────────────────────────────────────────────────
	api("/api/project/aliases", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site    string   `json:"site"`
			Aliases []string `json:"aliases"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if _, err := projectSite(a, in.Site); err != nil {
			return nil, err
		}
		if err := a.SetSiteAliases(in.Site, in.Aliases); err != nil {
			return nil, err
		}
		return projectInfo(a, *a.State.FindSite(in.Site)), nil
	})
	api("/api/project/devport/suggest", func(a *app.App, r *http.Request) (any, error) {
		var in siteReq
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		s, err := projectSite(a, in.Site)
		if err != nil {
			return nil, err
		}
		start := s.DevPort + 1
		if start <= 1 {
			start = 5173
		}
		return map[string]int{"port": a.SuggestFreePort(start)}, nil
	})
	api("/api/project/devport", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Site  string `json:"site"`
			Port  int    `json:"port"`
			Force bool   `json:"force"`
		}
		if err := decodeBody(r, &in); err != nil {
			return nil, err
		}
		if _, err := projectSite(a, in.Site); err != nil {
			return nil, err
		}
		id := jobs.start(func(setStatus func(string)) (any, error) {
			setStatus(fmt.Sprintf("Moving the dev server to port %d…", in.Port))
			hints, err := a.SetSiteDevPort(in.Site, in.Port, in.Force)
			var inUse *app.PortInUseError
			if errors.As(err, &inUse) {
				// Not a failure: the page offers to force it.
				return map[string]any{
					"conflict": true, "message": inUse.Error(),
					"pid": inUse.PID, "process": inUse.Name,
				}, nil
			}
			if err != nil {
				return nil, err
			}
			if hints == nil {
				hints = []string{}
			}
			return map[string]any{"port": in.Port, "hints": hints}, nil
		})
		return map[string]string{"job": id}, nil
	})
}

// ── git ─────────────────────────────────────────────────────────────

// gitOps allows one git write per site at a time (a commit racing a
// pull would trip over index.lock, or worse). Reads never wait.
var gitOps = struct {
	sync.Mutex
	busy map[string]string // site → what's running
}{busy: map[string]string{}}

func claimGitOp(site, what string) error {
	key := strings.ToLower(site)
	gitOps.Lock()
	defer gitOps.Unlock()
	if cur, ok := gitOps.busy[key]; ok {
		return fmt.Errorf("git is still busy with %s for %s — wait for it to finish", cur, site)
	}
	gitOps.busy[key] = what
	return nil
}

func releaseGitOp(site string) {
	gitOps.Lock()
	delete(gitOps.busy, strings.ToLower(site))
	gitOps.Unlock()
	app.ForgetGitSummary(site)
}

func gitBusy(site string) string {
	gitOps.Lock()
	defer gitOps.Unlock()
	return gitOps.busy[strings.ToLower(site)]
}

// gitJobResult turns a network operation's outcome into a job result:
// an authentication failure is a result the page explains (with an
// "open a terminal" button), not a bare error string.
func gitJobResult(err error) (any, error) {
	var ae *gitops.AuthError
	if errors.As(err, &ae) {
		return map[string]any{"authFailed": true, "message": ae.Error(), "output": ae.Output}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// registerProjectGit adds the Git tab's endpoints (/api/project/git/*).
// Reads run inline; local writes run inline but one at a time per site;
// network operations and rebases are streaming, cancellable jobs.
func registerProjectGit(api apiRegistrar) {
	type gitReq struct {
		Site             string   `json:"site"`
		Path             string   `json:"path"`
		Paths            []string `json:"paths"`
		All              bool     `json:"all"`
		Staged           bool     `json:"staged"`
		Hash             string   `json:"hash"`
		Message          string   `json:"message"`
		Amend            bool     `json:"amend"`
		Mode             string   `json:"mode"`
		SetUpstream      bool     `json:"setUpstream"`
		Force            bool     `json:"force"`
		Branch           string   `json:"branch"`
		Name             string   `json:"name"`
		Create           bool     `json:"create"`
		Onto             string   `json:"onto"`
		Limit            int      `json:"limit"`
		Action           string   `json:"action"`
		Index            int      `json:"index"`
		IncludeUntracked bool     `json:"includeUntracked"`
	}
	decode := func(a *app.App, r *http.Request) (gitReq, error) {
		var in gitReq
		if err := decodeBody(r, &in); err != nil {
			return in, err
		}
		_, err := projectSite(a, in.Site)
		return in, err
	}
	// read registers an inline read.
	read := func(path string, fn func(a *app.App, ctx context.Context, in gitReq) (any, error)) {
		api(path, func(a *app.App, r *http.Request) (any, error) {
			in, err := decode(a, r)
			if err != nil {
				return nil, err
			}
			return fn(a, r.Context(), in)
		})
	}
	// write registers an inline local write, serialized per site. It
	// runs detached from the request: a closed tab must not kill git
	// halfway through a commit (gitops has its own timeouts).
	write := func(path, what string, fn func(a *app.App, ctx context.Context, in gitReq) (any, error)) {
		api(path, func(a *app.App, r *http.Request) (any, error) {
			in, err := decode(a, r)
			if err != nil {
				return nil, err
			}
			if err := claimGitOp(in.Site, what); err != nil {
				return nil, err
			}
			defer releaseGitOp(in.Site)
			return fn(a, context.Background(), in)
		})
	}
	// job registers a streaming, cancellable operation (network ops,
	// rebase), holding the site's git lock until it finishes.
	job := func(path, what string, fn func(a *app.App, ctx context.Context, in gitReq, setStatus func(string), appendLog func([]byte)) (any, error)) {
		api(path, func(a *app.App, r *http.Request) (any, error) {
			in, err := decode(a, r)
			if err != nil {
				return nil, err
			}
			if !gitops.Available() {
				return nil, gitops.ErrNotInstalled
			}
			if err := claimGitOp(in.Site, what); err != nil {
				return nil, err
			}
			id := startCancellable(func(ctx context.Context, setStatus func(string), appendLog func([]byte)) (any, error) {
				defer releaseGitOp(in.Site)
				return fn(a, ctx, in, setStatus, appendLog)
			})
			return map[string]string{"job": id}, nil
		})
	}

	read("/api/project/git/status", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		if !gitops.Available() {
			return map[string]any{"noGit": true, "message": gitops.ErrNotInstalled.Error()}, nil
		}
		st, err := a.GitStatus(ctx, in.Site)
		if err != nil {
			return nil, err
		}
		return struct {
			gitops.RepoStatus
			Busy string `json:"busy"`
		}{st, gitBusy(in.Site)}, nil
	})
	read("/api/project/git/diff", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return a.GitDiff(ctx, in.Site, in.Path, in.Staged)
	})
	read("/api/project/git/show", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return a.GitShow(ctx, in.Site, in.Hash)
	})
	read("/api/project/git/branches", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		list, err := a.GitBranches(ctx, in.Site)
		if err != nil {
			return nil, err
		}
		return map[string]any{"branches": list}, nil
	})
	read("/api/project/git/log", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		list, err := a.GitLog(ctx, in.Site, in.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"commits": list}, nil
	})

	write("/api/project/git/stage", "staging", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitStage(ctx, in.Site, in.Paths, in.All)
	})
	write("/api/project/git/unstage", "unstaging", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitUnstage(ctx, in.Site, in.Paths, in.All)
	})
	write("/api/project/git/discard", "discarding changes", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitDiscard(ctx, in.Site, in.Paths)
	})
	write("/api/project/git/commit", "a commit", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		hash, err := a.GitCommit(ctx, in.Site, in.Message, in.Amend, in.All)
		if err != nil {
			return nil, err
		}
		short := hash
		if len(short) > 7 {
			short = short[:7]
		}
		return map[string]string{"hash": hash, "short": short}, nil
	})
	write("/api/project/git/checkout", "switching branches", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitCheckout(ctx, in.Site, in.Branch, in.Create)
	})
	write("/api/project/git/branch/create", "creating a branch", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitCheckout(ctx, in.Site, strings.TrimSpace(in.Name), true)
	})
	write("/api/project/git/branch/delete", "deleting a branch", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitDeleteBranch(ctx, in.Site, in.Name, in.Force)
	})
	for _, op := range []string{"rebase/continue", "rebase/skip", "rebase/abort", "merge/continue", "merge/abort", "cherry-pick/abort"} {
		name := strings.Replace(op, "/", "-", 1)
		write("/api/project/git/"+op, strings.Replace(op, "/", " --", 1), func(a *app.App, ctx context.Context, in gitReq) (any, error) {
			return nil, a.GitSequencer(ctx, in.Site, name)
		})
	}
	write("/api/project/git/init", "creating the repository", func(a *app.App, ctx context.Context, in gitReq) (any, error) {
		return nil, a.GitInit(ctx, in.Site)
	})
	// Stashes: list and show are reads; push/pop/apply/drop write.
	api("/api/project/git/stash", func(a *app.App, r *http.Request) (any, error) {
		in, err := decode(a, r)
		if err != nil {
			return nil, err
		}
		switch in.Action {
		case "", "list":
			list, err := a.GitStashList(r.Context(), in.Site)
			if err != nil {
				return nil, err
			}
			return map[string]any{"stashes": list}, nil
		case "show":
			return a.GitStashShow(r.Context(), in.Site, in.Index)
		case "push", "pop", "apply", "drop":
			if err := claimGitOp(in.Site, "a stash "+in.Action); err != nil {
				return nil, err
			}
			defer releaseGitOp(in.Site)
			return nil, a.GitStash(context.Background(), in.Site, in.Action, in.Message, in.IncludeUntracked, in.Index)
		}
		return nil, fmt.Errorf("unknown stash action %q", in.Action)
	})

	job("/api/project/git/fetch", "a fetch", func(a *app.App, ctx context.Context, in gitReq, setStatus func(string), appendLog func([]byte)) (any, error) {
		setStatus("Fetching…")
		return gitJobResult(a.GitFetch(ctx, in.Site, appendLog))
	})
	job("/api/project/git/pull", "a pull", func(a *app.App, ctx context.Context, in gitReq, setStatus func(string), appendLog func([]byte)) (any, error) {
		setStatus("Pulling…")
		return gitJobResult(a.GitPull(ctx, in.Site, in.Mode, appendLog))
	})
	job("/api/project/git/push", "a push", func(a *app.App, ctx context.Context, in gitReq, setStatus func(string), appendLog func([]byte)) (any, error) {
		setStatus("Pushing…")
		return gitJobResult(a.GitPush(ctx, in.Site, in.SetUpstream, in.Force, appendLog))
	})
	job("/api/project/git/rebase", "a rebase", func(a *app.App, ctx context.Context, in gitReq, setStatus func(string), appendLog func([]byte)) (any, error) {
		setStatus("Rebasing onto " + in.Onto + "…")
		return gitJobResult(a.GitRebase(ctx, in.Site, in.Onto, appendLog))
	})
}
