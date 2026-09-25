package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/postgres"
	"pm/internal/term"
)

var postgresCmd = &cobra.Command{
	Use:     "postgres",
	Aliases: []string{"pg"},
	Short:   "Manage the local PostgreSQL server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showPostgresStatus(mustApp())
	},
}

var postgresInstallCmd = &cobra.Command{
	Use:   "install [version]",
	Short: "Download PostgreSQL (newest " + postgres.DefaultSeries + " by default), initialize it, and start it",
	Long: `Installs and starts PostgreSQL. With no argument you get the newest
release of the ` + postgres.DefaultSeries + ` series. Pass "latest" for the newest release
overall, a major like "16" for the newest release of that major, or a
full version like "16.4".

Switching to a different major stops the previous server — data
directories are per-major, so the old data is left on disk untouched
(where it's kept is printed).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		fmt.Println("Resolving the PostgreSQL version...")
		version, err := a.InstallPostgres(cmd.Context(), arg)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", term.Green(fmt.Sprintf("✓ %s is running on 127.0.0.1:%d (user `%s`, no password unless you set one).",
			postgres.Label(version), postgres.Port, postgres.Superuser)))
		return nil
	},
}

var postgresStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the PostgreSQL server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StartPostgres(); err != nil {
			return err
		}
		fmt.Printf("%s is running on 127.0.0.1:%d.\n", postgres.Label(a.State.Config.Postgres), postgres.Port)
		return nil
	},
}

var postgresStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the PostgreSQL server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StopPostgres(); err != nil {
			return err
		}
		fmt.Println("PostgreSQL stopped.")
		return nil
	},
}

var postgresStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether PostgreSQL is installed and running",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showPostgresStatus(mustApp())
	},
}

func showPostgresStatus(a *app.App) error {
	v := a.State.Config.Postgres
	if v == "" {
		fmt.Println("PostgreSQL is not installed (run: mullion postgres install)")
		return nil
	}
	state := term.Red("stopped") + " (run `mullion postgres start`)"
	if postgres.Running() {
		state = term.Green("running")
	}
	pw := "no password set"
	if a.State.Config.PostgresPassword != "" {
		pw = "password set"
	}
	fmt.Printf("%-24s port %d  %s  (%s)\n", postgres.Label(v), postgres.Port, state, pw)
	return nil
}

var postgresPasswordCmd = &cobra.Command{
	Use:   "password [new-password]",
	Short: "Change the PostgreSQL superuser password ('' to remove it)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if a.State.Config.Postgres == "" {
			return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
		}
		var newPass string
		if len(args) == 1 {
			newPass = args[0]
		} else {
			if !console.Interactive() {
				return fmt.Errorf("pass the new password as an argument, e.g.: mullion postgres password secret")
			}
			fmt.Print("New superuser password (empty to remove): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			newPass = strings.TrimRight(line, "\r\n")
		}
		if err := a.SetPostgresPassword(newPass); err != nil {
			return err
		}
		if newPass == "" {
			fmt.Println("Superuser password removed.")
		} else {
			fmt.Println("Superuser password changed.")
		}
		return nil
	},
}

var postgresDbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage databases on the local PostgreSQL server",
}

var postgresDbListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your databases",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dbs, err := a.PostgresDatabases()
		if err != nil {
			return err
		}
		if len(dbs) == 0 {
			fmt.Println("No databases yet. Create one with: mullion postgres db create <name>")
			return nil
		}
		for _, db := range dbs {
			fmt.Println("  " + db)
		}
		return nil
	},
}

var postgresDbCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a database",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.CreatePostgresDB(args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s is ready.\n", args[0])
		return nil
	},
}

var postgresDbDropCmd = &cobra.Command{
	Use:   "drop <name>",
	Short: "Delete a database and ALL its data",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if console.Interactive() &&
			!askYesNo(fmt.Sprintf("Delete database %q and ALL its data?", args[0]), false) {
			fmt.Println("Aborted.")
			return nil
		}
		if err := a.DropPostgresDB(args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s dropped.\n", args[0])
		return nil
	},
}

var postgresBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Export all your PostgreSQL databases (and roles) to a timestamped backup folder",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, err := a.BackupEngine(cmd.Context(), "postgres")
		if err != nil {
			return err
		}
		fmt.Println("Backup saved:", dir)
		fmt.Println("  restore with: mullion postgres restore \"" + dir + "\"")
		return nil
	},
}

var postgresRestoreCmd = &cobra.Command{
	Use:   "restore <backup-folder-or-.dump-file> [database]",
	Short: "Restore a PostgreSQL backup",
	Long: `Restores databases into the running PostgreSQL server.

Pass a backup folder (as mullion postgres backup creates) to restore
every database in it, plus its globals.sql (roles, tablespaces) on a
best-effort basis; add a database name to restore just that one. Pass a
single .dump file with a database name to restore that file alone.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		path := strings.Trim(strings.TrimSpace(args[0]), `"`)
		db := ""
		if len(args) == 2 {
			db = args[1]
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s does not exist", path)
		}
		if info.IsDir() {
			if err := a.RestoreBackup(cmd.Context(), path, db); err != nil {
				return err
			}
			fmt.Println("Restored from", path)
			return nil
		}
		if db == "" {
			return fmt.Errorf("restoring a single .dump file needs a database name: mullion postgres restore %s <database>", path)
		}
		v := a.State.Config.Postgres
		if v == "" {
			return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
		}
		if err := postgres.Start(a.Paths, v); err != nil {
			return err
		}
		if err := postgres.RestoreFile(a.Paths, v, path, db); err != nil {
			return err
		}
		fmt.Printf("Restored %s into database %s.\n", filepath.Base(path), db)
		return nil
	},
}

var (
	postgresUninstallYes      bool
	postgresUninstallKeepData bool
	postgresUninstallNoBackup bool
)

var postgresUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop PostgreSQL, back it up, and remove it (binaries, data, and pgAdmin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		v := a.State.Config.Postgres
		if v == "" {
			return fmt.Errorf("PostgreSQL is not installed")
		}
		question := fmt.Sprintf("Uninstall %s", postgres.Label(v))
		if postgresUninstallKeepData {
			question += " (binaries and pgAdmin only — your databases are kept)?"
		} else {
			question += " AND delete all its databases?"
		}
		proceed, err := confirmDestructive(postgresUninstallYes, question)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		dir, err := a.UninstallEngine(cmd.Context(), "postgres", !postgresUninstallNoBackup, !postgresUninstallKeepData)
		if err != nil {
			return err
		}
		if dir != "" {
			fmt.Println("Backup saved:", dir)
		}
		fmt.Println("PostgreSQL removed.")
		return nil
	},
}

func init() {
	postgresUninstallCmd.Flags().BoolVar(&postgresUninstallYes, "yes", false, "do not ask for confirmation")
	postgresUninstallCmd.Flags().BoolVar(&postgresUninstallKeepData, "keep-data", false, "keep the data directory (binaries only)")
	postgresUninstallCmd.Flags().BoolVar(&postgresUninstallNoBackup, "no-backup", false, "skip the database backup")
	postgresDbCmd.AddCommand(postgresDbListCmd, postgresDbCreateCmd, postgresDbDropCmd)
	postgresCmd.AddCommand(postgresInstallCmd, postgresBackupCmd, postgresRestoreCmd, postgresStartCmd, postgresStopCmd,
		postgresStatusCmd, postgresPasswordCmd, postgresDbCmd, postgresUninstallCmd)
	rootCmd.AddCommand(postgresCmd)
}
