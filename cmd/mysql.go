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
	"pm/internal/mysql"
	"pm/internal/phpmyadmin"
	"pm/internal/term"
)

var mysqlCmd = &cobra.Command{
	Use:   "mysql",
	Short: "Manage the local MySQL server",
}

var mysqlInstallCmd = &cobra.Command{
	Use:   "install [version]",
	Short: "Download MySQL (newest " + mysql.DefaultSeries + " LTS by default), initialize it, and start it",
	Long: `Installs and starts MySQL. With no argument you get the newest release
of the ` + mysql.DefaultSeries + ` LTS branch. Pass "latest" for the newest release overall,
a branch like "8.0" for the newest release of that branch, or a full
version like "8.4.11".

When you switch versions, your databases are migrated automatically:
they are dumped from the old server, the new one starts with a fresh
data directory, and the dump is restored into it. The old data
directory and the dump file are kept as backups.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		return switchDatabase(cmd, arg)
	},
}

// switchDatabase resolves the version argument, asks about migration
// when a human is present, and hands over to the shared switch logic.
func switchDatabase(cmd *cobra.Command, arg string) error {
	a := mustApp()
	fmt.Println("Resolving the database version...")
	version, err := mysql.ResolveVersionArg(cmd.Context(), arg)
	if err != nil {
		return err
	}

	migrate := true
	if a.State.Config.MySQL != "" && a.State.Config.MySQL != version &&
		mysql.DataInitialized(a.Paths) && console.Interactive() {
		migrate = askYesNo(fmt.Sprintf("Migrate your databases from %s to %s?",
			mysql.Label(a.State.Config.MySQL), mysql.Label(version)), true)
		if !migrate {
			fmt.Println("Skipping migration — the old data directory is kept as a backup.")
		}
	}

	if _, err := app.SwitchDatabase(cmd.Context(), a, version, migrate); err != nil {
		return err
	}
	fmt.Printf("%s\n", term.Green(fmt.Sprintf("✓ %s is running on 127.0.0.1:%d (user `root`, empty password).", mysql.Label(version), mysql.Port)))
	return nil
}

var mariadbCmd = &cobra.Command{
	Use:   "mariadb [version]",
	Short: "Switch the database server to MariaDB (newest stable by default)",
	Long: `Installs MariaDB and makes it the active database server, migrating
your databases from the current server (MySQL or MariaDB) after asking.
Equivalent to: mullion mysql install mariadb`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		arg := "mariadb"
		if len(args) == 1 {
			arg = args[0]
			if !mysql.IsMaria(strings.ToLower(arg)) {
				arg = "mariadb-" + arg
			}
		}
		return switchDatabase(cmd, arg)
	},
}

var mysqlRestoreCmd = &cobra.Command{
	Use:   "restore <file-or-folder>",
	Short: "Import a backup: a single .sql file, or a backup folder",
	Long: `Imports databases into the running MySQL server.

Pass a single .sql file (e.g. one database from a backup folder), or a
whole backup folder — when the folder contains all-databases.sql that
one file is imported, otherwise every .sql file in it is.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if err := mysql.EnsureInitialized(a.Paths, version); err != nil {
			return err
		}
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}

		path := strings.Trim(strings.TrimSpace(args[0]), `"`)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s does not exist", path)
		}

		var files []string
		if info.IsDir() {
			if _, err := os.Stat(filepath.Join(path, "all-databases.sql")); err == nil {
				files = []string{filepath.Join(path, "all-databases.sql")}
			} else {
				entries, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				for _, e := range entries {
					if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
						files = append(files, filepath.Join(path, e.Name()))
					}
				}
				if len(files) == 0 {
					return fmt.Errorf("no .sql files in %s", path)
				}
			}
		} else {
			files = []string{path}
		}

		for _, f := range files {
			fmt.Println("Importing", filepath.Base(f), "...")
			if err := mysql.RestoreFile(a.Paths, version, f); err != nil {
				return err
			}
		}
		fmt.Printf("Done — %d file(s) imported into MySQL %s.\n", len(files), version)
		return nil
	},
}

