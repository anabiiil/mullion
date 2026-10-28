package app

// EXPLAIN for the Watch tab's queries (and `mullion watch <site>
// explain`): which database the query ran on (the project's .env), the
// site's own PHP to run the explain with, and the explain package for
// the rest.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"pm/internal/phpver"
	"pm/internal/redis"
	"pm/internal/watch/explain"
)

// WatchExplainInput is one explain request from the panel or the CLI.
type WatchExplainInput struct {
	explain.Input
	// Conn is the Laravel connection name the watcher recorded ("" =
	// the default).
	Conn string `json:"conn"`
}

// explainRunner builds the runner for a site; tests swap it.
var explainRunner = func(a *App, phpExe, ini string) (explain.Runner, error) {
	script, err := explain.EnsureScript(a.Paths.WatchDir())
	if err != nil {
		return nil, err
	}
	return explain.PHPRunner(phpExe, ini, script), nil
}

// explainSlots bounds concurrent explains (auto-explain can ask for a
// few at once): each one is a PHP process and a database connection.
var explainSlots = make(chan struct{}, 3)

// WatchExplain explains one query of a site.
func (a *App) WatchExplain(ctx context.Context, site string, in WatchExplainInput) (*explain.Result, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, fmt.Errorf("no site named %q", site)
	}
	if !s.IsPHP() {
		return nil, errors.New("explain needs a PHP site (it runs through the site's PHP)")
	}
	data, err := os.ReadFile(filepath.Join(s.Path, ".env"))
	if err != nil {
		return nil, errors.New("the project has no .env, so Mullion can't tell which database it uses")
	}
	c, notes, err := explain.ConnFromEnv(redis.ParseEnv(string(data)), s.Path, in.Conn)
	if err != nil {
		return nil, err
	}
	v := a.SiteVersion(*s)
	if v == "" {
		return nil, fmt.Errorf("no PHP version resolved for %s", s.Name)
	}
	dir := a.Paths.PhpVersionDir(v)
	phpExe := filepath.Join(dir, phpver.PhpExeName)
	if _, err := os.Stat(phpExe); err != nil {
		return nil, fmt.Errorf("PHP %s is not installed (it runs the explain for %s)", v, s.Name)
	}
	run, err := explainRunner(a, phpExe, filepath.Join(dir, "php.ini"))
	if err != nil {
		return nil, err
	}
	select {
	case explainSlots <- struct{}{}:
		defer func() { <-explainSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	res, err := explain.Explain(ctx, run, c, in.Input)
	if err != nil {
		return nil, err
	}
	res.Notes = append(notes, res.Notes...)
	return res, nil
}
