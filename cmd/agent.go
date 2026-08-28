package cmd

import (
	"fmt"
	"html"
	"net/http"
	"sync"

	"github.com/spf13/cobra"

	"pm/internal/agent"
	"pm/internal/app"
	"pm/internal/devserver"
)

// agentCmd is the hidden background helper behind wake-on-demand:
// Caddy proxies a down site's requests here; each page load shows a
// "starting…" screen while the dev server boots, and the reload lands
// on the running app.
var agentCmd = &cobra.Command{
	Use:    "agent",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			mu       sync.Mutex
			inFlight = map[string]bool{}
		)

		wake := func(name string) {
			mu.Lock()
			if inFlight[name] {
				mu.Unlock()
				return
			}
			inFlight[name] = true
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(inFlight, name)
				mu.Unlock()
			}()

			a, err := app.New()
			if err != nil {
				return
			}
			site := a.State.FindSite(name)
			if site == nil || site.Kind != "node" || site.Mode == "build" {
				return
			}
			if devserver.Running(a.Paths, site.Name) > 0 {
				return
			}
			fmt.Printf("wake: starting %s's dev server\n", name)
			site.DevPaused = false
			if err := a.Apply(); err != nil {
				fmt.Printf("wake: %s: %v\n", name, err)
			}
		}

		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			name := slugify(r.Header.Get(agent.WakeHeader))
			if name == "" {
				http.NotFound(w, r)
				return
			}
			go wake(name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, wakePage, html.EscapeString(name))
		})
		fmt.Printf("mullion agent listening on 127.0.0.1:%d\n", agent.Port)
		return http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", agent.Port), nil)
	},
}

const wakePage = `<!doctype html><html><head><meta charset="utf-8">
<title>Starting…</title>
<style>
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         background:#f6f7f9; color:#1a1d26; font:15px/1.6 "Segoe UI",system-ui,sans-serif; }
  @media (prefers-color-scheme: dark) { body { background:#0c0e13; color:#e7e9f1; } }
  .box { text-align:center; }
  .spin { width:34px; height:34px; border:3px solid rgba(79,99,230,.25); border-top-color:#4f63e6;
          border-radius:50%%; margin:0 auto 18px; animation:s .7s linear infinite; }
  @keyframes s { to { transform:rotate(360deg); } }
  b { font-size:17px; } p { color:#737a8c; margin:6px 0 0; font-size:13px; }
</style></head><body>
<div class="box"><div class="spin"></div>
<b>Starting %s&rsquo;s dev server…</b>
<p>Mullion is waking the project up — this page refreshes by itself.<br>First start can take a minute (installing dependencies if needed).</p>
</div>
<script>setTimeout(() => location.reload(), 2000);</script>
</body></html>`

func init() {
	rootCmd.AddCommand(agentCmd)
}
