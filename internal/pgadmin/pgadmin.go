// Package pgadmin installs pgAdmin 4 — the desktop admin tool for the
// local PostgreSQL — into ~/.mullion/pgadmin and opens it with a
// "Mullion (PostgreSQL)" server already registered.
//
// pgAdmin comes from the same EDB zip as the PostgreSQL server (it is
// the part postgres.Install skips), so it matches the installed server
// and needs no extra download source. On macOS it is EDB's signed app
// bundle, which is never modified after extraction: the server entry
// goes into pgAdmin's own config database (~/.pgadmin on macOS,
// %APPDATA%\pgAdmin on Windows) through pgAdmin's supported CLI,
// `setup.py load-servers`, run with the bundled Python.
package pgadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"pm/internal/pmdir"
	"pm/internal/postgres"
	"pm/internal/proc"
)

// ServerName and GroupName are how the local server shows up in
// pgAdmin's browser tree.
const (
	ServerName = "Mullion (PostgreSQL)"
	GroupName  = "Mullion"
)

// desktopUser is the internal account pgAdmin's desktop mode stores
// everything under (config.DESKTOP_USER).
const desktopUser = "pgadmin4@pgadmin.org"

// Dir is where pgAdmin lives: Dir/pgAdmin 4.app (macOS) or
// Dir/pgAdmin 4/ (Windows), plus Mullion's own files next to it.
func Dir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "pgadmin") }

// layout is where the pieces of an extracted pgAdmin live.
type layout struct {
	bundle string // what Launch opens (the .app on macOS)
	exe    string // the desktop runtime's executable
	python string // the bundled interpreter
	web    string // pgAdmin's Python sources (setup.py, version.py)
}

func layoutFor(goos, dir string) layout {
	root := filepath.Join(dir, postgres.PgAdminBundle(goos))
	if goos == "darwin" {
		c := filepath.Join(root, "Contents")
		return layout{
			bundle: root,
			exe:    filepath.Join(c, "MacOS", "pgAdmin 4"),
			python: filepath.Join(c, "Frameworks", "Python.framework", "Versions", "Current", "bin", "python3"),
			web:    filepath.Join(c, "Resources", "web"),
		}
	}
	return layout{
		bundle: root,
		exe:    filepath.Join(root, "runtime", "pgAdmin4.exe"),
		python: filepath.Join(root, "python", "python.exe"),
		web:    filepath.Join(root, "web"),
	}
}

func current(paths pmdir.Paths) layout { return layoutFor(runtime.GOOS, Dir(paths)) }

// Installed reports whether pgAdmin is present.
func Installed(paths pmdir.Paths) bool {
	l := current(paths)
	for _, f := range []string{l.exe, l.python, filepath.Join(l.web, "setup.py")} {
		if _, err := os.Stat(f); err != nil {
			return false
		}
	}
	return true
}

var versionLineRe = regexp.MustCompile(`(?m)^APP_(RELEASE|REVISION)\s*=\s*(\d+)`)

// Version reads the installed pgAdmin's version ("9.17"), or "".
func Version(paths pmdir.Paths) string {
	b, err := os.ReadFile(filepath.Join(current(paths).web, "version.py"))
	if err != nil {
		return ""
	}
	return parseVersion(string(b))
}

func parseVersion(src string) string {
	var release, revision string
	for _, m := range versionLineRe.FindAllStringSubmatch(src, -1) {
		if m[1] == "RELEASE" && release == "" {
			release = m[2]
		} else if m[1] == "REVISION" && revision == "" {
			revision = m[2]
		}
	}
	if release == "" || revision == "" {
		return ""
	}
	return release + "." + revision
}

// Install downloads the pgAdmin 4 bundled with PostgreSQL pgVersion
// (replacing any installed copy) and checks that it is intact.
func Install(ctx context.Context, paths pmdir.Paths, pgVersion string) error {
	fmt.Printf("Downloading pgAdmin 4 (from the %s package)...\n", postgres.Label(pgVersion))
	if err := postgres.FetchPgAdmin(ctx, paths, pgVersion, Dir(paths)); err != nil {
		return err
	}
	// A fresh copy may be a different pgAdmin: re-check the registration.
	os.Remove(markerFile(paths))
	l := current(paths)
	if runtime.GOOS == "darwin" {
		// Go's HTTP client never sets it, but a copy fetched some other way
		// would have every launch blocked by Gatekeeper.
		_ = exec.Command("xattr", "-dr", "com.apple.quarantine", l.bundle).Run()
	}
	if !Installed(paths) {
		return fmt.Errorf("unexpected archive layout: pgAdmin 4 is incomplete in %s", Dir(paths))
	}
	return verifySignature(l.bundle)
}

