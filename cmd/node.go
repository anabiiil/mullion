package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/nodever"
)

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Manage Node.js versions (official nodejs.org builds)",
}

var nodeAvailableCmd = &cobra.Command{
	Use:   "available",
	Short: "List installable Node versions (newest of each major)",
	RunE: func(cmd *cobra.Command, args []string) error {
		all, err := nodever.FetchAll(cmd.Context())
		if err != nil {
			return err
		}
		seen := map[int]bool{}
		fmt.Println("Newest release of each Node major on nodejs.org:")
		count := 0
		for _, r := range all { // newest first
			sel, err := nodever.ParseSelector(r.Version)
			if err != nil || seen[sel.Major] {
				continue
			}
			seen[sel.Major] = true
			tag := ""
			if r.LTS != "" {
				tag = "  (LTS " + r.LTS + ")"
			}
			fmt.Printf("  %s%s\n", r.Version, tag)
			if count++; count >= 8 {
				break
			}
		}
		fmt.Println("\nInstall with: mullion node install lts | latest | 22 | 22.12.0")
		return nil
	},
}

var nodeInstallCmd = &cobra.Command{
	Use:   "install [version]",
	Short: "Download and install a Node version (lts by default; latest, 22, 22.12.0)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		arg := "lts"
		if len(args) == 1 {
			arg = args[0]
		}
		rel, err := nodever.Resolve(cmd.Context(), arg)
		if err != nil {
			return err
		}
		dir, err := nodever.Install(cmd.Context(), a.Paths, rel)
		if err != nil {
			return err
		}
		fmt.Printf("Node %s installed at %s\n", rel.Version, dir)
		// First install becomes the global default right away.
		if a.State.Config.GlobalNode == "" {
			if err := activateNode(a, rel.Version); err != nil {
				return err
			}
			fmt.Printf("Node %s is now the default (`node -v` in any NEW terminal).\n", rel.Version)
		} else {
			fmt.Printf("Make it the default with: mullion node use %s\n", rel.Version)
		}
		return nil
	},
}

var nodeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed Node versions (* = global default)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		versions, err := nodever.Installed(a.Paths)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			fmt.Println("No Node versions installed yet. Try: mullion node install lts")
			return nil
		}
		for _, v := range versions {
			marker := "  "
			if v == a.State.Config.GlobalNode {
				marker = "* "
			}
			fmt.Println(marker + v)
		}
		return nil
	},
}

var nodeUseCmd = &cobra.Command{
	Use:   "use <version>",
	Short: "Switch the default Node version (node/npm on PATH)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		full, err := nodever.FindInstalled(a.Paths, args[0])
		if err != nil {
			return err
		}
		if err := activateNode(a, full); err != nil {
			return err
		}
		fmt.Printf("Default Node is now %s (`node -v` in any NEW terminal)\n", full)
		if found, err := exec.LookPath("node"); err == nil {
			shim := filepath.Join(a.Paths.BinDir(), "node")
			if filepath.Clean(found) != filepath.Clean(shim) {
				fmt.Printf("\nNote: in THIS terminal `node` still resolves to %s (another install).\n", found)
				offerShadowFix(found)
				fmt.Println("Open a NEW terminal for Mullion's node to take over.")
			}
		}
		return nil
	},
}

var nodeUninstallCmd = &cobra.Command{
	Use:   "uninstall <version>",
	Short: "Remove an installed Node version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mustApp().UninstallNode(args[0])
	},
}

var nodeIsolateCmd = &cobra.Command{
	Use:   "isolate <version>",
	Short: "Pin the current project's site to a specific Node version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		full, err := nodever.FindInstalled(a.Paths, args[0])
		if err != nil {
			return err
		}
		site, err := currentNodeSite(a)
		if err != nil {
			return err
		}
		site.Node = full
		// Restart the dev server on the new version.
		devserver.Stop(a.Paths, site.Name)
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s now runs on Node %s\n", a.State.Host(*site), full)
		return nil
	},
}

