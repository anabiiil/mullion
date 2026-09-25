package heidisql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/mysql"
	"pm/internal/pmdir"
)

func TestReplacePort(t *testing.T) {
	in := "Servers\\Mullion\\Host\t2\t127.0.0.1\r\nServers\\Mullion\\Port\t0\t3306\r\nServers\\Other\\Port\t0\t3306\r\n"
	out, changed := replacePort(in, 3307)
	if !changed {
		t.Fatal("not changed")
	}
	want := "Servers\\Mullion\\Host\t2\t127.0.0.1\r\nServers\\Mullion\\Port\t0\t3307\r\nServers\\Other\\Port\t0\t3306\r\n"
	if out != want {
		t.Errorf("got %q", out)
	}
	if _, changed := replacePort(out, 3307); changed {
		t.Error("second replace should be a no-op")
	}
}

func TestSessionFollowsPort(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	old := mysql.Port
	t.Cleanup(func() { mysql.Port = old })
	// Not installed: nothing to do.
	if err := SyncPort(paths); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(Dir(paths), 0o755)
	mysql.Port = 3306
	if err := writeSession(paths); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(Dir(paths), "portable_settings.txt")
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), "Servers\\Mullion\\Port\t0\t3306\r\n") {
		t.Fatalf("seeded settings:\n%q", b)
	}
	mysql.Port = 3317
	if err := SyncPort(paths); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(settings)
	if !strings.Contains(string(b), "Servers\\Mullion\\Port\t0\t3317\r\n") || !strings.Contains(string(b), "Host\t2\t127.0.0.1") {
		t.Errorf("after SyncPort:\n%q", b)
	}
}
