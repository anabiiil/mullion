package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/devserver"
)

var renameCmd = &cobra.Command{
	Use:   "rename [old] <new>",
	Short: "Rename a site — its domain becomes <new>.<tld>",
	Long: `Renames a linked site (and therefore its domain). With one argument
the site linked to the current directory is renamed; with two, the
first names the site to rename.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		var oldName, newName string
		if len(args) == 2 {
			oldName, newName = slugify(args[0]), args[1]
		} else {
			n, err := siteNameFromArgOrCwd(a, nil)
			if err != nil {
				return err
			}
			oldName, newName = n, args[0]
		}
		site := a.State.FindSite(oldName)
		if site == nil {
			return fmt.Errorf("no site named %q", oldName)
		}
		if site.Kind == "node" {
			devserver.Stop(a.Paths, site.Name)
		}
		if err := a.State.RenameSite(oldName, newName); err != nil {
			return err
		}
		if err := a.Apply(); err != nil {
			return err
		}
		site = a.State.FindSite(slugify(newName))
		scheme := "http"
		if site != nil && site.Secure {
			scheme = "https"
		}
		fmt.Printf("Renamed %s -> %s://%s\n", oldName, scheme, a.State.Host(*site))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(renameCmd)
}
