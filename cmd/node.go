package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/devserver"
	"pm/internal/nodever"
	"pm/internal/proc"
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
		a := mustApp()
		full, err := nodever.FindInstalled(a.Paths, args[0])
		if err != nil {
			return err
		}
		if full == a.State.Config.GlobalNode {
			return fmt.Errorf("Node %s is the default; switch first with `mullion node use <other>`", full)
		}
		for _, s := range a.State.Sites {
			if s.Node == full {
				return fmt.Errorf("site %q is pinned to Node %s; run `mullion node isolate <other>` there first", s.Name, full)
			}
		}
		if err := os.RemoveAll(a.Paths.NodeVersionDir(full)); err != nil {
			return err
		}
		fmt.Println("Removed Node", full)
		return nil
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
		a := mustApp()
		target := ""
		if len(args) == 2 {
			target = args[1]
		} else if a.State.Config.GlobalNode == "" {
			return fmt.Errorf("no Node installed yet (run: mullion node install lts)")
		} else {
			target = a.State.Config.GlobalNode
		}
		full, err := nodever.FindInstalled(a.Paths, target)
		if err != nil {
			return err
		}
		dir := a.Paths.NodeVersionDir(full)
		spec := "npm@" + strings.TrimPrefix(args[0], "npm@")
		fmt.Printf("Installing %s into Node %s...\n", spec, full)
		c := proc.Quiet(nodever.Tool(dir, "npm"), "install", "-g", spec)
		c.Env = append(os.Environ(), "PATH="+nodever.BinDir(dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
		fmt.Printf("Done — Node %s now ships that npm.\n", full)
		return nil
	},
}

var nodeWhichCmd = &cobra.Command{
	Use:   "which",
	Short: "Explain which Node version this directory gets, and why",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, reason, err := resolveNodeDirForCwd(a)
		if err != nil {
			return err
		}
		fmt.Printf("Here, Node %s runs — decided by %s.\n", filepath.Base(dir), reason)
		if g := a.State.Config.GlobalNode; g != "" {
			fmt.Printf("Global default: %s\n", g)
		}

		// Is the `node` on the PATH actually Mullion's shim?
		found, err := exec.LookPath("node")
		if err != nil {
			fmt.Println("warning: no `node` on this terminal's PATH — open a NEW terminal.")
			return nil
		}
		shim := filepath.Join(a.Paths.BinDir(), "node")
		if filepath.Clean(found) != filepath.Clean(shim) {
			fmt.Printf(`
WARNING: in THIS terminal `+"`node`"+` resolves to
  %s
which is NOT Mullion's — another Node install (nvm? Homebrew?) is
earlier on the PATH, so `+"`node -v`"+` here ignores Mullion entirely.
Fix: run `+"`mullion node use %s`"+` (re-asserts the PATH) and open a
NEW terminal.
`, found, filepath.Base(dir))
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
// the choice to a human.
func resolveNodeDirForCwd(a *app.App) (dir, reason string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	home, _ := os.UserHomeDir()
	for d := cwd; ; d = filepath.Dir(d) {
		if site := a.State.FindSiteByPath(d); site != nil {
			verDir, err := a.NodeVersionDirFor(*site)
			why := fmt.Sprintf("linked site %q", site.Name)
			if site.Node == "" {
				why += " (.nvmrc / global)"
			} else {
				why += " (pinned)"
			}
			return verDir, why, err
		}
		if data, err := os.ReadFile(filepath.Join(d, ".nvmrc")); err == nil {
			full, err := nodever.FindInstalled(a.Paths, strings.TrimSpace(string(data)))
			if err != nil {
				return "", "", err
			}
			return a.Paths.NodeVersionDir(full), filepath.Join(d, ".nvmrc"), nil
		}
		if d == home || filepath.Dir(d) == d {
			break
		}
	}
	full, err := nodever.FindInstalled(a.Paths, a.State.Config.GlobalNode)
	if err != nil {
		return "", "", err
	}
	return a.Paths.NodeVersionDir(full), "the global default", nil
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
