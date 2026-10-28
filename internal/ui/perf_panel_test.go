package ui

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestPerfPanelServe serves the control panel (no stack bring-up, no
// Apply) for a manual look at the Performance tab in a sandbox:
//
//	HOME=<sandbox> MULLION_PERF_PANEL=127.0.0.1:18090 go test ./internal/ui -run TestPerfPanelServe -timeout 30m
//
// It stops when the file named by MULLION_PERF_PANEL_STOP appears (or
// after 20 minutes). The token is "perf-panel-test".
func TestPerfPanelServe(t *testing.T) {
	addr := os.Getenv("MULLION_PERF_PANEL")
	if addr == "" {
		t.Skip("set MULLION_PERF_PANEL=<addr> (and a sandbox HOME)")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newMux("perf-panel-test")}
	go srv.Serve(ln)
	defer srv.Close()
	t.Logf("panel: http://%s/?t=perf-panel-test", addr)
	stop := os.Getenv("MULLION_PERF_PANEL_STOP")
	for end := time.Now().Add(20 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		if stop != "" {
			if _, err := os.Stat(stop); err == nil {
				return
			}
		}
	}
}
