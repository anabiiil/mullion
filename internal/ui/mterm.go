package ui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"pm/internal/app"
	"pm/internal/mterm"
)

// registerMterm adds the Mullion Terminal endpoints — the standalone
// terminal app the panel can open folders in instead of its built-in
// terminal (preference 'terminal.app' = 'external'):
//
//	/api/mterm/status  → mterm.Status (latest version best-effort, ≤4s)
//	/api/mterm/install → job: download + install/update, with progress
//	/api/mterm/open    {dir} or {site} → opens it as a new tab
func registerMterm(api apiRegistrar) {
	api("/api/mterm/status", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Latest bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		return mterm.GetStatus(ctx, in.Latest), nil
	})
	api("/api/mterm/install", func(a *app.App, r *http.Request) (any, error) {
		if !mterm.Available() {
			return nil, fmt.Errorf("%s", mterm.UnavailableReason())
		}
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			setStatus("Checking the latest Mullion Terminal…")
			v, err := mterm.Install(context.Background(), func(stage string, done, total int64) {
				switch {
				case stage == "install":
					setStatus("Installing Mullion Terminal…")
				case total > 0:
					setStatus(fmt.Sprintf("Downloading Mullion Terminal… %d%% (%.1f / %.1f MB)",
						done*100/total, float64(done)/1e6, float64(total)/1e6))
				default:
					setStatus(fmt.Sprintf("Downloading Mullion Terminal… %.1f MB", float64(done)/1e6))
				}
			})
			if err != nil {
				return nil, err
			}
			st := mterm.GetStatus(context.Background(), false)
			if st.Version == "" {
				st.Version = v
			}
			return st, nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	})
	api("/api/mterm/open", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Dir, Site string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		dir := in.Dir
		if in.Site != "" {
			s := a.State.FindSite(in.Site)
			if s == nil {
				return nil, fmt.Errorf("no site named %q", in.Site)
			}
			dir = s.Path
		}
		return nil, mterm.Open(dir)
	})
}
