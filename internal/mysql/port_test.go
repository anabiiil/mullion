package mysql

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestRenderIniPort(t *testing.T) {
	paths := pmdir.Paths{Home: "/m"}
	if ini := renderIni(paths, "8.4.5", "darwin"); !strings.Contains(ini, "\nport=3306\n") {
		t.Errorf("default port missing:\n%s", ini)
	}
	old := Port
	Port = 3307
	t.Cleanup(func() { Port = old })
	ini := renderIni(paths, "8.4.5", "windows")
	if !strings.Contains(ini, "\r\nport=3307\r\n") || strings.Contains(ini, "3306") {
		t.Errorf("port 3307 not rendered:\n%s", ini)
	}
	if strings.Contains(ini, "socket=") {
		t.Error("no socket line on Windows")
	}
}

// WriteConfig rewrites my.ini with the current port, and skips versions
// that aren't installed.
func TestWriteConfigFollowsPort(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	old := Port
	t.Cleanup(func() { Port = old })

	if err := WriteConfig(paths, "8.4.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.MysqlIni()); err == nil {
		t.Fatal("wrote my.ini for a version that isn't installed")
	}
	if err := os.MkdirAll(filepath.Join(paths.MysqlVersionDir("8.4.5"), "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{3306, 3310} {
		Port = port
		if err := WriteConfig(paths, "8.4.5"); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(paths.MysqlIni())
		if !strings.Contains(string(b), "port="+strconv.Itoa(port)+"\n") {
			t.Errorf("port %d: my.ini =\n%s", port, b)
		}
	}
}