// verifySignature checks, on macOS, that the app's executable carries
// a valid Apple-anchored (Developer ID) signature — EDB signs the
// bundle. --ignore-resources is needed because EDB also signed a few
// non-Mach-O files (Contents/LICENSE, the Tcl/Tk .a stubs in
// Python.framework) whose signatures live in extended attributes, which
// a zip can't carry; a strict check of the extracted bundle therefore
// reports them as unsigned even though nothing was altered.
func verifySignature(bundle string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("codesign", "--verify", "--ignore-resources",
		"-R=anchor apple generic", bundle).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pgAdmin 4's code signature does not verify: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Launch opens pgAdmin, first making sure the local server is
// registered (best-effort: a failure there is reported, not fatal).
func Launch(paths pmdir.Paths) error {
	if !Installed(paths) {
		return fmt.Errorf("pgAdmin 4 is not installed (run: mullion pgadmin)")
	}
	if err := Register(paths); err != nil {
		fmt.Printf("note: could not pre-register the Mullion server in pgAdmin (%v) — add 127.0.0.1:%d, user %s, by hand.\n",
			err, postgres.Port, postgres.Superuser)
	}
	l := current(paths)
	if runtime.GOOS == "darwin" {
		// Opening the bundle path focuses an already running pgAdmin.
		return exec.Command("open", l.bundle).Run()
	}
	// No detach attributes: HideWindow would start the GUI invisible
	// (see heidisql.Launch); a GUI process outlives the CLI on its own.
	cmd := exec.Command(l.exe)
	cmd.Dir = filepath.Dir(l.exe)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Remove deletes Mullion's pgAdmin. pgAdmin's own settings (saved
// servers, preferences) are the user's and stay where pgAdmin keeps them.
func Remove(paths pmdir.Paths) error {
	return os.RemoveAll(Dir(paths))
}

// ── server pre-registration ───────────────────────────────────────────

// dataDirFor mirrors pgAdmin's desktop-mode DATA_DIR (config.py):
// %APPDATA%\pgAdmin on Windows, ~/.pgadmin elsewhere.
func dataDirFor(goos, home, appData string) string {
	if goos == "windows" {
		return filepath.Join(appData, "pgAdmin")
	}
	return filepath.Join(home, ".pgadmin")
}

// configDB is pgAdmin's SQLite config database for this user.
func configDB() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	appData := os.Getenv("APPDATA")
	if runtime.GOOS == "windows" && appData == "" {
		return "", fmt.Errorf("APPDATA is not set")
	}
	return filepath.Join(dataDirFor(runtime.GOOS, home, appData), "pgadmin4.db"), nil
}

// markerFile records the config database the server was registered in
// (and the port it was registered with), so later launches skip the
// (slow, Python) check — and a server the user deleted in pgAdmin stays
// deleted.
func markerFile(paths pmdir.Paths) string { return filepath.Join(Dir(paths), "registered") }

// renderMarker is the marker's content: the config database, then the
// PostgreSQL port the registered entry points at.
func renderMarker(db string, port int) string {
	return db + "\nport=" + strconv.Itoa(port) + "\n"
}

// parseMarker reads a marker back. Markers written before the port was
// configurable hold only the database path — they were registered on the
// default port.
func parseMarker(b []byte) (db string, port int) {
	port = postgres.DefaultPort
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if i == 0 {
			db = line
			continue
		}
		if v, ok := strings.CutPrefix(line, "port="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				port = n
			}
		}
	}
	return db, port
}

func writeMarker(paths pmdir.Paths, db string) error {
	return os.WriteFile(markerFile(paths), []byte(renderMarker(db, postgres.Port)), 0o644)
}

// PassFile is the libpq password file the registered server points at,
// so pgAdmin connects without a password prompt. It holds the same
// password Mullion's config already stores and is rewritten on every
// launch, so it follows `mullion postgres password`.
func PassFile(paths pmdir.Paths) string { return filepath.Join(Dir(paths), "pgpass") }

