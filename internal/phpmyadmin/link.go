package phpmyadmin

import (
	"context"
	"fmt"

	"pm/internal/app"
	"pm/internal/caddy"
	"pm/internal/config"
)

// EnsureLinked installs phpMyAdmin (version "" = latest, or keep the one
// already installed), links it as a secured site, and converges the
// machine. It is the reusable core shared by `mullion phpmyadmin`,
// `mullion setup`, and the control panel's install/reinstall button —
// each of those previously duplicated (or would have duplicated) this
// exact install-link-apply-trust sequence.
func EnsureLinked(ctx context.Context, a *app.App, version string) (string, error) {
	if a.State.Config.GlobalPHP == "" {
		return "", fmt.Errorf("phpMyAdmin needs PHP: run `mullion php install 8.4` and `mullion use 8.4` first")
	}

	if err := Install(ctx, a.Paths, version, a.State.Config.MySQLPassword); err != nil {
		return "", err
	}
	if site := a.State.FindSite("phpmyadmin"); site == nil {
		a.State.AddSite(config.Site{Name: "phpmyadmin", Path: a.Paths.PhpMyAdminDir(), Secure: true})
	} else {
		site.Secure = true
	}
	if err := a.Apply(); err != nil {
		return "", err
	}
	// Make sure Caddy's local root CA is in the system trust store so
	// browsers show the padlock (one-time confirmation prompt). Not
	// fatal — phpMyAdmin is already linked and reachable either way.
	_ = caddy.TrustCA(a.Paths)

	return "https://phpmyadmin." + a.State.Config.TLD, nil
}
