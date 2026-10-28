package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNeedsHostRewrite(t *testing.T) {
	cases := map[string]bool{
		"vite":                               true,
		"webpack-dev-server":                 true,
		filepath.Join("@vue", "cli-service"): true,
		"react-scripts":                      true,
		filepath.Join("@angular-devkit", "build-angular"): true,
		"next": false,
	}
	for mod, want := range cases {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", mod), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := needsHostRewrite(dir); got != want {
			t.Errorf("%s: needsHostRewrite = %v, want %v", mod, got, want)
		}
	}
}
