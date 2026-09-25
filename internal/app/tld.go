package app

import (
	"fmt"
	"strings"

	"pm/internal/config"
)

// ValidateTLD normalizes a proposed domain suffix (lowercase, trims any
// leading/trailing dots) and checks it contains only letters, digits and
// dashes. It is pure — no config or filesystem access — so both the CLI
// and the control panel can validate before touching anything.
func ValidateTLD(tld string) (string, error) {
	t := strings.Trim(strings.ToLower(strings.TrimSpace(tld)), ".")
	if t == "" || t != config.Slugify(t) {
		return "", fmt.Errorf("invalid TLD %q (letters, digits and dashes only)", tld)
	}
	return t, nil
}

// SetTLD validates tld and switches every linked site to it: it updates
// the saved config and reconverges the machine (Caddyfile, hosts file,
// certificates) so sites resolve under the new suffix immediately. With
// wildcard DNS on, the OS resolver hookup moves to the new TLD too (the
// agent's DNS server picks the new zone up from config by itself).
func (a *App) SetTLD(tld string) error {
	t, err := ValidateTLD(tld)
	if err != nil {
		return err
	}
	if a.State.Config.WildcardDNS {
		if err := moveResolver(a.State.Config.TLD, t); err != nil {
			return err
		}
	}
	a.State.Config.TLD = t
	return a.applyIfEnabled()
}
