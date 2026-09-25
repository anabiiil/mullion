package cmd

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/agent"
	"pm/internal/app"
	"pm/internal/devserver"
	"pm/internal/sysproc"
	"pm/internal/version"
)

// Idle limits: with no tab open (no live connection) a dev server
// sleeps fast; with a tab open it gets a longer grace, and any action —
// a request through the domain OR a source-file edit — resets the clock.
const (
	idleNoTab   = 2 * time.Minute
	idleWithTab = 10 * time.Minute
)

// agentCmd is the hidden background helper behind wake-on-demand:
// Caddy proxies a down site's requests here; each page load shows a
// "starting…" screen while the dev server boots, and the reload lands
// on the running app. It also puts idle dev servers back to sleep.
var agentCmd = &cobra.Command{
	Use:    "agent",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			mu       sync.Mutex
			inFlight = map[string]bool{}
			lastErr  = map[string]string{}
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
			fmt.Printf("wake: %s\n", name)
			site.DevPaused = false
			// Apply even when the server is already up: it re-syncs the
			// Caddyfile and reloads Caddy, so a previously failed reload
			// can never leave the domain stuck on this starting page.
			err = a.Apply()
			mu.Lock()
			if err != nil {
				lastErr[name] = err.Error()
				fmt.Printf("wake: %s: %v\n", name, err)
			} else if devserver.Running(a.Paths, site.Name) == 0 {
				lastErr[name] = "the dev server did not come up — last log lines:\n" + devserver.LogTail(a.Paths, site.Name)
			} else {
				delete(lastErr, name)
			}
			mu.Unlock()
		}

		// Idle watcher: a dev server with no open tab (no established
		// connections through Caddy) and no recent requests goes back to
		// sleep — opening the link wakes it again.
		go func() {
			for range time.Tick(time.Minute) {
				a, err := app.New()
				if err != nil {
					continue
				}
				changed := false
				for i := range a.State.Sites {
					site := &a.State.Sites[i]
					if site.Kind != "node" || site.Mode == "build" || site.DevPaused {
						continue
					}
					port := devserver.Running(a.Paths, site.Name)
					if port == 0 {
						continue
					}
					limit := idleNoTab
					if sysproc.PortActive(port) {
						limit = idleWithTab // a tab is open (live websocket)
					}
					activity := time.Time{}
					accessLog := filepath.Join(a.Paths.LogsDir(), a.State.Host(*site)+".log")
					if info, err := os.Stat(accessLog); err == nil {
						activity = info.ModTime()
					}
					// Editing code counts as action too — an active coding
					// session must never be killed under the developer.
					if t := latestSourceMtime(site.Path); t.After(activity) {
						activity = t
					}
					if time.Since(activity) < limit {
						continue
					}
					fmt.Printf("idle: stopping %s after %s of inactivity\n", site.Name, limit)
					site.DevPaused = true
					devserver.Stop(a.Paths, site.Name)
					changed = true
				}
				if changed {
					if err := a.Apply(); err != nil {
						fmt.Println("idle:", err)
					}
				}
			}
		}()

		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			name := slugify(r.Header.Get(agent.WakeHeader))
			if name == "" {
				// The version endpoint lets Ensure retire stale agents
				// after an upgrade.
				if r.URL.Path == "/__mullion-agent-version" {
					fmt.Fprint(w, version.Number)
					return
				}
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/__mullion-wake-status" {
				mu.Lock()
				out := map[string]string{"error": lastErr[name]}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				json.NewEncoder(w).Encode(out)
				return
			}
			go wake(name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, wakePage, html.EscapeString(name))
		})
		fmt.Printf("mullion agent %s listening on 127.0.0.1:%d\n", version.Number, agent.Port)
		return http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", agent.Port), nil)
	},
}

const wakePage = `<!doctype html><html><head><meta charset="utf-8">
<title>Starting…</title>
<style>
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         background:#f6f7f9; color:#1a1d26; font:15px/1.6 "Segoe UI",system-ui,sans-serif; }
  @media (prefers-color-scheme: dark) { body { background:#0c0e13; color:#e7e9f1; } }
  .box { text-align:center; max-width:640px; padding:0 20px; }
  .spin { width:34px; height:34px; border:3px solid rgba(79,99,230,.25); border-top-color:#4f63e6;
          border-radius:50%%; margin:0 auto 18px; animation:s .7s linear infinite; }
  @keyframes s { to { transform:rotate(360deg); } }
  b { font-size:17px; } p { color:#737a8c; margin:6px 0 0; font-size:13px; }
  pre { display:none; text-align:left; background:rgba(212,55,62,.08); color:#d4373e;
        border-radius:10px; padding:14px 18px; font-size:12px; white-space:pre-wrap;
        margin-top:18px; }
</style></head><body>
<div class="box"><div class="spin" id="spin"></div>
<b id="title">Starting %s&rsquo;s dev server…</b>
<p id="sub">Mullion is waking the project up — this page refreshes by itself.<br>First start can take a minute (installing dependencies if needed).</p>
<pre id="err"></pre>
</div>
<script>
async function tick() {
  try {
    const res = await fetch('/__mullion-wake-status', { cache: 'no-store' });
    const st = await res.json();
    if (st.error) {
      document.getElementById('spin').style.display = 'none';
      document.getElementById('err').style.display = 'block';
      document.getElementById('err').textContent = st.error;
      document.getElementById('title').textContent = 'The dev server could not start';
      document.getElementById('sub').innerHTML = 'Fix the problem, then refresh this page to retry.';
      return;
    }
  } catch (e) { /* proxy switched over — reload lands on the app */ }
  location.reload();
}
setTimeout(tick, 2000);
</script>
</body></html>`

// latestSourceMtime finds the newest file change in a project, skipping
// the heavy generated directories; the walk is capped so a minutely
// check stays cheap.
func latestSourceMtime(root string) time.Time {
	skip := map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		".output": true, ".next": true, ".nuxt": true, "vendor": true,
	}
	var newest time.Time
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if count++; count > 4000 {
			return filepath.SkipAll
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
