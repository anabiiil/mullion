package app

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"pm/internal/agent"
	"pm/internal/config"
	"pm/internal/dnsd"
	"pm/internal/sysproc"
)

// WildcardAlias is the alias entry that serves every subdomain of a site.
const WildcardAlias = "*"

var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeAlias validates one subdomain label for the site host (e.g.
// "shop.test") and returns its canonical form: lowercase, no surrounding
// dots, and — as a convenience — with a pasted ".<host>" suffix removed
// ("api.shop.test" -> "api"). Nested labels ("v1.api") are fine; the
// wildcard is exactly "*".
func NormalizeAlias(label, host string) (string, error) {
	l := strings.Trim(strings.ToLower(strings.TrimSpace(label)), ".")
	if host != "" {
		l = strings.TrimSuffix(l, "."+strings.ToLower(host))
	}
	if l == WildcardAlias {
		return l, nil
	}
	if l == "" {
		return "", fmt.Errorf("empty subdomain")
	}
	for _, part := range strings.Split(l, ".") {
		if !dnsLabelRE.MatchString(part) {
			return "", fmt.Errorf("invalid subdomain %q (letters, digits and dashes per label, e.g. \"api\" or \"v1.api\"; or \"*\" for every subdomain)", label)
		}
	}
	if len(l)+1+len(host) > 253 {
		return "", fmt.Errorf("subdomain %q makes the hostname longer than 253 characters", label)
	}
	return l, nil
}

// normalizeAliases validates, dedupes and sorts a site's alias list.
func normalizeAliases(aliases []string, host string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range aliases {
		l, err := NormalizeAlias(raw, host)
		if err != nil {
			return nil, err
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

// SiteHosts lists every hostname a site answers on, for display: the
// apex first, then explicit aliases (sorted), then "*.<name>.<tld>" when
// the site has the wildcard alias.
func SiteHosts(s config.Site, tld string) []string {
	host := s.Name + "." + tld
	out := []string{host}
	out = append(out, explicitAliasHosts(s, host)...)
	if hasWildcard(s) {
		out = append(out, "*."+host)
	}
	return out
}

// explicitAliasHosts are the non-wildcard alias hostnames — the ones a
// hosts file can carry.
func explicitAliasHosts(s config.Site, host string) []string {
	var out []string
	for _, l := range s.Aliases {
		if l != WildcardAlias && l != "" {
			out = append(out, l+"."+host)
		}
	}
	sort.Strings(out)
	return out
}

func hasWildcard(s config.Site) bool {
	for _, l := range s.Aliases {
		if l == WildcardAlias {
			return true
		}
	}
	return false
}

// SetSiteAliases replaces a site's subdomain aliases (validated, deduped,
// sorted), saves, and reconverges (Caddyfile, hosts entries).
func (a *App) SetSiteAliases(name string, aliases []string) error {
	site := a.State.FindSite(name)
	if site == nil {
		return fmt.Errorf("no site named %q", name)
	}
	norm, err := normalizeAliases(aliases, a.State.Host(*site))
	if err != nil {
		return err
	}
	site.Aliases = norm
	return a.applyIfEnabled()
}

// Resolver hookup indirection, so tests can exercise SetWildcardDNS and
// SetTLD without touching /etc/resolver or NRPT.
var (
	installResolver   = dnsd.InstallResolver
	removeResolver    = dnsd.RemoveResolver
	resolverInstalled = dnsd.ResolverInstalled
)

// SetWildcardDNS turns Mullion's local DNS for *.<tld> on or off: it
// installs/removes the OS resolver hookup (one admin prompt), restarts
// the background agent (which hosts the DNS server) so it binds or
// releases the DNS ports, saves, and reconverges.
func (a *App) SetWildcardDNS(enable bool) error {
	tld := a.State.Config.TLD
	if enable {
		if err := installResolver(tld); err != nil {
			return err
		}
	} else if err := removeResolver(tld); err != nil {
		return err
	}
	a.State.Config.WildcardDNS = enable
	if a.skipApply {
		return a.State.Save()
	}
	if err := a.State.Save(); err != nil {
		return err
	}
	// The agent hosts the DNS server: restart it so it binds (or
	// releases) the DNS ports; stop it when nothing else needs it.
	if enable || a.hasNodeSites() {
		if err := agent.Restart(a.Paths); err != nil {
			return err
		}
	} else {
		agent.Stop(a.Paths)
	}
	return a.Apply()
}

// WildcardDNSStatus reports the saved setting, whether the OS resolver
// hookup is in place, whether the DNS server answers, and a human note
// on anything that needs attention ("" when all is well).
func (a *App) WildcardDNSStatus() (enabled bool, resolverInstalledNow bool, serverRunning bool, note string) {
	tld := a.State.Config.TLD
	enabled = a.State.Config.WildcardDNS
	resolverInstalledNow = resolverInstalled(tld)
	serverRunning = dnsd.Probe(dnsd.ProbeAddr(), "mullion-probe."+tld, 700*time.Millisecond) == nil
	if !enabled {
		if resolverInstalledNow {
			note = fmt.Sprintf("wildcard DNS is off but %s is still installed — run `mullion dns off` to remove it", dnsd.ResolverDescription(tld))
		}
		return
	}
	var notes []string
	if !resolverInstalledNow {
		notes = append(notes, fmt.Sprintf("%s is missing — run `mullion dns on` to install it", dnsd.ResolverDescription(tld)))
	}
	if !serverRunning {
		n := fmt.Sprintf("the DNS server is not answering on %s", dnsd.ProbeAddr())
		if runtime.GOOS == "windows" {
			if pid, proc := sysproc.PortOwner(53); pid > 0 && !strings.Contains(strings.ToLower(proc), "mullion") {
				n += fmt.Sprintf(" — port 53 is held by %s (PID %d; often Internet Connection Sharing or a local DNS proxy), and Windows' NRPT can only point at port 53", proc, pid)
			}
		}
		if !agent.Running() {
			n += " — the background agent is not running (`mullion start` brings it up)"
		}
		notes = append(notes, n)
	}
	note = strings.Join(notes, "; ")
	return
}

func (a *App) hasNodeSites() bool {
	for _, s := range a.State.Sites {
		if s.Kind == "node" {
			return true
		}
	}
	return false
}

// needsAgent reports whether the background agent must run: for
// wake-on-demand of node sites, or to host the wildcard DNS server.
func (a *App) needsAgent() bool {
	return a.hasNodeSites() || a.State.Config.WildcardDNS || a.hasWorkers()
}

// hasWorkers reports whether any site has supervised workers — the
// agent is what autostarts them and restarts them after a crash.
func (a *App) hasWorkers() bool {
	for _, s := range a.State.Sites {
		if len(s.Workers) > 0 {
			return true
		}
	}
	return false
}

// moveResolver re-points the OS resolver hookup from an old TLD to a new
// one (install first, so a declined prompt leaves the old one working).
func moveResolver(oldTLD, newTLD string) error {
	if oldTLD == newTLD {
		return nil
	}
	if err := installResolver(newTLD); err != nil {
		return err
	}
	return removeResolver(oldTLD)
}
