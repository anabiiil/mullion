package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/pmdir"
)

// workersSandboxApp builds an App over a throwaway Home (never the real
// ~/.mullion; skipApply keeps Apply away from hosts/Caddy).
func workersSandboxApp(t *testing.T) *App {
	t.Helper()
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Paths: paths, State: state, skipApply: true}
}

func envValue(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == key {
			val, found = v, true
		}
	}
	return val, found
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	env := []string{"HOME=/h", "PATH=/usr/bin" + sep + "/a" + sep + "/bin", "X=1"}
	got := prependPath(env, []string{"/a", "/b"})
	path, _ := envValue(got, "PATH")
	if want := strings.Join([]string{"/a", "/b", "/usr/bin", "/bin"}, sep); path != want {
		t.Errorf("PATH = %q, want %q", path, want)
	}
	if len(got) != 3 || got[0] != "HOME=/h" || got[2] != "X=1" {
		t.Errorf("other vars disturbed: %v", got)
	}
	// No PATH at all: one is added.
	got = prependPath([]string{"A=1"}, []string{"/x"})
	if path, ok := envValue(got, "PATH"); !ok || path != "/x" {
		t.Errorf("PATH = %q (%v), want /x", path, ok)
	}
}

func TestSiteEnvPathComposition(t *testing.T) {
	a := workersSandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	// A fake installed Node the site resolves to.
	nodeDir := a.Paths.NodeVersionDir("22.3.0")
	if err := os.MkdirAll(nodever.BinDir(nodeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(nodever.NodeBin(nodeDir), []byte("x"), 0o755)

	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "package.json"), []byte(`{}`), 0o644)
	site := config.Site{Name: "shop", Path: proj, Kind: "php", PHP: "8.3.9"}

	env := a.SiteEnv(site)
	path, _ := envValue(env, "PATH")
	parts := filepath.SplitList(path)
	want := []string{a.Paths.PhpVersionDir("8.3.9"), nodever.BinDir(nodeDir), a.Paths.BinDir()}
	if len(parts) < 3 {
		t.Fatalf("PATH too short: %q", path)
	}
	for i, w := range want {
		if parts[i] != w {
			t.Errorf("PATH[%d] = %q, want %q (full %q)", i, parts[i], w, path)
		}
	}
	if phprc, _ := envValue(env, "PHPRC"); phprc != a.Paths.PhpVersionDir("8.3.9") {
		t.Errorf("PHPRC = %q, want the site's PHP dir", phprc)
	}

	// No package.json → no node dir; unpinned → global PHP.
	plain := config.Site{Name: "blog", Path: t.TempDir()}
	path, _ = envValue(a.SiteEnv(plain), "PATH")
	parts = filepath.SplitList(path)
	if parts[0] != a.Paths.PhpVersionDir("8.4.1") || parts[1] != a.Paths.BinDir() {
		t.Errorf("PATH for a plain PHP site = %q", path)
	}
	if strings.Contains(path, nodever.BinDir(nodeDir)) {
		t.Errorf("node dir on PATH of a site without package.json: %q", path)
	}
}

