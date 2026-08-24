//go:build live

// A manual end-to-end test of the managed dev server, driven against a
// real project. Not part of the normal test run:
//
//	MULLION_LIVE_PROJECT=/path/to/vite-app MULLION_LIVE_NODE=/path/to/.mullion/node/<v> \
//	  go test -tags live -run TestLiveDevServer -v ./internal/devserver/
package devserver

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func TestLiveDevServer(t *testing.T) {
	project := os.Getenv("MULLION_LIVE_PROJECT")
	nodeDir := os.Getenv("MULLION_LIVE_NODE")
	if project == "" || nodeDir == "" {
		t.Skip("set MULLION_LIVE_PROJECT and MULLION_LIVE_NODE")
	}
	home := t.TempDir()
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	site := config.Site{Name: "livetest", Path: project, Kind: "node", DevPort: 42099}

	port, err := Ensure(paths, site, "livetest.test", nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dev server on port %d", port)

	// Re-Ensure must be a fast no-op returning the same port.
	again, err := Ensure(paths, site, "livetest.test", nodeDir)
	if err != nil || again != port {
		t.Fatalf("re-ensure: port %d err %v", again, err)
	}

	// The dev server must answer with the app's HTML.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "<div id=\"app\"") && !strings.Contains(string(body), "<title>") {
		t.Fatalf("unexpected response: %.200s", body)
	}
	t.Logf("HTML served OK (%d bytes)", len(body))

	Stop(paths, site.Name)
	time.Sleep(1 * time.Second)
	if p := Running(paths, site.Name); p != 0 {
		t.Fatalf("still running on %d after Stop", p)
	}
	t.Log("stopped cleanly")
}