var nodeUnisolateCmd = &cobra.Command{
	Use:   "unisolate",
	Short: "Make the current project's site follow .nvmrc / the default Node again",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site, err := currentNodeSite(a)
		if err != nil {
			return err
		}
		site.Node = ""
		devserver.Stop(a.Paths, site.Name)
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s now follows .nvmrc / the default Node\n", a.State.Host(*site))
		return nil
	},
}

var nodeNpmCmd = &cobra.Command{
	Use:   "npm <npm-version> [node-version]",
	Short: "Change the npm version bundled with a Node install (latest, 10, 10.9.2)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		nodeVersion := ""
		if len(args) == 2 {
			nodeVersion = args[1]
		}
		return mustApp().SetNpmVersion(cmd.Context(), args[0], nodeVersion)
	},
}

var nodeWhichCmd = &cobra.Command{
	Use:   "which",
	Short: "Explain which Node version this directory gets, and why",
	RunE: func(cmd *cobra.Command, args []string) error {
		info := mustApp().NodeInfo()
		if info.Error != "" {
			return fmt.Errorf("%s", info.Error)
		}
		fmt.Printf("Here, Node %s runs — decided by %s.\n", info.Version, info.Reason)
		if info.GlobalDefault != "" {
			fmt.Printf("Global default: %s\n", info.GlobalDefault)
		}

		// Is the `node` on the PATH actually Mullion's shim?
		if info.Warning != "" {
			fmt.Println("warning:", info.Warning)
			return nil
		}
		if info.Shadowed {
			fmt.Printf(`
WARNING: in THIS terminal `+"`node`"+` resolves to
  %s
which is NOT Mullion's — another Node install (nvm? Homebrew?) is
earlier on the PATH, so `+"`node -v`"+` here ignores Mullion entirely.
Fix: run `+"`mullion node use %s`"+` (re-asserts the PATH) and open a
NEW terminal.
`, info.ResolvedPath, info.Version)
		}
		return nil
	},
}

// nodeBinCmd powers the node/npm/npx shims: it prints the absolute path
// of a tool in the version the CURRENT DIRECTORY should use (pinned
// site version → .nvmrc → global default). Hidden — not for humans.
var nodeBinCmd = &cobra.Command{
	Use:    "bin <tool>",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, _, err := resolveNodeDirForCwd(a)
		if err != nil {
			return err
		}
		fmt.Println(nodever.Tool(dir, filepath.Base(args[0])))
		return nil
	},
}

// resolveNodeDirForCwd resolves the Node version directory for the
// current directory: a linked site's pin, a .nvmrc walking up (stopping
// at the home directory), then the global default. The reason explains
// the choice to a human. The core logic lives on App (ResolveNodeForCwd)
// so the control panel can use it too.
func resolveNodeDirForCwd(a *app.App) (dir, reason string, err error) {
	return a.ResolveNodeForCwd()
}

// activateNode makes a version the default (junction, shims, PATH).
func activateNode(a *app.App, fullVersion string) error {
	return a.ActivateNode(fullVersion)
}

// currentNodeSite resolves the node site linked to the current directory.
func currentNodeSite(a *app.App) (*config.Site, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	site := a.State.FindSiteByPath(dir)
	if site == nil {
		return nil, fmt.Errorf("current directory is not a linked site; run `mullion link` first")
	}
	if site.Kind != "node" {
		return nil, fmt.Errorf("%s is not a Node site — for PHP versions use `mullion isolate`", site.Name)
	}
	return site, nil
}

func init() {
	nodeCmd.AddCommand(nodeAvailableCmd, nodeInstallCmd, nodeListCmd, nodeUseCmd, nodeWhichCmd,
		nodeUninstallCmd, nodeIsolateCmd, nodeUnisolateCmd, nodeNpmCmd, nodeBinCmd)
	rootCmd.AddCommand(nodeCmd)
}