// renderPassFile is one pgpass line for the local superuser (':' and
// '\' are backslash-escaped). An empty password is fine under trust
// auth — libpq never asks for one.
func renderPassFile(password string) string {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	return fmt.Sprintf("127.0.0.1:%d:*:%s:%s\n", postgres.Port, postgres.Superuser, esc)
}

// renderServers is the servers.json that load-servers imports.
func renderServers(passFile string) ([]byte, error) {
	type server struct {
		Name                 string         `json:"Name"`
		Group                string         `json:"Group"`
		Host                 string         `json:"Host"`
		Port                 int            `json:"Port"`
		MaintenanceDB        string         `json:"MaintenanceDB"`
		Username             string         `json:"Username"`
		ConnectionParameters map[string]any `json:"ConnectionParameters"`
	}
	params := map[string]any{"sslmode": "prefer", "connect_timeout": 10}
	if passFile != "" {
		params["passfile"] = passFile
	}
	doc := map[string]map[string]server{"Servers": {"1": {
		Name:                 ServerName,
		Group:                GroupName,
		Host:                 "127.0.0.1",
		Port:                 postgres.Port,
		MaintenanceDB:        "postgres",
		Username:             postgres.Superuser,
		ConnectionParameters: params,
	}}}
	return json.MarshalIndent(doc, "", "  ")
}

// hasServer reports whether a dump-servers file lists the Mullion server,
// and the port that entry points at.
func hasServer(dump []byte) (bool, int, error) {
	var doc struct {
		Servers map[string]struct {
			Name string `json:"Name"`
			Port int    `json:"Port"`
		} `json:"Servers"`
	}
	if err := json.Unmarshal(dump, &doc); err != nil {
		return false, 0, fmt.Errorf("reading pgAdmin's server list: %w", err)
	}
	for _, s := range doc.Servers {
		if s.Name == ServerName {
			return true, s.Port, nil
		}
	}
	return false, 0, nil
}

var addedRe = regexp.MustCompile(`Added \d+ Server Group\(s\) and (\d+) Server\(s\)`)

// Register makes sure pgAdmin's config database lists the local
// server. It only ever ADDS to the user's pgAdmin data: a missing
// database is created with pgAdmin's own setup-db, an existing one is
// checked with dump-servers and gets the server only if it has none by
// that name. The one edit it makes is following a moved PostgreSQL
// port: the Mullion entry's port is updated in place (updatePort). Safe
// to call on every launch — after the first success it is a file check.
func Register(paths pmdir.Paths) error {
	if err := writePassFile(paths); err != nil {
		return err
	}
	db, err := configDB()
	if err != nil {
		return err
	}
	_, dbErr := os.Stat(db)
	if b, err := os.ReadFile(markerFile(paths)); err == nil && dbErr == nil {
		if markedDB, markedPort := parseMarker(b); markedDB == db {
			if markedPort == postgres.Port {
				return nil
			}
			// Registered before, on another port: move that entry (a
			// user-deleted entry simply matches nothing and stays gone).
			if err := updatePort(paths, db); err != nil {
				return err
			}
			return writeMarker(paths, db)
		}
	}

	fmt.Println("Registering the Mullion server in pgAdmin 4...")
	if dbErr != nil {
		// First pgAdmin run on this machine: create its database the way
		// pgAdmin itself would (schema + desktop user).
		if _, err := runSetup(paths, "setup-db"); err != nil {
			return err
		}
	} else {
		dump := filepath.Join(paths.TmpDir(), "pgadmin-servers-dump.json")
		defer os.Remove(dump)
		if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
			return err
		}
		os.Remove(dump)
		out, err := runSetup(paths, "dump-servers", dump, "--user", desktopUser)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(dump)
		if err != nil {
			return fmt.Errorf("pgAdmin dump-servers wrote nothing: %s", lastLines(out))
		}
		if found, port, err := hasServer(b); err != nil {
			return err
		} else if found {
			if port != postgres.Port {
				if err := updatePort(paths, db); err != nil {
					return err
				}
			}
			return writeMarker(paths, db)
		}
	}

	servers, err := renderServers(PassFile(paths))
	if err != nil {
		return err
	}
	serversFile := filepath.Join(Dir(paths), "servers.json")
	if err := os.WriteFile(serversFile, servers, 0o644); err != nil {
		return err
	}
	out, err := runSetup(paths, "load-servers", serversFile, "--user", desktopUser)
	if err != nil {
		return err
	}
	// load-servers reports failures on stdout and still exits 0.
	if m := addedRe.FindStringSubmatch(out); m == nil || m[1] == "0" {
		return fmt.Errorf("pgAdmin load-servers failed: %s", lastLines(out))
	}
	return writeMarker(paths, db)
}

