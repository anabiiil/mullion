package adminer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestPickAsset(t *testing.T) {
	assets := []asset{
		{Name: "adminer-6.1.2-en.php", URL: "u-en"},
		{Name: "adminer-6.1.2-mysql-en.php", URL: "u-mysql-en"},
		{Name: "adminer-6.1.2-mysql.php", URL: "u-mysql"},
		{Name: "adminer-6.1.2.php", URL: "u-full"},
		{Name: "adminer-6.1.2.zip", URL: "u-zip"},
		{Name: "editor-6.1.2.php", URL: "u-editor"},
	}
	a, v, err := pickAsset(assets)
	if err != nil {
		t.Fatal(err)
	}
	if a.URL != "u-full" || v != "6.1.2" {
		t.Errorf("picked %+v %q, want the full build", a, v)
	}
	if _, _, err := pickAsset(assets[:3]); err == nil {
		t.Error("no full build must be an error")
	}
}

func TestInstallFromRelease(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(release{TagName: "v6.1.2", Assets: []asset{
				{Name: "adminer-6.1.2-en.php", URL: srv.URL + "/en.php"},
				{Name: "adminer-6.1.2.php", URL: srv.URL + "/full.php"},
			}})
		case "/full.php":
			w.Write([]byte("<?php // adminer full"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := releasesAPI
	releasesAPI = srv.URL + "/latest"
	defer func() { releasesAPI = old }()

	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	v, err := Install(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if v != "6.1.2" || Version(paths) != "6.1.2" || !Installed(paths) {
		t.Errorf("version %q / %q, installed %v", v, Version(paths), Installed(paths))
	}
	b, _ := os.ReadFile(filepath.Join(Dir(paths), "adminer.php"))
	if string(b) != "<?php // adminer full" {
		t.Errorf("downloaded the wrong asset: %q", b)
	}
	idx, _ := os.ReadFile(filepath.Join(Dir(paths), "index.php"))
	for _, want := range []string{"function adminer_object()", "extends Adminer\\Adminer", "Adminer\\DRIVER == 'mssql'", "'TrustServerCertificate' => true", "include __DIR__ . '/adminer.php';"} {
		if !strings.Contains(string(idx), want) {
			t.Errorf("index.php misses %q", want)
		}
	}
	// Installed: a second install downloads nothing.
	srv.Close()
	if _, err := Install(context.Background(), paths); err != nil {
		t.Errorf("reinstall should not hit the network: %v", err)
	}
	if err := Remove(paths); err != nil || Installed(paths) {
		t.Errorf("Remove: %v, installed %v", err, Installed(paths))
	}
}

func TestURL(t *testing.T) {
	if got := URL("test", 1433); got != "https://adminer.test/?mssql=127.0.0.1%3A1433&username=sa" {
		t.Errorf("URL = %s", got)
	}
}
