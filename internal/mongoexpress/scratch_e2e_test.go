package mongoexpress

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pm/internal/mongodb"
	"pm/internal/nodever"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// TestScratchE2E is a manual, network-touching end-to-end check driven
// entirely by environment variables — not part of the normal test suite
// (skipped unless MONGOEXPRESS_E2E_SCRATCH_HOME is set). It installs the
// wrapper project with a real (read-only) Node binary dir, boots
// mongo-express against a real (scratch) mongod, and curls it.
//
// Required env:
//
//	MONGOEXPRESS_E2E_SCRATCH_HOME  scratch dir for the wrapper project's
//	                               own ~/.mullion-style Home (npm cache
//	                               and HOME are pointed here too)
//	MONGOEXPRESS_E2E_NODE_DIR      an installed Node version dir (read-only)
//	MONGOEXPRESS_E2E_MONGO_HOME    scratch pmdir Home with an installed mongod
//	MONGOEXPRESS_E2E_MONGO_VERSION the installed mongod version under it
//	MONGOEXPRESS_E2E_PORT          port for mongo-express itself (e.g. 18081)
func TestScratchE2E(t *testing.T) {
	scratchHome := os.Getenv("MONGOEXPRESS_E2E_SCRATCH_HOME")
	nodeDir := os.Getenv("MONGOEXPRESS_E2E_NODE_DIR")
	mongoHome := os.Getenv("MONGOEXPRESS_E2E_MONGO_HOME")
	mongoVersion := os.Getenv("MONGOEXPRESS_E2E_MONGO_VERSION")
	portStr := os.Getenv("MONGOEXPRESS_E2E_PORT")
	if scratchHome == "" || nodeDir == "" || mongoHome == "" || mongoVersion == "" || portStr == "" {
		t.Skip("set MONGOEXPRESS_E2E_* env vars to run this manual scratch test")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad MONGOEXPRESS_E2E_PORT: %v", err)
	}

	// npm's cache and HOME must land in scratch, never the real HOME.
	t.Setenv("HOME", scratchHome)
	npmCache := filepath.Join(scratchHome, "npm-cache")
	if err := os.MkdirAll(npmCache, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("npm_config_cache", npmCache)

	mongoPaths := pmdir.Paths{Home: mongoHome}
	if err := mongoPaths.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout(mongoHome): %v", err)
	}
	if err := mongodb.Start(mongoPaths, mongoVersion); err != nil {
		t.Fatalf("mongodb.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := mongodb.Stop(mongoPaths, mongoVersion); err != nil {
			t.Logf("mongodb.Stop: %v", err)
		}
	})

	mexPaths := pmdir.Paths{Home: filepath.Join(scratchHome, "mullion-home")}
	if err := mexPaths.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout(mexHome): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := Install(ctx, mexPaths, nodeDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !Installed(mexPaths) {
		t.Fatal("Installed() = false after Install succeeded")
	}
	dir := Dir(mexPaths)
	for _, name := range []string{"package.json", "start.js", filepath.Join("node_modules", "mongo-express", "app.js")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}

	cmd := exec.CommandContext(ctx, nodever.Tool(nodeDir, "node"), "start.js")
	proc.HideConsole(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+nodever.BinDir(nodeDir)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PORT="+strconv.Itoa(port),
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting node start.js: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		t.Logf("mongo-express output:\n%s", out.String())
	})

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	var resp *http.Response
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s never succeeded: %v\noutput so far:\n%s", url, err, out.String())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200\nbody:\n%s", url, resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "mongo") {
		t.Fatalf("response body doesn't look like mongo-express's UI:\n%s", body)
	}
	if resp.Header.Get("Www-Authenticate") != "" {
		t.Fatalf("got a WWW-Authenticate header — basic auth is prompting: %s", resp.Header.Get("Www-Authenticate"))
	}
	t.Logf("mongo-express served %d bytes at %s with no basic-auth prompt", len(body), url)
}