// SyncPort points an already registered Mullion server at PostgreSQL's
// current port (postgres.Port) after the user moved it, and rewrites
// the password file (whose line carries the port too). No-op when
// pgAdmin isn't installed or never registered the server — the next
// Launch registers it with the right port.
func SyncPort(paths pmdir.Paths) error {
	if !Installed(paths) {
		return nil
	}
	if _, err := os.Stat(markerFile(paths)); err != nil {
		return writePassFile(paths)
	}
	return Register(paths)
}

// updatePortScript moves the Mullion server entry of one pgAdmin user
// to a new port, straight in pgAdmin's SQLite config database (setup.py
// can only add servers, or replace ALL of them — never edit one). Only
// the entry by our name pointing at the loopback is touched.
const updatePortScript = `import sqlite3, sys
db, name, email, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
con = sqlite3.connect(db, timeout=15)
cur = con.execute(
    "UPDATE server SET port = ? WHERE name = ? AND host IN ('127.0.0.1', 'localhost') "
    "AND user_id IN (SELECT id FROM \"user\" WHERE email = ?)",
    (port, name, email))
con.commit()
print("updated %d" % cur.rowcount)`

// updatePort runs updatePortScript with pgAdmin's bundled Python.
func updatePort(paths pmdir.Paths, db string) error {
	l := current(paths)
	cmd := proc.Quiet(l.python, "-s", "-c", updatePortScript, db, ServerName, desktopUser, strconv.Itoa(postgres.Port))
	cmd.Env = setupEnv(os.Environ(), filepath.Join(Dir(paths), ".pycache"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("updating the Mullion server's port in pgAdmin: %v: %s", err, lastLines(string(out)))
	}
	return nil
}

func writePassFile(paths pmdir.Paths) error {
	if err := os.MkdirAll(Dir(paths), 0o755); err != nil {
		return err
	}
	// libpq ignores a password file others can read (on Unix).
	f := PassFile(paths)
	if err := os.WriteFile(f, []byte(renderPassFile(postgres.Password)), 0o600); err != nil {
		return err
	}
	return os.Chmod(f, 0o600)
}

// setupDriver runs pgAdmin's setup.py the way the old desktop runtime
// did: with SERVER_MODE=False already set, so config.py derives the
// desktop data directory (~/.pgadmin, %APPDATA%\pgAdmin) — run
// directly, setup.py assumes server mode and /var/lib/pgadmin.
const setupDriver = `import builtins, runpy, sys
builtins.SERVER_MODE = False
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], init_globals={"SERVER_MODE": False}, run_name="__main__")`

// runSetup runs `setup.py <args>` with the bundled Python and returns
// its combined output.
func runSetup(paths pmdir.Paths, args ...string) (string, error) {
	l := current(paths)
	cmd := proc.Quiet(l.python, append([]string{"-s", "-c", setupDriver, filepath.Join(l.web, "setup.py")}, args...)...)
	cmd.Dir = l.web
	cmd.Env = setupEnv(os.Environ(), filepath.Join(Dir(paths), ".pycache"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("pgAdmin setup.py %s: %v: %s", args[0], err, lastLines(string(out)))
	}
	return string(out), nil
}

// setupEnv drops the caller's Python settings (a PYTHONHOME or
// PYTHONPATH would point the bundled interpreter at a foreign stdlib)
// and keeps bytecode caches out of the signed bundle.
func setupEnv(environ []string, pycache string) []string {
	var env []string
	for _, kv := range environ {
		if strings.HasPrefix(strings.ToUpper(kv), "PYTHON") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"PYTHONPYCACHEPREFIX="+pycache,
		"PYTHONIOENCODING=utf-8",
		"COLUMNS=200", // rich wraps long paths at 80 columns otherwise
		"NO_COLOR=1",
	)
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}
