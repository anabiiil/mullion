package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/phpver"
)

// artisanListFixture is a trimmed `php artisan list --format=json`,
// preceded by the kind of boot noise some apps print on stdout.
const artisanListFixture = `PHP Deprecated:  Something in /app/vendor/x.php on line 3
{"application":{"name":"Laravel Framework","version":"11.9.2"},"commands":[
 {"name":"_complete","description":"Internal command to provide shell completion suggestions","hidden":true},
 {"name":"about","description":"Display basic information about your application","hidden":false,"usage":["about"]},
 {"name":"migrate","description":"Run the database migrations","hidden":false},
 {"name":"queue:work","description":"Start processing jobs on the queue as a daemon","hidden":false},
 {"name":"secret:hidden","description":"x","hidden":true}
],"namespaces":[{"id":"_global","commands":["about","migrate"]}]}
`

func TestParseConsoleList(t *testing.T) {
	cmds, err := parseConsoleList([]byte(artisanListFixture), "artisan", "php artisan")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "about,migrate,queue:work" {
		t.Fatalf("names = %v", names)
	}
	if c := cmds[1]; c.Group != "artisan" || c.Command != "php artisan migrate" || c.Description != "Run the database migrations" {
		t.Errorf("migrate = %+v", c)
	}
	if _, err := parseConsoleList([]byte("Fatal error: boom"), "artisan", "php artisan"); err == nil {
		t.Error("garbage output accepted")
	}
}

func TestProjectCommandsWithFakePHP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake php is a shell script")
	}
	a := workersSandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	phpDir := a.Paths.PhpVersionDir("8.4.1")
	_ = os.MkdirAll(phpDir, 0o755)
	fixture := filepath.Join(t.TempDir(), "list.json")
	_ = os.WriteFile(fixture, []byte(artisanListFixture), 0o644)
	calls := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho x >> '" + calls + "'\n[ \"$1 $2 $3\" = \"artisan list --format=json\" ] || exit 9\ncat '" + fixture + "'\n"
	if err := os.WriteFile(filepath.Join(phpDir, phpver.PhpExeName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	proj := t.TempDir()
	_ = os.WriteFile(filepath.Join(proj, "artisan"), []byte("<?php"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "composer.json"), []byte(`{"scripts":{
		"post-autoload-dump":["@php artisan package:discover"],
		"test":"phpunit",
		"dev":["Composer\\Config::disableProcessTimeout","npx concurrently x"]}}`), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "package.json"), []byte(`{"scripts":{"build":"vite build","dev":"vite"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "yarn.lock"), nil, 0o644)
	a.State.AddSite(config.Site{Name: "shop", Path: proj})

	cmds, err := a.ProjectCommands(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cmds {
		got = append(got, c.Group+":"+c.Command)
	}
	want := []string{
		"artisan:php artisan about", "artisan:php artisan migrate", "artisan:php artisan queue:work",
		"npm:yarn run build", "npm:yarn run dev",
		"composer:composer run-script dev", "composer:composer run-script test",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n got %v\nwant %v", got, want)
	}

	// Cached: a second listing does not boot the framework again.
	if _, err := a.ProjectCommands(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(calls); strings.Count(string(data), "x") != 1 {
		t.Errorf("php ran %d times, want 1 (cache)", strings.Count(string(data), "x"))
	}
	// composer.lock appearing invalidates the cache.
	_ = os.WriteFile(filepath.Join(proj, "composer.lock"), []byte(`{}`), 0o644)
	if _, err := a.ProjectCommands(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(calls); strings.Count(string(data), "x") != 2 {
		t.Errorf("cache not invalidated by composer.lock")
	}

	// A broken artisan: the other groups still come back, with an error.
	_ = os.WriteFile(filepath.Join(phpDir, phpver.PhpExeName), []byte("#!/bin/sh\necho 'boom' >&2; exit 1\n"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "composer.lock"), []byte(`{"x":1}`), 0o644)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(proj, "composer.lock"), future, future)
	cmds, err = a.ProjectCommands(context.Background(), "shop")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the artisan failure", err)
	}
	if len(cmds) != 4 {
		t.Errorf("partial result = %d commands, want the 4 script commands", len(cmds))
	}
}

func TestRunProjectCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh syntax")
	}
	a := workersSandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	proj := t.TempDir()
	a.State.AddSite(config.Site{Name: "shop", Path: proj})

	var mu sync.Mutex
	var out strings.Builder
	code, err := a.RunProjectCommand(context.Background(), "shop",
		`echo "dir=$(pwd)"; echo "path=$PATH"; echo oops >&2; exit 3`,
		func(b []byte) { mu.Lock(); out.Write(b); mu.Unlock() })
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	s := out.String()
	if !strings.Contains(s, filepath.Base(proj)) || !strings.Contains(s, "oops") ||
		!strings.Contains(s, "path="+a.Paths.PhpVersionDir("8.4.1")) {
		t.Errorf("output:\n%s", s)
	}

	// Cancelling kills the whole tree promptly.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	code, err = a.RunProjectCommand(ctx, "shop", "sleep 30 & sleep 30; wait", nil)
	if err == nil || code != -1 {
		t.Fatalf("cancelled run: code=%d err=%v", code, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancel took %v", time.Since(start))
	}
}
