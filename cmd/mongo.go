package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/mongodb"
	"pm/internal/term"
)

var mongoCmd = &cobra.Command{
	Use:   "mongo",
	Short: "Manage the local MongoDB server",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showMongoStatus(mustApp())
	},
}

var mongoInstallCmd = &cobra.Command{
	Use:   "install [version]",
	Short: "Download MongoDB (newest " + mongodb.DefaultSeries + " by default), initialize it, and start it",
	Long: `Installs and starts MongoDB. With no argument you get the newest
release of the ` + mongodb.DefaultSeries + ` series. Pass "latest" for the newest release
overall, a series like "7.0" for the newest release of that series, or
a full version like "8.0.12".`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		fmt.Println("Resolving the MongoDB version...")
		version, err := a.InstallMongo(cmd.Context(), arg)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", term.Green(fmt.Sprintf("✓ %s is running on 127.0.0.1:%d.", mongodb.Label(version), mongodb.Port)))
		return nil
	},
}

var mongoStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the MongoDB server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StartMongo(); err != nil {
			return err
		}
		fmt.Printf("%s is running on 127.0.0.1:%d.\n", mongodb.Label(a.State.Config.Mongo), mongodb.Port)
		return nil
	},
}

var mongoStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the MongoDB server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StopMongo(); err != nil {
			return err
		}
		fmt.Println("MongoDB stopped.")
		return nil
	},
}

var mongoStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether MongoDB is installed and running",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showMongoStatus(mustApp())
	},
}

func showMongoStatus(a *app.App) error {
	v := a.State.Config.Mongo
	if v == "" {
		fmt.Println("MongoDB is not installed (run: mullion mongo install)")
		return nil
	}
	state := term.Red("stopped") + " (run `mullion mongo start`)"
	if mongodb.Running() {
		state = term.Green("running")
	}
	fmt.Printf("%-24s port %d  %s\n", mongodb.Label(v), mongodb.Port, state)
	return nil
}

var mongoDbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage databases on the local MongoDB server",
}

var mongoDbListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your databases",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dbs, err := a.MongoDatabases()
		if err != nil {
			return err
		}
		if len(dbs) == 0 {
			fmt.Println("No databases yet. Create one with: mullion mongo db create <name>")
			return nil
		}
		for _, db := range dbs {
			fmt.Println("  " + db)
		}
		return nil
	},
}

var mongoDbCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a database",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.CreateMongoDB(args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s is ready.\n", args[0])
		return nil
	},
}

var mongoDbDropCmd = &cobra.Command{
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
		if err := a.DropMongoDB(args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s dropped.\n", args[0])
		return nil
	},
}

var mongoBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Export all your MongoDB databases to a timestamped backup folder",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, err := a.BackupEngine(cmd.Context(), "mongo")
		if err != nil {
			return err
		}
		fmt.Println("Backup saved:", dir)
		fmt.Println("  restore with: mullion mongo restore \"" + dir + "\"")
		return nil
	},
}

var mongoRestoreCmd = &cobra.Command{
	Use:   "restore <backup-folder-or-.archive.gz-file> [database]",
	Short: "Restore a MongoDB backup",
	Long: `Restores databases into the running MongoDB server.

Pass a backup folder (as mullion mongo backup creates) to restore every
database in it, or add a database name to restore just that one. Pass a
single .archive.gz file directly to restore it alone (the database it
holds was fixed when it was dumped, so no database name is needed).`,
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
		v := a.State.Config.Mongo
		if v == "" {
			return fmt.Errorf("MongoDB is not installed (run: mullion mongo install)")
		}
		if !mongodb.ToolsInstalled(a.Paths) {
			if err := mongodb.InstallTools(cmd.Context(), a.Paths); err != nil {
				return err
			}
		}
		if err := mongodb.Start(a.Paths, v); err != nil {
			return err
		}
		if err := mongodb.RestoreFile(a.Paths, path); err != nil {
			return err
		}
		fmt.Printf("Restored %s.\n", filepath.Base(path))
		return nil
	},
}

var (
	mongoUninstallYes      bool
	mongoUninstallKeepData bool
	mongoUninstallNoBackup bool
)

var mongoUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop MongoDB, back it up, and remove it (binaries, data, and mongo-express)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		v := a.State.Config.Mongo
		if v == "" {
			return fmt.Errorf("MongoDB is not installed")
		}
		question := fmt.Sprintf("Uninstall %s", mongodb.Label(v))
		if mongoUninstallKeepData {
			question += " (binaries and mongo-express only — your databases are kept)?"
		} else {
			question += " AND delete all its databases?"
		}
		proceed, err := confirmDestructive(mongoUninstallYes, question)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		dir, err := a.UninstallEngine(cmd.Context(), "mongo", !mongoUninstallNoBackup, !mongoUninstallKeepData)
		if err != nil {
			return err
		}
		if dir != "" {
			fmt.Println("Backup saved:", dir)
		}
		fmt.Println("MongoDB removed.")
		return nil
	},
}

func init() {
	mongoUninstallCmd.Flags().BoolVar(&mongoUninstallYes, "yes", false, "do not ask for confirmation")
	mongoUninstallCmd.Flags().BoolVar(&mongoUninstallKeepData, "keep-data", false, "keep the data directory (binaries only)")
	mongoUninstallCmd.Flags().BoolVar(&mongoUninstallNoBackup, "no-backup", false, "skip the database backup")
	mongoDbCmd.AddCommand(mongoDbListCmd, mongoDbCreateCmd, mongoDbDropCmd)
	mongoCmd.AddCommand(mongoInstallCmd, mongoBackupCmd, mongoRestoreCmd, mongoStartCmd, mongoStopCmd,
		mongoStatusCmd, mongoDbCmd, mongoUninstallCmd)
	rootCmd.AddCommand(mongoCmd)
}
