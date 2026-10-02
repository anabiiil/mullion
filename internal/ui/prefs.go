package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"pm/internal/app"
)

// prefsMu serializes preference writes. The panel often sets two keys at
// once (choosing Mullion Terminal stores terminal.app and
// terminal.appChosen together); each request used to save the config it
// had loaded before the other's write landed, so the last save silently
// dropped the other key.
var prefsMu sync.Mutex

// registerPrefs adds the panel-preference endpoints: GET/POST
// /api/prefs returns every preference, /api/prefs/set {key, value}
// stores one ("" deletes it). Values are opaque strings to the backend.
func registerPrefs(api apiRegistrar) {
	api("/api/prefs", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.UI == nil {
			return map[string]string{}, nil
		}
		return a.State.Config.UI, nil
	})
	api("/api/prefs/set", func(_ *app.App, r *http.Request) (any, error) {
		var in struct{ Key, Value string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if in.Key == "" || len(in.Key) > 100 || len(in.Value) > 64<<10 {
			return nil, errors.New("invalid preference")
		}
		prefsMu.Lock()
		defer prefsMu.Unlock()
		// Re-read under the lock: the App handed in was loaded before it.
		a, err := app.New()
		if err != nil {
			return nil, err
		}
		if a.State.Config.UI == nil {
			a.State.Config.UI = map[string]string{}
		}
		if in.Value == "" {
			delete(a.State.Config.UI, in.Key)
		} else {
			a.State.Config.UI[in.Key] = in.Value
		}
		return nil, a.State.Save()
	})
}
