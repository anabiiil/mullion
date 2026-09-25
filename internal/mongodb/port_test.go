package mongodb

import (
	"os"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestNonDefaultPort(t *testing.T) {
	old := Port
	Port = 27018
	t.Cleanup(func() { Port = old })
	paths := pmdir.Paths{Home: t.TempDir()}
	if conf := renderConf(paths); !strings.Contains(conf, "  port: 27018\n") || strings.Contains(conf, "27017") {
		t.Errorf("port 27018 not rendered:\n%s", conf)
	}
	if got := ConnectionURI(); got != "mongodb://127.0.0.1:27018" {
		t.Errorf("ConnectionURI = %q", got)
	}
	args := strings.Join(shellArgs("1"), " ")
	if !strings.Contains(args, "--port 27018") {
		t.Errorf("shell args = %s", args)
	}

	// WriteConfig: no-op before MongoDB was ever set up, rewrites after.
	if err := WriteConfig(paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(confFile(paths)); err == nil {
		t.Fatal("wrote mongod.conf although MongoDB was never set up")
	}
	os.MkdirAll(baseDir(paths), 0o755)
	if err := WriteConfig(paths); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(confFile(paths))
	if !strings.Contains(string(b), "port: 27018") {
		t.Errorf("mongod.conf =\n%s", b)
	}
}