func TestWorkerCRUD(t *testing.T) {
	a := workersSandboxApp(t)
	a.State.AddSite(config.Site{Name: "shop", Path: t.TempDir()})

	if _, err := a.AddWorker("shop", config.Worker{Name: "Queue"}); err == nil {
		t.Fatal("worker without a command accepted")
	}
	if _, err := a.AddWorker("nope", config.Worker{Command: "x"}); err == nil {
		t.Fatal("unknown site accepted")
	}
	if _, err := a.AddWorker("shop", config.Worker{Command: "x", Kind: "weird"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	w, err := a.AddWorker("shop", config.Worker{Name: "Queue: default", Kind: "queue", Command: "  php artisan queue:work  "})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.ID, "queue-default-") || len(w.ID) != len("queue-default-")+4 {
		t.Errorf("id = %q, want queue-default-<4 hex>", w.ID)
	}
	if w.Command != "php artisan queue:work" {
		t.Errorf("command not trimmed: %q", w.Command)
	}
	w2, _ := a.AddWorker("shop", config.Worker{Name: "Queue: default", Command: "y"})
	if w2.ID == w.ID || w2.Kind != "custom" {
		t.Errorf("second worker: %+v", w2)
	}

	// Persisted.
	reloaded, err := config.Load(a.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.FindSite("shop").Workers; len(got) != 2 || got[0].ID != w.ID {
		t.Fatalf("saved workers = %+v", got)
	}

	w.Command = "php artisan queue:work --queue=high"
	w.AutoStart = true
	if err := a.UpdateWorker("shop", w); err != nil {
		t.Fatal(err)
	}
	views := a.WorkersStatus("shop")
	if len(views) != 2 || views[0].Command != w.Command || !views[0].AutoStart || views[0].Status.State != "stopped" {
		t.Fatalf("views = %+v", views)
	}
	if err := a.RemoveWorker("shop", w2.ID); err != nil {
		t.Fatal(err)
	}
	if len(a.State.FindSite("shop").Workers) != 1 {
		t.Fatal("worker not removed")
	}
	if err := a.RemoveWorker("shop", w2.ID); err == nil {
		t.Fatal("removing an unknown worker succeeded")
	}
}

func TestWorkerTemplatesLaravel(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "composer.json"), []byte(`{
		"require": {"php": "^8.2", "laravel/framework": "^11.0", "laravel/horizon": "^5.0"}
	}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "composer.lock"), []byte(`{"packages":[{"name":"laravel/framework","version":"v11.9.2"}]}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"dev":"vite","build":"vite build","queue:listen":"node q.js","prestart":"x"}}`), 0o644)

	got := WorkerTemplates("laravel", dir)
	byName := map[string]config.Worker{}
	for _, w := range got {
		byName[w.Name] = w
	}
	if w := byName["Queue worker"]; w.Command != "php artisan queue:work --tries=3 --sleep=1" || w.Kind != "queue" {
		t.Errorf("queue template = %+v", w)
	}
	if w := byName["Scheduler"]; w.Command != "php artisan schedule:work" || w.Kind != "scheduler" {
		t.Errorf("scheduler template = %+v", w)
	}
	if _, ok := byName["Horizon"]; !ok {
		t.Error("horizon missing though laravel/horizon is required")
	}
	if _, ok := byName["Reverb"]; ok {
		t.Error("reverb suggested without laravel/reverb")
	}
	if w, ok := byName["npm run queue:listen"]; !ok || w.Kind != "queue" {
		t.Errorf("npm queue script suggestion = %+v (%v)", w, ok)
	}
	for _, w := range got {
		if strings.Contains(w.Command, "run dev") || strings.Contains(w.Command, "run build") || strings.Contains(w.Command, "prestart") {
			t.Errorf("unexpected suggestion %q", w.Command)
		}
	}

	// Laravel 7: no schedule:work → a schedule:run loop.
	_ = os.WriteFile(filepath.Join(dir, "composer.lock"), []byte(`{"packages":[{"name":"laravel/framework","version":"v7.30.6"}]}`), 0o644)
	for _, w := range WorkerTemplates("laravel", dir) {
		if w.Name == "Scheduler" && !strings.Contains(w.Command, "schedule:run") {
			t.Errorf("laravel 7 scheduler = %q", w.Command)
		}
	}
}

func TestWorkerTemplatesSymfonyAndNode(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "composer.json"), []byte(`{"require":{"symfony/messenger":"^7"}}`), 0o644)
	got := WorkerTemplates("symfony", dir)
	if len(got) != 1 || got[0].Command != "php bin/console messenger:consume async -vv" {
		t.Errorf("symfony templates = %+v", got)
	}

	node := t.TempDir()
	_ = os.WriteFile(filepath.Join(node, "package.json"), []byte(`{"scripts":{"start":"node server.js","worker":"node w.js","cron":"node c.js","lint":"eslint ."}}`), 0o644)
	_ = os.WriteFile(filepath.Join(node, "pnpm-lock.yaml"), nil, 0o644)
	got = WorkerTemplates("express", node)
	var cmds []string
	for _, w := range got {
		cmds = append(cmds, w.Command)
	}
	if strings.Join(cmds, ",") != "pnpm run cron,pnpm run start,pnpm run worker" {
		t.Errorf("node suggestions = %v", cmds)
	}
}
