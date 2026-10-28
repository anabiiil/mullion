package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/phpver"
	"pm/internal/watch/explain"
)

func TestWatchExplainResolvesConnAndPHP(t *testing.T) {
	a := sandboxApp(t)
	proj := t.TempDir()
	a.State.Config.GlobalPHP = "8.4.23"
	a.State.AddSite(config.Site{Name: "blog", Path: proj})
	a.State.AddSite(config.Site{Name: "front", Path: t.TempDir(), Kind: "node"})

	// No .env yet.
	if _, err := a.WatchExplain(context.Background(), "blog", WatchExplainInput{Input: explain.Input{SQL: "select 1"}}); err == nil || !strings.Contains(err.Error(), ".env") {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.WatchExplain(context.Background(), "front", WatchExplainInput{}); err == nil {
		t.Fatal("a Node site has no PHP to explain with")
	}
	os.WriteFile(filepath.Join(proj, ".env"), []byte("DB_CONNECTION=mysql\nDB_PORT=33999\nDB_DATABASE=blog\nDB_PASSWORD=\"hunter2\"\n"), 0o644)
	// PHP not installed.
	if _, err := a.WatchExplain(context.Background(), "blog", WatchExplainInput{Input: explain.Input{SQL: "select 1"}}); err == nil || !strings.Contains(err.Error(), "PHP 8.4.23") {
		t.Fatalf("err = %v", err)
	}
	phpDir := a.Paths.PhpVersionDir("8.4.23")
	os.MkdirAll(phpDir, 0o755)
	os.WriteFile(filepath.Join(phpDir, phpver.PhpExeName), []byte(""), 0o755)

	var gotPHP, gotIni string
	var sent map[string]any
	old := explainRunner
	t.Cleanup(func() { explainRunner = old })
	explainRunner = func(a *App, phpExe, ini string) (explain.Runner, error) {
		gotPHP, gotIni = phpExe, ini
		return func(ctx context.Context, req []byte) ([]byte, error) {
			json.Unmarshal(req, &sent)
			return []byte(`{"ok":true,"driver":"mysql","server":"8.4.11","sql":"select * from posts where user_id = 1",
				"explain":{"tabular":[{"id":1,"select_type":"SIMPLE","table":"posts","type":"ALL","possible_keys":null,"key":null,"rows":5000,"filtered":10,"Extra":"Using where"}],"json":"{}"},
				"tables":{"posts":{"rows":5000,"indexes":[{"name":"PRIMARY","unique":true,"primary":true,"columns":["id"]}],"columns":{"id":"bigint","user_id":"bigint"}}}}`), nil
		}, nil
	}
	res, err := a.WatchExplain(context.Background(), "blog", WatchExplainInput{Conn: "pgsql", Input: explain.Input{SQL: "select * from `posts` where `user_id` = ?", Bindings: []any{1.0}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPHP != filepath.Join(phpDir, phpver.PhpExeName) || gotIni != filepath.Join(phpDir, "php.ini") {
		t.Errorf("php = %s ini = %s", gotPHP, gotIni)
	}
	conn := sent["conn"].(map[string]any)
	if conn["driver"] != "pgsql" || conn["port"] != 33999.0 || conn["database"] != "blog" || conn["password"] != "hunter2" {
		t.Errorf("conn = %v", conn)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], `"pgsql" connection`) {
		t.Errorf("notes = %v", res.Notes)
	}
	if len(res.Suggestions) != 1 || res.Suggestions[0].SQL != "CREATE INDEX idx_posts_user_id ON posts (user_id);" {
		t.Errorf("suggestions = %+v", res.Suggestions)
	}
	if b, _ := json.Marshal(res); strings.Contains(string(b), "hunter2") {
		t.Error("the result leaks the password")
	}
}
