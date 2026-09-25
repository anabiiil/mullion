package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func TestNormalizeAlias(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"api", "api", false},
		{" API ", "api", false},
		{"v1.api", "v1.api", false},
		{"api.shop.test", "api", false}, // pasted full host
		{".api.", "api", false},
		{"*", "*", false},
		{"a-b-9", "a-b-9", false},
		{"", "", true},
		{"-api", "", true},
		{"api-", "", true},
		{"a_b", "", true},
		{"*.api", "", true},
		{"a..b", "", true},
		{"ünï", "", true},
		{strings.Repeat("a", 64), "", true},
	}
	for _, c := range cases {
		got, err := NormalizeAlias(c.in, "shop.test")
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeAlias(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeAlias(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestSiteHosts(t *testing.T) {
	s := config.Site{Name: "shop", Aliases: []string{"*", "v1.api", "api"}}
	got := SiteHosts(s, "test")
	want := []string{"shop.test", "api.shop.test", "v1.api.shop.test", "*.shop.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SiteHosts = %v, want %v", got, want)
	}
	if got := SiteHosts(config.Site{Name: "blog"}, "local"); !reflect.DeepEqual(got, []string{"blog.local"}) {
		t.Fatalf("SiteHosts(no aliases) = %v", got)
	}
}

// sandboxApp builds an App over a temp HOME with skipApply set, so
// nothing here can reach the real hosts file, Caddy, or resolver.
func sandboxApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	paths := pmdir.Paths{Home: filepath.Join(home, ".mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Paths: paths, State: state, skipApply: true}
}

// stubResolver replaces the OS resolver hookup with a recorder.
func stubResolver(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	oi, or, oq := installResolver, removeResolver, resolverInstalled
	installResolver = func(tld string) error { calls = append(calls, "install "+tld); return nil }
	removeResolver = func(tld string) error { calls = append(calls, "remove "+tld); return nil }
	resolverInstalled = func(tld string) bool { return false }
	t.Cleanup(func() { installResolver, removeResolver, resolverInstalled = oi, or, oq })
	return &calls
}

func TestSetSiteAliasesAndHostnames(t *testing.T) {
	a := sandboxApp(t)
	proj := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(filepath.Join(proj, "dist"), 0o755)
	os.WriteFile(filepath.Join(proj, "dist", "index.html"), []byte("hi"), 0o644)
	a.State.Sites = []config.Site{{Name: "shop", Path: proj, Kind: "static", BuildDir: "dist"}}

	if err := a.SetSiteAliases("shop", []string{"API", "*", "api", "v1.api.shop.test"}); err != nil {
		t.Fatal(err)
	}
	if got := a.State.FindSite("shop").Aliases; !reflect.DeepEqual(got, []string{"*", "api", "v1.api"}) {
		t.Fatalf("aliases = %v", got)
	}
	// Saved to disk.
	st, err := config.Load(a.Paths)
	if err != nil || !reflect.DeepEqual(st.FindSite("shop").Aliases, []string{"*", "api", "v1.api"}) {
		t.Fatalf("aliases not persisted: %v %v", st.FindSite("shop"), err)
	}
	// Hosts get the explicit aliases only — never a wildcard.
	if got := a.Hostnames(); !reflect.DeepEqual(got, []string{"shop.test", "api.shop.test", "v1.api.shop.test"}) {
		t.Fatalf("Hostnames = %v", got)
	}
	if err := a.SetSiteAliases("shop", []string{"bad_label"}); err == nil {
		t.Fatal("invalid alias must be rejected")
	}
	if err := a.SetSiteAliases("nope", nil); err == nil {
		t.Fatal("unknown site must be rejected")
	}

	if err := a.WriteCaddyfile(nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a.Paths.Caddyfile())
	if !strings.Contains(string(data), "http://shop.test, http://api.shop.test, http://v1.api.shop.test, http://*.shop.test {") {
		t.Fatalf("caddyfile address list wrong:\n%s", data)
	}

	if err := a.SetSiteAliases("shop", nil); err != nil {
		t.Fatal(err)
	}
	if got := a.Hostnames(); !reflect.DeepEqual(got, []string{"shop.test"}) {
		t.Fatalf("Hostnames after clearing = %v", got)
	}
}

func TestSetWildcardDNSAndTLDMove(t *testing.T) {
	a := sandboxApp(t)
	calls := stubResolver(t)

	if err := a.SetWildcardDNS(true); err != nil {
		t.Fatal(err)
	}
	if !a.State.Config.WildcardDNS || !a.needsAgent() {
		t.Fatal("wildcard DNS not enabled / agent not required")
	}
	if err := a.SetTLD("local"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWildcardDNS(false); err != nil {
		t.Fatal(err)
	}
	want := []string{"install test", "install local", "remove test", "remove local"}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resolver calls = %v, want %v", *calls, want)
	}
	st, _ := config.Load(a.Paths)
	if st.Config.WildcardDNS || st.Config.TLD != "local" {
		t.Fatalf("saved config wrong: %+v", st.Config)
	}
	// With DNS off, a TLD change must not touch the resolver.
	*calls = nil
	if err := a.SetTLD("test"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("resolver touched while DNS off: %v", *calls)
	}
}
