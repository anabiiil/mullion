package pgadmin

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pm/internal/postgres"
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
	if found, port, err := hasServer(b); err != nil || !found || port != 5432 {
		t.Errorf("hasServer(rendered) = %v, %d, %v", found, port, err)
	}

	b, _ = renderServers("")
	if strings.Contains(string(b), "passfile") {
		t.Error("no passfile requested, but one was rendered")
	}
}

func TestHasServer(t *testing.T) {
	dump := `{"Servers": {"1": {"Name": "Local", "Group": "Servers", "Port": 5432}, "7": {"Name": "Mullion (PostgreSQL)", "Group": "Mine", "Port": 5433}}}`
	if found, port, err := hasServer([]byte(dump)); err != nil || !found || port != 5433 {
		t.Errorf("found = %v, %d, %v", found, port, err)
	}
	if found, _, err := hasServer([]byte(`{"Servers": {}}`)); err != nil || found {
		t.Errorf("empty: found = %v, %v", found, err)
	}
	if _, _, err := hasServer([]byte("not json")); err == nil {
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

// withPort runs fn with postgres.Port moved, restoring it afterwards.
func withPort(t *testing.T, port int) {
	t.Helper()
	old := postgres.Port
	postgres.Port = port
	t.Cleanup(func() { postgres.Port = old })
}

func TestNonDefaultPort(t *testing.T) {
	withPort(t, 5433)
	b, err := renderServers("/p")
	if err != nil {
		t.Fatal(err)
	}
	if found, port, err := hasServer(b); err != nil || !found || port != 5433 {
		t.Errorf("servers.json port = %d (%v, %v), want 5433", port, found, err)
	}
	if got := renderPassFile("x"); got != "127.0.0.1:5433:*:postgres:x\n" {
		t.Errorf("pgpass = %q", got)
	}
}

func TestMarker(t *testing.T) {
	withPort(t, 5440)
	db, port := parseMarker([]byte(renderMarker("/h/.pgadmin/pgadmin4.db", 5440)))
	if db != "/h/.pgadmin/pgadmin4.db" || port != 5440 {
		t.Errorf("round trip = %q, %d", db, port)
	}
	// A marker from before ports were configurable: db only, default port.
	db, port = parseMarker([]byte("C:\\Users\\me\\AppData\\Roaming\\pgAdmin\\pgadmin4.db\r\n"))
	if db != `C:\Users\me\AppData\Roaming\pgAdmin\pgadmin4.db` || port != 5432 {
		t.Errorf("legacy = %q, %d", db, port)
	}
}

// TestUpdatePortScript runs the SQL port update against a scratch
// SQLite database shaped like pgAdmin's (server + "user" tables) with
// any python3 on PATH — it only needs the stdlib sqlite3 module.
func TestUpdatePortScript(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	db := filepath.Join(t.TempDir(), "pgadmin4.db")
	setup := `import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.executescript("""
CREATE TABLE "user" (id INTEGER PRIMARY KEY, email TEXT);
CREATE TABLE server (id INTEGER PRIMARY KEY, user_id INTEGER, name TEXT, host TEXT, port INTEGER);
INSERT INTO "user" VALUES (1, 'pgadmin4@pgadmin.org'), (2, 'other@x');
INSERT INTO server VALUES (1, 1, 'Mullion (PostgreSQL)', '127.0.0.1', 5432);
INSERT INTO server VALUES (2, 1, 'Prod', 'db.example.com', 5432);
INSERT INTO server VALUES (3, 2, 'Mullion (PostgreSQL)', '127.0.0.1', 5432);
""")
con.commit()`
	if out, err := exec.Command(py, "-c", setup, db).CombinedOutput(); err != nil {
		t.Fatalf("setup: %v: %s", err, out)
	}
	out, err := exec.Command(py, "-s", "-c", updatePortScript, db, ServerName, desktopUser, "5433").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "updated 1") {
		t.Fatalf("update: %v: %s", err, out)
	}
	got, err := exec.Command(py, "-c", `import sqlite3, sys
print(",".join(str(r[0]) for r in sqlite3.connect(sys.argv[1]).execute("SELECT port FROM server ORDER BY id")))`, db).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "5433,5432,5432" {
		t.Errorf("ports after update = %s (only the desktop user's Mullion entry may move)", got)
	}
}
