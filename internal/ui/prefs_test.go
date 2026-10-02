package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"pm/internal/app"
)

// Preferences set together (terminal.app + terminal.appChosen) must all
// survive: concurrent writes used to overwrite each other's key.
func TestConcurrentPrefSetsAllPersist(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("SystemDrive", home)
	} else {
		t.Setenv("HOME", home)
	}
	srv := httptest.NewServer(newMux("tok"))
	defer srv.Close()

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"key":"k%d","value":"v%d"}`, i, i)
			req, _ := http.NewRequest("POST", srv.URL+"/api/prefs/set", strings.NewReader(body))
			req.Header.Set("X-Mullion-Token", "tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("set k%d: HTTP %d", i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	a, err := app.New()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if got := a.State.Config.UI[fmt.Sprintf("k%d", i)]; got != fmt.Sprintf("v%d", i) {
			t.Errorf("k%d = %q after concurrent sets, want v%d (all prefs: %v)", i, got, i, a.State.Config.UI)
		}
	}
}
