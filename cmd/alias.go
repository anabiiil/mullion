package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
)

var aliasCmd = &cobra.Command{
	Use:   "alias <site> [add|remove|list] [subdomain]",
	Short: "Serve a site on extra subdomains (api.<site>.test, or * for all)",
	Long: `Give a site extra subdomains, served by the same project:

  mullion alias shop add api       # api.shop.test
  mullion alias shop add v1.api    # v1.api.shop.test
  mullion alias shop add '*'       # every subdomain: tenant1.shop.test, ...
  mullion alias shop remove api
  mullion alias shop list

PHP apps see the real Host header (Laravel subdomain routing works);
frontend dev servers get it in X-Forwarded-Host.

Named subdomains resolve through the hosts file. The '*' wildcard needs
Mullion's local DNS — enable it once with ` + "`mullion dns on`" + `.`,
	Args: cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site := a.State.FindSite(args[0])
		if site == nil {
			return fmt.Errorf("no site named %q", args[0])
		}
		action := "list"
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "list", "ls":
			if len(args) > 2 {
				return fmt.Errorf("list takes no subdomain")
			}
			for _, h := range app.SiteHosts(*site, a.State.Config.TLD) {
				fmt.Println(h)
			}
			return nil
		case "add", "remove", "rm":
		default:
			return fmt.Errorf("unknown action %q (use add, remove or list)", action)
		}
		if len(args) < 3 {
			return fmt.Errorf("%s needs a subdomain, e.g. `mullion alias %s %s api`", action, site.Name, action)
		}
		label, err := app.NormalizeAlias(args[2], a.State.Host(*site))
		if err != nil {
			return err
		}
		name := site.Name
		next := []string{}
		found := false
		for _, l := range site.Aliases {
			if l == label {
				found = true
				if action != "add" {
					continue
				}
			}
			next = append(next, l)
		}
		if action == "add" {
			if found {
				fmt.Printf("%s already has %s\n", name, label)
				return nil
			}
			next = append(next, label)
		} else if !found {
			return fmt.Errorf("%s has no subdomain %q (have: %s)", name, label, strings.Join(site.Aliases, ", "))
		}
		if err := a.SetSiteAliases(name, next); err != nil {
			return err
		}
		host := a.State.Host(*a.State.FindSite(name))
		full := label + "." + host
		if action == "add" {
			fmt.Printf("%s now also answers on %s\n", name, full)
			if label == app.WildcardAlias && !a.State.Config.WildcardDNS {
				fmt.Println("note: wildcard subdomains need Mullion's local DNS — run `mullion dns on` once (asks for administrator rights).")
			}
		} else {
			fmt.Printf("Removed %s from %s\n", full, name)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(aliasCmd)
}
