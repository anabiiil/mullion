package fcgi

import (
	"os"
	"path/filepath"
	"testing"

	"pm/internal/pmdir"
	"pm/internal/watch"
)

func TestWatchScanDir(t *testing.T) {
	sep := string(os.PathListSeparator)
	dir := filepath.Join("h", "watch", "ini")
	// Unset: a leading separator keeps PHP's compiled-in scan dir.
	if got := watchScanDir("", dir); got != sep+dir {
		t.Fatalf("unset = %q", got)
	}
	if got := watchScanDir("conf.d", dir); got != "conf.d"+sep+dir {
		t.Fatalf("existing = %q", got)
	}
	if got := watchScanDir("conf.d"+sep+dir, dir); got != "conf.d"+sep+dir {
		t.Fatalf("already there must not repeat: %q", got)
	}
	env := watchEnv([]string{"Path=x", "php_ini_scan_dir=conf.d", "A=1"}, dir)
	n := 0
	for _, kv := range env {
		if len(kv) > 16 && (kv[:16] == "PHP_INI_SCAN_DIR" || kv[:16] == "php_ini_scan_dir") {
			n++
			if kv != "PHP_INI_SCAN_DIR=conf.d"+sep+dir {
				t.Fatalf("env entry = %q", kv)
			}
		}
	}
	if n != 1 || len(env) != 3 {
		t.Fatalf("env = %q", env)
	}
}

func TestWebWatchMarker(t *testing.T) {
	p := pmdir.Paths{Home: t.TempDir()}
	if webWatchWanted(p) {
		t.Fatal("off by default")
	}
	if err := SetWebWatch(p, true); err != nil {
		t.Fatal(err)
	}
	if webWatchWanted(p) {
		t.Fatal("the marker alone must not count while the ini is missing")
	}
	if err := watch.EnsureFiles(p); err != nil {
		t.Fatal(err)
	}
	if !webWatchWanted(p) {
		t.Fatal("marker + ini = wanted")
	}
	if err := SetWebWatch(p, false); err != nil {
		t.Fatal(err)
	}
	if webWatchWanted(p) || SetWebWatch(p, false) != nil {
		t.Fatal("off again (and idempotent)")
	}
	// Where workers don't use the scan dir (php-fpm), nothing is
	// recorded and nothing restarts.
	if !workerUsesScanDir {
		recordWatchState(p, "8.4.1", true)
		if _, err := os.Stat(watchStateFile(p, "8.4.1")); err == nil {
			t.Fatal("php-fpm workers must not record a watch state")
		}
		RestartForWatch(p, []string{"8.4.1"})
	}
}
