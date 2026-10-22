//go:build darwin

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

// TestUpdatePHPEndToEnd really downloads and installs two patches of
// one branch into a throwaway HOME and updates one to the other: the
// older patch (with LDAP and pdo_dblib built for it) is customised,
// then UpdatePHP installs the newest patch — rebuilding both modules
// for it — and must carry php.ini over, switch the global version and a
// pinned site, and remove the old patch. skipApply keeps it away from
// /etc/hosts, Caddy and php-fpm (checked: /etc/hosts must not change).
//
// Skipped unless MULLION_PHP_UPDATE_E2E=1 (downloads ~150 MB and
// compiles for several minutes; needs the Xcode Command Line Tools).
// MULLION_PHP_UPDATE_E2E_HOME picks the sandbox (default: a temp dir),
// MULLION_PHP_UPDATE_E2E_FROM the older patch (default 8.4.22).
func TestUpdatePHPEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_PHP_UPDATE_E2E") != "1" {
		t.Skip("set MULLION_PHP_UPDATE_E2E=1 to run (real downloads and builds)")
	}
	hostsTime, hostsSum := hostsSnapshot(t)
	home := os.Getenv("MULLION_PHP_UPDATE_E2E_HOME")
	if home == "" {
		home = t.TempDir()
	}
	from := os.Getenv("MULLION_PHP_UPDATE_E2E_FROM")
	if from == "" {
		from = "8.4.22"
	}
	paths := pmdir.Paths{Home: filepath.Join(home, ".mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	sel, _ := phpver.ParseSelector(from)
	rel, err := phpver.Resolve(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := phpver.Install(ctx, paths, rel); err != nil {
		t.Fatal(err)
	}
	t.Logf("installed PHP %s in %s", from, time.Since(start).Round(time.Second))
	oldDir := paths.PhpVersionDir(from)
	for _, so := range []string{"ldap.so", "pdo_dblib.so"} {
		if _, err := os.Stat(filepath.Join(oldDir, "ext", so)); err != nil {
			t.Fatalf("the old patch has no %s to migrate (build failed?): %v", so, err)
		}
	}
	// Customise the old version: limits, OPcache, Mail's line.
	if err := phpver.SetIni(paths, from, "memory_limit", "1G"); err != nil {
		t.Fatal(err)
	}
	jit := "tracing"
	if err := phpver.SetOpcache(paths, from, phpver.OpcacheChange{JIT: &jit}); err != nil {
		t.Fatal(err)
	}
	ini := phpver.IniPath(paths, from)
	if err := phpver.SetIniDirective(ini, "sendmail_path", `"/usr/bin/true"`); err != nil {
		t.Fatal(err)
	}

	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Paths: paths, State: state, skipApply: true}
	if err := a.UseGlobal(from); err != nil {
		t.Fatal(err)
	}
	a.State.Sites = []config.Site{{Name: "pinned", Path: home, PHP: from}, {Name: "follows", Path: home}}
	a.State.Config.MailIniPrev = map[string]map[string]string{from: {}}
	if err := a.State.Save(); err != nil {
		t.Fatal(err)
	}

	start = time.Now()
	res, err := a.UpdatePHP(ctx, phpver.Branch(from), PHPUpdateOptions{Log: func(s string) { t.Log(s) }})
	if err != nil {
		t.Fatal(err)
	}
	to := res.To
	t.Logf("updated %s -> %s in %s; migration %+v; warnings %v", from, to, time.Since(start).Round(time.Second), res.Migration, res.Warnings)

	newDir := paths.PhpVersionDir(to)
	data, err := os.ReadFile(filepath.Join(newDir, "php.ini"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"memory_limit=1G", "opcache.jit=tracing", `sendmail_path="/usr/bin/true"`,
		`extension="` + filepath.Join(newDir, "ext", "ldap.so") + `"`,
		`extension="` + filepath.Join(newDir, "ext", "pdo_dblib.so") + `"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("new php.ini lacks %q", want)
		}
	}
	if strings.Contains(text, oldDir+"/") {
		t.Errorf("new php.ini still points into %s:\n%s", oldDir, text)
	}
	php := filepath.Join(newDir, phpver.PhpExeName)
	out, err := exec.Command(php, "-c", filepath.Join(newDir, "php.ini"), "-m").CombinedOutput()
	if err != nil || strings.Contains(string(out), "Warning") {
		t.Fatalf("new php -m: %v\n%s", err, out)
	}
	for _, mod := range []string{"ldap", "pdo_dblib"} {
		if !strings.Contains("\n"+string(out)+"\n", "\n"+mod+"\n") {
			t.Errorf("php -m of %s lacks %s", to, mod)
		}
	}
	out, _ = exec.Command(php, "-c", filepath.Join(newDir, "php.ini"), "-r", `echo ini_get("memory_limit"), " ", ini_get("opcache.jit");`).CombinedOutput()
	t.Logf("PHP %s reports: %s", to, out)

	if a.State.Config.GlobalPHP != to || a.State.FindSite("pinned").PHP != to || a.State.FindSite("follows").PHP != "" {
		t.Errorf("switch: global %q pinned %q follows %q", a.State.Config.GlobalPHP, a.State.FindSite("pinned").PHP, a.State.FindSite("follows").PHP)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("old patch still installed (err=%v)", err)
	}
	if _, ok := a.State.Config.MailIniPrev[to]; !ok {
		t.Errorf("MailIniPrev not moved: %+v", a.State.Config.MailIniPrev)
	}
	if mt, sum := hostsSnapshot(t); !mt.Equal(hostsTime) || sum != hostsSum {
		t.Fatal("/etc/hosts changed during the test")
	}
}
