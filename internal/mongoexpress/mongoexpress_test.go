package mongoexpress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/mongodb"
	"pm/internal/pmdir"
)

func TestRenderPackageJSON(t *testing.T) {
	data, err := renderPackageJSON()
	if err != nil {
		t.Fatalf("renderPackageJSON: %v", err)
	}

	var pkg struct {
		Name         string            `json:"name"`
		Private      bool              `json:"private"`
		Type         string            `json:"type"`
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("package.json is not valid JSON: %v\n%s", err, data)
	}

	if pkg.Name != "mullion-mongo-express" {
		t.Errorf("name = %q, want mullion-mongo-express", pkg.Name)
	}
	if !pkg.Private {
		t.Error("private = false, want true")
	}
	// start.js itself uses import/top-level-await; without "type":"module",
	// `node start.js` would try (and fail) to parse it as CommonJS. This is
	// independent of mongo-express's own module type (it's CommonJS, and
	// resolves that from its own nested package.json regardless of ours).
	if pkg.Type != "module" {
		t.Errorf(`type = %q, want "module"`, pkg.Type)
	}
	if got := pkg.Scripts["start"]; got != "node start.js" {
		t.Errorf(`scripts.start = %q, want "node start.js"`, got)
	}
	if got := pkg.Dependencies["mongo-express"]; got != Version {
		t.Errorf("dependencies[mongo-express] = %q, want %q", got, Version)
	}
}

func TestRenderStartJS(t *testing.T) {
	js := renderStartJS("mongodb://127.0.0.1:27017", "cookie-secret-123", "session-secret-456")

	for _, want := range []string{
		`setDefault('ME_CONFIG_MONGODB_URL', "mongodb://127.0.0.1:27017")`,
		`setDefault('ME_CONFIG_MONGODB_ENABLE_ADMIN', 'true')`,
		`setDefault('ME_CONFIG_BASICAUTH_USERNAME', '')`,
		`setDefault('ME_CONFIG_SITE_COOKIESECRET', "cookie-secret-123")`,
		`setDefault('ME_CONFIG_SITE_SESSIONSECRET', "session-secret-456")`,
		`setDefault('VCAP_APP_HOST', '127.0.0.1')`,
		`'node_modules', 'mongo-express', 'app.js'`,
		"await import(pathToFileURL(appEntry).href)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("start.js missing %q\n--- full output ---\n%s", want, js)
		}
	}

	// PORT is intentionally left for the caller (mullion's devserver) to
	// supply — start.js must not hardcode or default it itself.
	if strings.Contains(js, "setDefault('PORT'") {
		t.Error("start.js should not set a PORT default; devserver supplies it")
	}
}

func TestRenderStartJSEscapesTemplateValues(t *testing.T) {
	// %q-based rendering must survive values containing quotes/backslashes
	// without producing invalid JavaScript (e.g. a secret that happens to
	// contain a double quote).
	js := renderStartJS(`mongodb://127.0.0.1:27017/db?x="y"`, `sec"ret`, `back\slash`)
	if !strings.Contains(js, `\"y\"`) {
		t.Errorf("expected escaped quote in rendered URI, got:\n%s", js)
	}
}

func TestDirAndInstalled(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}

	if got, want := Dir(paths), filepath.Join(paths.Home, "mongo-express"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if Installed(paths) {
		t.Error("Installed() = true before anything was written")
	}

	entry := nodeModulesEntry(paths)
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Installed(paths) {
		t.Error("Installed() = false after app.js was created")
	}
}

func TestRemove(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	dir := Dir(paths)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(paths); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Dir() still exists after Remove: err=%v", err)
	}

	// Remove on an already-absent directory must not error.
	if err := Remove(paths); err != nil {
		t.Fatalf("Remove on missing dir: %v", err)
	}
}

func TestRefreshConfigFollowsPort(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	// No start.js yet: nothing to do.
	if changed, err := RefreshConfig(paths); err != nil || changed {
		t.Fatalf("missing start.js: %v, %v", changed, err)
	}
	os.MkdirAll(Dir(paths), 0o755)
	js := renderStartJS("mongodb://127.0.0.1:27017", "cookie", "session")
	file := filepath.Join(Dir(paths), "start.js")
	os.WriteFile(file, []byte(js), 0o644)

	old := mongodb.Port
	t.Cleanup(func() { mongodb.Port = old })
	mongodb.Port = 27017
	if changed, err := RefreshConfig(paths); err != nil || changed {
		t.Fatalf("same port: %v, %v", changed, err)
	}
	mongodb.Port = 27018
	if changed, err := RefreshConfig(paths); err != nil || !changed {
		t.Fatalf("new port: %v, %v", changed, err)
	}
	b, _ := os.ReadFile(file)
	got := string(b)
	if !strings.Contains(got, `setDefault('ME_CONFIG_MONGODB_URL', "mongodb://127.0.0.1:27018");`) {
		t.Errorf("URL not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `"cookie"`) || !strings.Contains(got, `"session"`) {
		t.Error("secrets were not preserved")
	}
	if got != renderStartJS("mongodb://127.0.0.1:27018", "cookie", "session") {
		t.Error("rewrite differs from a fresh render with the new URL")
	}
}
