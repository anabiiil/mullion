package cmd

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/spf13/cobra"

	"pm/internal/pgadmin"
	"pm/internal/postgres"
)

var pgadminCmd = &cobra.Command{
	Use:   "pgadmin",
	Short: "Install (if needed) and open pgAdmin 4 for the local PostgreSQL",
	Long: `Installs pgAdmin 4 into ~/.mullion/pgadmin on first use — taken from the
same EDB package as the installed PostgreSQL — and opens it with a
"Mullion (PostgreSQL)" server (127.0.0.1:5432, user postgres) already
registered, so connecting is one click.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if !pgadmin.Installed(a.Paths) {
			versions := postgres.Installed(a.Paths)
			if len(versions) == 0 {
				return fmt.Errorf("PostgreSQL is not installed — run `mullion postgres install` first")
			}
			// pgAdmin ships inside the server's package: take the newest.
			if err := pgadmin.Install(cmd.Context(), a.Paths, versions[len(versions)-1]); err != nil {
				return err
			}
			fmt.Printf("pgAdmin 4 %s installed in %s (%s)\n", pgadmin.Version(a.Paths),
				pgadmin.Dir(a.Paths), pgadminSize(pgadmin.Dir(a.Paths)))
		}
		if err := pgadmin.Launch(a.Paths); err != nil {
			return err
		}
		fmt.Printf("pgAdmin 4 opened — use the %q server under %q.\n", pgadmin.ServerName, pgadmin.GroupName)
		return nil
	},
}

// pgadminSize renders a directory's total file size ("782 MB").
func pgadminSize(dir string) string {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return fmt.Sprintf("%.0f MB", float64(total)/1e6)
}

var pgadminUninstallYes bool

var pgadminUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove pgAdmin 4 (your saved servers in ~/.pgadmin are untouched)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		proceed, err := confirmDestructive(pgadminUninstallYes, "Remove pgAdmin 4?")
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		if err := a.UninstallAdminTool("pgadmin"); err != nil {
			return err
		}
		fmt.Println("pgAdmin 4 removed.")
		return nil
	},
}

func init() {
	pgadminUninstallCmd.Flags().BoolVar(&pgadminUninstallYes, "yes", false, "do not ask for confirmation")
	pgadminCmd.AddCommand(pgadminUninstallCmd)
	rootCmd.AddCommand(pgadminCmd)
}