var mysqlPasswordCmd = &cobra.Command{
	Use:   "password [new-password]",
	Short: "Change the MySQL root password ('' to remove it); phpMyAdmin follows automatically",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		var newPass string
		if len(args) == 1 {
			newPass = args[0]
		} else {
			if !console.Interactive() {
				return fmt.Errorf("pass the new password as an argument, e.g.: mullion mysql password secret")
			}
			fmt.Print("New root password (empty to remove): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			newPass = strings.TrimRight(line, "\r\n")
		}
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}
		if err := mysql.SetRootPassword(a.Paths, version, newPass); err != nil {
			return err
		}
		a.State.Config.MySQLPassword = newPass
		mysql.RootPassword = newPass
		if err := a.State.Save(); err != nil {
			return err
		}
		if err := phpmyadmin.RefreshConfig(a.Paths, newPass); err != nil {
			fmt.Println("note: could not update phpMyAdmin's config -", err)
		}
		if newPass == "" {
			fmt.Println("Root password removed. phpMyAdmin updated.")
		} else {
			fmt.Println("Root password changed. phpMyAdmin updated to match.")
		}
		return nil
	},
}

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage databases on the local MySQL server",
}

var dbListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your databases",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}
		dbs, err := mysql.UserDatabases(a.Paths, version)
		if err != nil {
			return err
		}
		if len(dbs) == 0 {
			fmt.Println("No databases yet. Create one with: mullion db create <name>")
			return nil
		}
		for _, db := range dbs {
			fmt.Println("  " + db)
		}
		return nil
	},
}

var dbCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a database (utf8mb4)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}
		if err := mysql.CreateDatabase(a.Paths, version, args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s is ready.\n", args[0])
		return nil
	},
}

var dbDropCmd = &cobra.Command{
	Use:   "drop <name>",
	Short: "Delete a database and ALL its data",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if console.Interactive() &&
			!askYesNo(fmt.Sprintf("Delete database %q and ALL its data?", args[0]), false) {
			fmt.Println("Aborted.")
			return nil
		}
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}
		if err := mysql.DropDatabase(a.Paths, version, args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s dropped.\n", args[0])
		return nil
	},
}

var mysqlStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the MySQL server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		reclaimMysqlPort(a, false)
		if err := mysql.Start(a.Paths, version); err != nil {
			return err
		}
		fmt.Printf("MySQL %s is running on 127.0.0.1:%d.\n", version, mysql.Port)
		return nil
	},
}

var mysqlStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the MySQL server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if err := mysql.Stop(a.Paths, version); err != nil {
			return err
		}
		fmt.Println("MySQL stopped.")
		return nil
	},
}

var mysqlUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop MySQL and remove its binaries (the data directory is kept)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, version, err := mustMysql()
		if err != nil {
			return err
		}
		if err := mysql.Stop(a.Paths, version); err != nil {
			return err
		}
		if err := os.RemoveAll(a.Paths.MysqlVersionDir(version)); err != nil {
			return err
		}
		a.State.Config.MySQL = ""
		if err := a.State.Save(); err != nil {
			return err
		}
		fmt.Printf("MySQL %s removed. Your databases remain in %s.\n", version, a.Paths.MysqlDataDir())
		return nil
	},
}

func mustMysql() (*app.App, string, error) {
	a := mustApp()
	v := a.State.Config.MySQL
	if v == "" {
		return nil, "", fmt.Errorf("MySQL is not installed (run: mullion mysql install)")
	}
	return a, v, nil
}

func init() {
	mysqlCmd.AddCommand(mysqlInstallCmd, mysqlRestoreCmd, mysqlPasswordCmd, mysqlStartCmd, mysqlStopCmd, mysqlUninstallCmd)
	dbCmd.AddCommand(dbListCmd, dbCreateCmd, dbDropCmd)
	rootCmd.AddCommand(mysqlCmd, mariadbCmd, dbCmd)
}
