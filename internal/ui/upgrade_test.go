package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"pm/internal/app"
	"pm/internal/selfupdate"
)

// The status endpoint reports the latest release from GitHub (faked
// here) alongside the running version and channel. Only status is
// exercised: /api/upgrade/start would run a real package manager.
func TestUpgradeStatusEndpoint(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("SystemDrive", home)
	} else {
		t.Setenv("HOME", home)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v99.0.0", "html_url": "https://github.com/anabiiil/mullion/releases/tag/v99.0.0",
			"body": "notes", "published_at": "2026-10-20T10:00:00Z",
		})
	}))
	defer gh.Close()
	old := selfupdate.APIBase
	selfupdate.APIBase = gh.URL
	defer func() { selfupdate.APIBase = old }()

	srv := httptest.NewServer(newMux("tok"))
	defer srv.Close()
	var st struct {
		Current, Latest, URL, Channel, Relaunch, Manual string
		Update                                          bool
	}
	if ok, msg := postJSON(t, srv, "/api/upgrade/status", map[string]bool{"force": true}, &st); !ok {
		t.Fatal(msg)
	}
	if st.Latest != "99.0.0" || !st.Update || st.Current == "" || st.Channel == "" || st.Manual == "" || st.Relaunch == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestSetUpgradedPref(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("SystemDrive", home)
	} else {
		t.Setenv("HOME", home)
	}
	setUpgradedPref("9.9.9")
	a, err := app.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := a.State.Config.UI[upgradedPref]; got != "9.9.9" {
		t.Fatalf("pref = %q", got)
	}
}
