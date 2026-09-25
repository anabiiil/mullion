package pgadmin

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLayoutFor(t *testing.T) {
	mac := layoutFor("darwin", "/m/pgadmin")
	if mac.bundle != filepath.Join("/m/pgadmin", "pgAdmin 4.app") {
		t.Errorf("mac bundle = %q", mac.bundle)
	}
	if !strings.HasSuffix(mac.exe, filepath.Join("Contents", "MacOS", "pgAdmin 4")) {
		t.Errorf("mac exe = %q", mac.exe)
	}
	if !strings.HasSuffix(mac.python, filepath.Join("Python.framework", "Versions", "Current", "bin", "python3")) {
		t.Errorf("mac python = %q", mac.python)
	}
	if mac.web != filepath.Join(mac.bundle, "Contents", "Resources", "web") {
		t.Errorf("mac web = %q", mac.web)
	}

	win := layoutFor("windows", "/m/pgadmin")
	root := filepath.Join("/m/pgadmin", "pgAdmin 4")
	if win.bundle != root ||
		win.exe != filepath.Join(root, "runtime", "pgAdmin4.exe") ||
		win.python != filepath.Join(root, "python", "python.exe") ||
		win.web != filepath.Join(root, "web") {
		t.Errorf("windows layout = %+v", win)
	}
}

func TestDataDirFor(t *testing.T) {
	if got := dataDirFor("darwin", "/Users/me", ""); got != filepath.Join("/Users/me", ".pgadmin") {
		t.Errorf("darwin = %q", got)
	}
	if got := dataDirFor("windows", `C:\Users\me`, `C:\Users\me\AppData\Roaming`); got != filepath.Join(`C:\Users\me\AppData\Roaming`, "pgAdmin") {
		t.Errorf("windows = %q", got)
	}
}

func TestRenderServers(t *testing.T) {
	b, err := renderServers("/m/pgadmin/pgpass")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]map[string]any
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	s, ok := doc.Servers["1"]
	if !ok || len(doc.Servers) != 1 {
		t.Fatalf("servers = %v", doc.Servers)
	}
	want := map[string]any{
		"Name": "Mullion (PostgreSQL)", "Group": "Mullion", "Host": "127.0.0.1",
		"Port": float64(5432), "MaintenanceDB": "postgres", "Username": "postgres",
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%s = %v, want %v", k, s[k], v)
		}
	}
	params, _ := s["ConnectionParameters"].(map[string]any)
	if params["sslmode"] != "prefer" || params["passfile"] != "/m/pgadmin/pgpass" {
		t.Errorf("ConnectionParameters = %v", params)
	}
	if _, has := s["Password"]; has {
		t.Error("servers.json must not carry a password")
	}
	// The dump format load-servers round-trips must be recognized.
	if found, err := hasServer(b); err != nil || !found {
		t.Errorf("hasServer(rendered) = %v, %v", found, err)
	}

	b, _ = renderServers("")
	if strings.Contains(string(b), "passfile") {
		t.Error("no passfile requested, but one was rendered")
	}
}

func TestHasServer(t *testing.T) {
	dump := `{"Servers": {"1": {"Name": "Local", "Group": "Servers"}, "7": {"Name": "Mullion (PostgreSQL)", "Group": "Mine"}}}`
	if found, err := hasServer([]byte(dump)); err != nil || !found {
		t.Errorf("found = %v, %v", found, err)
	}
	if found, err := hasServer([]byte(`{"Servers": {}}`)); err != nil || found {
		t.Errorf("empty: found = %v, %v", found, err)
	}
	if _, err := hasServer([]byte("not json")); err == nil {
		t.Error("garbage should fail")
	}
}

func TestRenderPassFile(t *testing.T) {
	if got := renderPassFile(""); got != "127.0.0.1:5432:*:postgres:\n" {
		t.Errorf("empty = %q", got)
	}
	if got := renderPassFile(`a:b\c`); got != `127.0.0.1:5432:*:postgres:a\:b\\c`+"\n" {
		t.Errorf("escaped = %q", got)
	}
}

func TestParseVersion(t *testing.T) {
	src := "# comment\nAPP_RELEASE = 9\nAPP_REVISION = 17\nAPP_SUFFIX = ''\nAPP_VERSION_INT = 91700\n"
	if got := parseVersion(src); got != "9.17" {
		t.Errorf("got %q", got)
	}
	if got := parseVersion("APP_RELEASE = 9\n"); got != "" {
		t.Errorf("incomplete: got %q", got)
	}
}

func TestSetupEnv(t *testing.T) {
	env := setupEnv([]string{"PATH=/bin", "PYTHONHOME=/x", "PythonPath=/y", "HOME=/h"}, "/c")
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTHONHOME=") || strings.HasPrefix(kv, "PythonPath=") {
			t.Errorf("leaked %q", kv)
		}
	}
	for _, kv := range []string{"PATH=/bin", "HOME=/h", "PYTHONPYCACHEPREFIX=/c"} {
		if !slices.Contains(env, kv) {
			t.Errorf("missing %q in %v", kv, env)
		}
	}
}

func TestAddedRe(t *testing.T) {
	m := addedRe.FindStringSubmatch("----------\nAdded 1 Server Group(s) and 1 Server(s).\n")
	if m == nil || m[1] != "1" {
		t.Errorf("match = %v", m)
	}
}
