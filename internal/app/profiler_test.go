package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/phpver"
	"pm/internal/profiler"
)

// profilerTestApp: two PHP sites and a node site on PHP 8.4.1, whose
// php.ini loads a (fake) spx.so.
func profilerTestApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	a.State.Config.TLD = "test"
	a.State.Sites = []config.Site{
		{Name: "blog", Path: "/p/blog"},
		{Name: "shop", Path: "/p/shop"},
		{Name: "spa", Path: "/p/spa", Kind: "node"},
	}
	so := phpver.SpxSoPath(a.Paths, "8.4.1")
	os.MkdirAll(filepath.Dir(so), 0o755)
	os.WriteFile(so, nil, 0o755)
	os.WriteFile(phpver.IniPath(a.Paths, "8.4.1"), []byte("extension=\""+so+"\"\n"), 0o644)
	return a
}

func envMap(env [][2]string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e[0]] = e[1]
	}
	return m
}

func TestProfilerFastCGIEnv(t *testing.T) {
	a := profilerTestApp(t)
	blog, shop, spa := a.State.Sites[0], a.State.Sites[1], a.State.Sites[2]

	// Nobody profiles: nothing changes in the Caddyfile.
	if env := a.ProfilerFastCGIEnv(blog, "8.4.1"); env != nil {
		t.Fatalf("unprofiled: %v", env)
	}

	st, _ := profiler.LoadState(a.Paths)
	st.Sites["blog"] = &profiler.Site{Mode: profiler.ModeNext, Count: 3, Key: "abc"}
	if err := st.Save(a.Paths); err != nil {
		t.Fatal(err)
	}
	env := envMap(a.ProfilerFastCGIEnv(blog, "8.4.1"))
	v := env["PHP_ADMIN_VALUE"]
	if !strings.Contains(v, `spx.http_key="abc"`) || !strings.Contains(v, "spx.http_profiling_auto_start=0") || !strings.Contains(v, "gate.php") {
		t.Fatalf("blog admin value:\n%s", v)
	}
	if env["MULLION_SPX"] == "" {
		t.Fatalf("blog env: %v", env)
	}
	// The gate and its counter exist before Caddy points PHP at them.
	if g, ok := profiler.ReadGate(a.Paths, "blog"); !ok || g.Remaining != 3 {
		t.Fatalf("gate = %+v %v", g, ok)
	}
	if _, err := os.Stat(profiler.GatePath(a.Paths)); err != nil {
		t.Fatal(err)
	}

	// The other site of the pool gets SPX switched off (and its prepend
	// back, the gate being in play).
	o := envMap(a.ProfilerFastCGIEnv(shop, "8.4.1"))
	if o["PHP_ADMIN_VALUE"] != "spx.http_enabled=0\nspx.http_profiling_enabled=0\nauto_prepend_file=\"\"" || len(o) != 1 {
		t.Fatalf("shop env: %v", o)
	}
	if env := a.ProfilerFastCGIEnv(spa, "8.4.1"); env != nil {
		t.Fatalf("node site: %v", env)
	}
	// Another PHP version (no spx.so) is left alone.
	if env := a.ProfilerFastCGIEnv(shop, "8.3.0"); env != nil {
		t.Fatalf("other version: %v", env)
	}
}

func TestProfilerStatusFinishesNextRun(t *testing.T) {
	a := profilerTestApp(t)
	st, _ := profiler.LoadState(a.Paths)
	s := &profiler.Site{Mode: profiler.ModeNext, Count: 2, Key: "k"}
	st.Sites["blog"] = s
	st.Save(a.Paths)
	if err := profiler.Prepare(a.Paths, "blog", s, true); err != nil {
		t.Fatal(err)
	}
	info, err := a.ProfilerStatus("blog")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Enabled || info.Finished || info.Gate == nil || info.Gate.Remaining != 2 {
		t.Fatalf("running: %+v", info)
	}
	if !strings.HasPrefix(info.ControlURL, "http://blog.test/?SPX_KEY=k&SPX_UI_URI=/") {
		t.Fatalf("control url %q", info.ControlURL)
	}
	// The gate used up its count: back to browser mode.
	os.WriteFile(filepath.Join(profiler.SiteDir(a.Paths, "blog"), "gate.json"), []byte(`{"remaining":0,"captured":2}`), 0o644)
	info, err = a.ProfilerStatus("blog")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Finished || info.Settings.Mode != profiler.ModeBrowser || info.Gate != nil {
		t.Fatalf("finished: %+v", info)
	}
	st, _ = profiler.LoadState(a.Paths)
	if st.Get("blog").Mode != profiler.ModeBrowser {
		t.Fatal("mode not saved")
	}

	if _, err := a.ProfilerStatus("spa"); err == nil {
		t.Fatal("node site accepted")
	}
	if _, err := a.ProfilerStatus("nope"); err == nil {
		t.Fatal("unknown site accepted")
	}
}

func TestSetProfilerOff(t *testing.T) {
	a := profilerTestApp(t)
	st, _ := profiler.LoadState(a.Paths)
	st.Sites["blog"] = &profiler.Site{Mode: profiler.ModeBrowser, Key: "k"}
	st.Save(a.Paths)
	info, err := a.SetProfiler(t.Context(), "blog", false, profiler.Site{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Enabled {
		t.Fatalf("still on: %+v", info)
	}
	if st, _ := profiler.LoadState(a.Paths); st.Get("blog") != nil {
		t.Fatal("state kept the site")
	}
}
