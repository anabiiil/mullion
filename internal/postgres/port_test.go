package postgres

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestRenderConfPort(t *testing.T) {
	old := Port
	Port = 5433
	t.Cleanup(func() { Port = old })
	conf := renderConf("darwin")
	if !strings.Contains(conf, "port = 5433\n") || strings.Contains(conf, "5432") {
		t.Errorf("port 5433 not rendered:\n%s", conf)
	}
}

// WriteConfig rewrites mullion.conf of an initialized cluster only.
func TestWriteConfigFollowsPort(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	old := Port
	t.Cleanup(func() { Port = old })
	Port = 5440
	if err := WriteConfig(paths, "17.2"); err != nil {
		t.Fatal(err)
	}
	data := DataDir(paths, "17.2")
	if _, err := os.Stat(filepath.Join(data, confName)); err == nil {
		t.Fatal("wrote a config for a cluster that doesn't exist")
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "PG_VERSION"), []byte("17\n"), 0o644)
	os.WriteFile(filepath.Join(data, "postgresql.conf"), []byte("# defaults\n"), 0o644)
	if !DataInitialized(paths, "17.2") {
		t.Skip("fixture does not look initialized to DataInitialized")
	}
	if err := WriteConfig(paths, "17.2"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(data, confName))
	if !strings.Contains(string(b), "port = 5440") {
		t.Errorf("mullion.conf =\n%s", b)
	}
	main, _ := os.ReadFile(filepath.Join(data, "postgresql.conf"))
	if strings.Count(string(main), "include_if_exists") != 1 {
		t.Errorf("postgresql.conf =\n%s", main)
	}
	// Idempotent: a second write doesn't add a second include.
	if err := WriteConfig(paths, "17.2"); err != nil {
		t.Fatal(err)
	}
	main, _ = os.ReadFile(filepath.Join(data, "postgresql.conf"))
	if strings.Count(string(main), "include_if_exists") != 1 {
		t.Errorf("second write duplicated the include:\n%s", main)
	}
}
