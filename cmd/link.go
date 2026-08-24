package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/nodever"
)

var (
	linkSecure   bool
	linkBuild    bool
	linkBuildDir string
	linkPHP      bool
)

var linkCmd = &cobra.Command{
	Use:   "link [name]",
	Short: "Serve the current directory at http://<name>.<tld>",
	Long: `Registers the current directory as a site. The name defaults to the
directory name.

The project type is detected automatically:
  - PHP projects are served through PHP (Laravel public/ auto-detected)
  - Frontend projects (a package.json with a dev script) get a managed
    dev server: Mullion runs ` + "`npm run dev`" + ` for you and proxies the
    .test domain to it — opening the link is all you do

With --build, the project's LAST PRODUCTION BUILD (dist/, build/, out/)
is served as a separate static site named <name>-build — handy to
compare the deployed state against the current dev version.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, err := os.Getwd()
		if err != nil {
			return err
		}

		kind, buildDir := "", ""
		switch {
		case linkBuild:
			kind = "static"
			buildDir, err = a.EnsureBuildOutput(dir, linkBuildDir)
			if err != nil {
				return err
			}
		case linkPHP:
			kind = "php"
		default:
			kind = app.DetectProjectKind(dir)
		}

		name := filepath.Base(dir)
		if len(args) == 1 {
			name = args[0]
		} else if kind == "static" {
			name = name + "-build"
		}
		name = slugify(name)
		if name == "" {
			return fmt.Errorf("could not derive a valid site name; pass one explicitly: mullion link myapp")
		}
		if existing := a.State.FindSite(name); existing != nil {
			return fmt.Errorf("site %q already links to %s (use `mullion unlink %s` first)", name, existing.Path, name)
		}

		site := config.Site{Name: name, Path: dir, Kind: kind, BuildDir: buildDir, Secure: linkSecure}
		if kind == "node" {
			site.DevPort = devserver.AssignPort(a.State.Sites)
			if err := ensureAnyNode(cmd, a); err != nil {
				return err
			}
		}
		a.State.AddSite(site)
		if err := a.Apply(); err != nil {
			return err
		}
		scheme := "http"
		if linkSecure {
			scheme = "https"
		}
		switch kind {
		case "node":
			fmt.Printf("Linked %s -> %s://%s.%s (managed dev server — just open the link)\n", dir, scheme, name, a.State.Config.TLD)
		case "static":
			fmt.Printf("Linked %s/%s -> %s://%s.%s (static build)\n", dir, buildDir, scheme, name, a.State.Config.TLD)
		default:
			fmt.Printf("Linked %s -> %s://%s.%s\n", dir, scheme, name, a.State.Config.TLD)
		}
		return nil
	},
}

// ensureAnyNode makes sure at least one Node version exists before a
// node site is linked, installing the LTS automatically if none is.
func ensureAnyNode(cmd *cobra.Command, a *app.App) error {
	if versions, _ := nodever.Installed(a.Paths); len(versions) > 0 {
		return nil
	}
	fmt.Println("No Node installed yet — installing the LTS release...")
	rel, err := nodever.Resolve(cmd.Context(), "lts")
	if err != nil {
		return err
	}
	if _, err := nodever.Install(cmd.Context(), a.Paths, rel); err != nil {
		return err
	}
	return activateNode(a, rel.Version)
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
		devserver.Stop(a.Paths, name)
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
		fmt.Printf("%-20s %-8s %-8s %-18s %s\n", "SITE", "TLS", "KIND", "RUNTIME", "PATH")
		for _, s := range a.State.Sites {
			tls := "http"
			if s.Secure {
				tls = "https"
			}
			kind, runtime := "php", ""
			switch s.Kind {
			case "node":
				kind = "node"
				runtime = s.Node
				if runtime == "" {
					runtime = ".nvmrc/global"
				}
			case "static":
				kind = "static"
				runtime = s.BuildDir
			default:
				runtime = s.PHP
				if runtime == "" {
					runtime = a.State.Config.GlobalPHP + " (global)"
				}
			}
			fmt.Printf("%-20s %-8s %-8s %-18s %s\n", a.State.Host(s), tls, kind, runtime, s.Path)
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
	linkCmd.Flags().BoolVar(&linkBuild, "build", false, "serve the project's production build as a static site (<name>-build)")
	linkCmd.Flags().StringVar(&linkBuildDir, "dir", "", "build output folder for --build (auto-detected: dist, build, out)")
	linkCmd.Flags().BoolVar(&linkPHP, "php", false, "force serving as a PHP site")
	rootCmd.AddCommand(linkCmd, unlinkCmd, linksCmd)
}
