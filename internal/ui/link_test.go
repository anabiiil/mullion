package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pm/internal/app"
)

// callAPI calls one panel endpoint and decodes its {ok, data, error}.
// It takes no *testing.T so a test can run it on its own goroutine.
func callAPI(srv *httptest.Server, path string, body any, data any) (bool, string, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(string(raw)))
	req.Header.Set("X-Mullion-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", err
	}
	if out.OK && data != nil {
		if err := json.Unmarshal(out.Data, data); err != nil {
			return false, "", err
		}
	}
	return out.OK, out.Error, nil
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body any, data any) (bool, string) {
	t.Helper()
	ok, msg, err := callAPI(srv, path, body, data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return ok, msg
}

// Linking a project must answer as soon as the site is saved: the
// machine-side setup (Apply — dev server, hosts file behind an admin
// prompt, Caddy) runs as a job the page polls. It used to run inside
// the request, so a slow or unseen step kept the panel on "Linking…"
// forever although the site was already added.
func TestLinkAnswersBeforeApplyFinishes(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("SystemDrive", home)
	} else {
		t.Setenv("HOME", home)
	}
	project := filepath.Join(t.TempDir(), "MyApp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "index.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	prev := applySite
	applySite = func(a *app.App) error {
		a.Progress("Updating the hosts file…")
		<-release // e.g. an administrator prompt nobody answered yet
		return errors.New("UAC declined")
	}
	defer func() { applySite = prev }()

	srv := httptest.NewServer(newMux("tok"))
	defer srv.Close()

	var link linkResult
	done := make(chan struct{})
	var ok bool
	var errMsg string
	var callErr error
	go func() {
		ok, errMsg, callErr = callAPI(srv, "/api/sites/link", map[string]string{"path": project}, &link)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("/api/sites/link is still waiting on Apply — it must answer once the site is saved")
	}
	if callErr != nil {
		t.Fatal(callErr)
	}
	if !ok {
		t.Fatalf("link failed: %s", errMsg)
	}
	if link.Name != "myapp" || link.URL != "http://myapp.test" || link.Job == "" {
		t.Fatalf("link answer = %+v, want name myapp, url http://myapp.test and a job id", link)
	}

	// The site is saved before the answer, so a reload lists it.
	a, err := app.New()
	if err != nil {
		t.Fatal(err)
	}
	if a.State.FindSite("myapp") == nil {
		t.Fatalf("site not saved; sites = %+v", a.State.Sites)
	}

	// The job reports Apply's progress while it runs...
	var st JobStatus
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ok, msg := postJSON(t, srv, "/api/job", map[string]string{"id": link.Job}, &st); !ok {
			t.Fatalf("/api/job: %s", msg)
		}
		if st.Log == "Updating the hosts file…" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Done || st.Log != "Updating the hosts file…" {
		t.Fatalf("running job status = %+v, want not done with Apply's progress line", st)
	}

	// ...and its error (a declined prompt) once it ends.
	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for !st.Done && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		postJSON(t, srv, "/api/job", map[string]string{"id": link.Job}, &st)
	}
	if !st.Done || st.Error != "UAC declined" {
		t.Fatalf("finished job status = %+v, want done with Apply's error", st)
	}

	// Linking the same folder again (retrying a declined hosts update)
	// is job-backed too.
	applySite = func(a *app.App) error { return nil }
	var again linkResult
	if ok, msg := postJSON(t, srv, "/api/sites/link", map[string]string{"path": project}, &again); !ok {
		t.Fatalf("re-link failed: %s", msg)
	}
	if again.Name != "myapp" || again.Job == "" || again.Job == link.Job {
		t.Fatalf("re-link answer = %+v, want myapp with a new job", again)
	}
	// Let that job finish before applySite is restored (no data race).
	var st2 JobStatus
	deadline = time.Now().Add(5 * time.Second)
	for !st2.Done && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		postJSON(t, srv, "/api/job", map[string]string{"id": again.Job}, &st2)
	}
	if !st2.Done || st2.Error != "" {
		t.Fatalf("re-link job status = %+v, want done without error", st2)
	}
}
