package mongoexpress

import (
	"context"
	"errors"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/devserver"
)

// SiteName is the fixed site mongo-express is linked as: https://mongo.<tld>.
const SiteName = "mongo"

// Follow a moved MongoDB port: point start.js at the new URI and bounce
// a running mongo-express so it reconnects there. (Registered as a hook
// because this package imports app, not the other way around.)
func init() {
	app.OnEnginePortChange(func(a *app.App, engine string) error {
		if engine != "mongo" {
			return nil
		}
		changed, err := RefreshConfig(a.Paths)
		if err != nil || !changed {
			return err
		}
		if devserver.Running(a.Paths, SiteName) > 0 {
			return a.RestartDevServer(SiteName)
		}
		return nil
	})
}

// Ensure installs mongo-express (a no-op once it already is) and links
// it as a secured node site named "mongo", converging the machine so
// https://mongo.<tld> serves it. It returns that URL.
//
// Safe to call repeatedly: re-running neither reinstalls mongo-express
// nor duplicates the site. It lives here (rather than in cmd) so both
// `mullion mongo-express` and, later, the control panel can share the
// exact same install-and-link logic without the panel importing cmd.
func Ensure(ctx context.Context, a *app.App) (string, error) {
	// Same resolution every other node site gets: the site's pinned
	// version, else its .nvmrc, else the global default, else (since
	// mongo-express's own folder has none of those) the newest Node
	// installed.
	nodeDir, err := a.NodeVersionDirFor(config.Site{Path: Dir(a.Paths)})
	if err != nil {
		return "", errors.New("mongo-express needs Node — run `mullion node install lts` first")
	}

	if err := Install(ctx, a.Paths, nodeDir); err != nil {
		return "", err
	}

	if site := a.State.FindSite(SiteName); site == nil {
		a.State.AddSite(config.Site{
			Name:    SiteName,
			Path:    Dir(a.Paths),
			Kind:    "node",
			Secure:  true,
			DevPort: devserver.AssignPort(a.State.Sites),
		})
	}

	if err := a.Apply(); err != nil {
		return "", err
	}

	return "https://" + SiteName + "." + a.State.Config.TLD, nil
}
