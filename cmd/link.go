package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
)

var linkSecure bool

var linkCmd = &cobra.Command{
	Use:   "link [name]",
	Short: "Serve the current directory at http://<name>.<tld>",
	Long: `Registers the current directory as a site. The name defaults to the
directory name. Laravel-style projects are served from their public/ folder.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		name := filepath.Base(dir)
		if len(args) == 1 {
			name = args[0]
		}
		name = slugify(name)
		if name == "" {
			return fmt.Errorf("could not derive a valid site name; pass one explicitly: mullion link myapp")
		}
		if existing := a.State.FindSite(name); existing != nil {
			return fmt.Errorf("site %q already links to %s (use `mullion unlink %s` first)", name, existing.Path, name)
		}

		a.State.AddSite(config.Site{Name: name, Path: dir, Secure: linkSecure})
		if err := a.Apply(); err != nil {
			return err
		}
		scheme := "http"
		if linkSecure {
			scheme = "https"
		}
		fmt.Printf("Linked %s -> %s://%s.%s\n", dir, scheme, name, a.State.Config.TLD)
		return nil
	},
}

var unlinkCmd = &cobra.Command{
	Use:   "unlink [name]",
	Short: "Stop serving a site (defaults to the current directory's link)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name, err := siteNameFromArgOrCwd(a, args)
		if err != nil {
			return err
		}
		if !a.State.RemoveSite(name) {
			return fmt.Errorf("no site named %q", name)
		}
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Println("Unlinked", name)
		return nil
	},
}

var linksCmd = &cobra.Command{
	Use:     "links",
	Aliases: []string{"sites"},
	Short:   "List linked sites",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if len(a.State.Sites) == 0 {
			fmt.Println("No sites linked yet. Run `mullion link` inside a project.")
			return nil
		}
		fmt.Printf("%-20s %-8s %-10s %s\n", "SITE", "TLS", "PHP", "PATH")
		for _, s := range a.State.Sites {
			tls := "http"
			if s.Secure {
				tls = "https"
			}
			php := s.PHP
			if php == "" {
				php = a.State.Config.GlobalPHP + " (global)"
			}
			fmt.Printf("%-20s %-8s %-10s %s\n", a.State.Host(s), tls, php, s.Path)
		}
		return nil
	},
}

func slugify(name string) string { return config.Slugify(name) }

// siteNameFromArgOrCwd resolves which site a command targets: explicit
// argument first, else the site linked to the current directory.
func siteNameFromArgOrCwd(a *app.App, args []string) (string, error) {
	if len(args) == 1 {
		return slugify(args[0]), nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if site := a.State.FindSiteByPath(dir); site != nil {
		return site.Name, nil
	}
	return "", fmt.Errorf("current directory is not a linked site; pass a name or run `mullion link` first")
}

func init() {
	linkCmd.Flags().BoolVar(&linkSecure, "secure", false, "serve over https immediately")
	rootCmd.AddCommand(linkCmd, unlinkCmd, linksCmd)
}
