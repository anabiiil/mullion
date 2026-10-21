package cmd

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/mssql"
	"pm/internal/term"
)

var mssqlCmd = &cobra.Command{
	Use:     "mssql",
	Aliases: []string{"sqlserver"},
	Short:   "Manage the local Microsoft SQL Server (Developer edition)",
	Long: `Runs Microsoft SQL Server 2025 Developer edition (free for development).

On macOS it runs as Microsoft's official Linux container under OrbStack
(install OrbStack first: mullion mssql orbstack install). On Windows it is
a native named instance, MULLION, installed with Microsoft's setup.

Either way it listens on 127.0.0.1:1433 (configurable with mullion ports)
with the "sa" login and a generated password (mullion mssql password).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return showMSSQLStatus(mustApp())
	},
}

var mssqlInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install SQL Server 2025 Developer and start it",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if runtime.GOOS == "darwin" && !mssql.OrbStackInstalled() && !mssql.Status(a.Paths).Docker {
			fmt.Println("SQL Server on macOS runs in a container, which needs OrbStack.")
			if !console.Interactive() || !askYesNo("Download and install OrbStack now?", true) {
				return mssql.ErrNoDocker
			}
			if err := installOrbStack(cmd, a); err != nil {
				return err
			}
		}
		if runtime.GOOS == "windows" {
			fmt.Println("This downloads about 1.5 GB from Microsoft and runs SQL Server setup — accept the UAC prompt.")
		}
		series, err := a.InstallMSSQL(cmd.Context(), nil)
		if err != nil {
			return err
		}
		fmt.Println(term.Green(fmt.Sprintf("✓ %s is running on 127.0.0.1:%d (login sa).", mssql.Label(series), mssql.Port)))
		fmt.Println("  password: mullion mssql password --show")
		if a.SQLServerPHPExtension() == "" {
			fmt.Println("  note: " + app.PHPExtensionHint)
		}
		return nil
	},
}

var mssqlOrbstackCmd = &cobra.Command{
	Use:   "orbstack",
	Short: "OrbStack, the container runtime SQL Server needs on macOS",
}

var mssqlOrbstackInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Download OrbStack from orbstack.dev and install it into /Applications",
	RunE: func(cmd *cobra.Command, args []string) error {
		return installOrbStack(cmd, mustApp())
	},
}

func installOrbStack(cmd *cobra.Command, a *app.App) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("OrbStack is macOS-only — on Windows SQL Server installs natively")
	}
	fmt.Println("OrbStack's first launch opens its own window and may ask you to approve its helper — accept it.")
	if err := a.InstallOrbStack(cmd.Context(), func(s string) { fmt.Println(s) }); err != nil {
		return err
	}
	fmt.Println(term.Green("✓ OrbStack is running."))
	return nil
}

var mssqlStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start SQL Server",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		fmt.Println("Starting SQL Server…")
		if err := a.StartMSSQL(); err != nil {
			return err
		}
		fmt.Printf("%s is running on 127.0.0.1:%d.\n", mssql.Label(a.State.Config.MSSQL), mssql.Port)
		return nil
	},
}

var mssqlStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop SQL Server",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := mustApp().StopMSSQL(); err != nil {
			return err
		}
		fmt.Println("SQL Server stopped.")
		return nil
	},
}

var mssqlStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether SQL Server is installed and running",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showMSSQLStatus(mustApp())
	},
}

func showMSSQLStatus(a *app.App) error {
	v := a.State.Config.MSSQL
	if v == "" {
		fmt.Println("SQL Server is not installed (run: mullion mssql install)")
		return nil
	}
	state := term.Red("stopped") + " (run `mullion mssql start`)"
	if mssql.Running() {
		state = term.Green("running")
	}
	where := "native instance " + mssql.InstanceName
	if runtime.GOOS != "windows" {
		where = "container " + mssql.ContainerName + " (OrbStack)"
	}
	fmt.Printf("%-24s port %d  %s  login sa  — %s\n", mssql.Label(v), mssql.Port, state, where)
	return nil
}

var mssqlPasswordShow bool

var mssqlPasswordCmd = &cobra.Command{
	Use:   "password [new-password]",
	Short: "Show (--show) or change the sa password (no argument = generate a new one)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if a.State.Config.MSSQL == "" {
			return fmt.Errorf("SQL Server is not installed (run: mullion mssql install)")
		}
		if mssqlPasswordShow {
			fmt.Println(a.State.Config.MSSQLPassword)
			return nil
		}
		newPass := ""
		if len(args) == 1 {
			newPass = args[0]
		} else if console.Interactive() {
			fmt.Print("New sa password (empty = generate one): ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			newPass = strings.TrimRight(line, "\r\n")
		}
		pw, err := a.SetMSSQLPassword(newPass)
		if err != nil {
			return err
		}
		fmt.Println("sa password changed:", pw)
		fmt.Println("  update DB_PASSWORD in your projects' .env files.")
		return nil
	},
}

var mssqlDbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage databases on the local SQL Server",
}

var mssqlDbListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your databases",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbs, err := mustApp().MSSQLDatabases()
		if err != nil {
			return err
		}
		if len(dbs) == 0 {
			fmt.Println("No databases yet. Create one with: mullion mssql db create <name>")
			return nil
		}
		for _, db := range dbs {
			fmt.Println("  " + db)
		}
		return nil
	},
}

var mssqlDbCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a database",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		collation, _ := cmd.Flags().GetString("collation")
		if err := mustApp().CreateMSSQLDBWith(args[0], collation); err != nil {
			return err
		}
		fmt.Printf("Database %s is ready.\n", args[0])
		return nil
	},
}

var mssqlDbDropCmd = &cobra.Command{
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
		if err := a.DropMSSQLDB(args[0]); err != nil {
			return err
		}
		fmt.Printf("Database %s dropped.\n", args[0])
		return nil
	},
}

var mssqlBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Back up every SQL Server database (.bak files) to a timestamped folder",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := mustApp().BackupEngine(cmd.Context(), "mssql")
		if err != nil {
			return err
		}
		fmt.Println("Backup saved:", dir)
		fmt.Println("  restore with: mullion mssql restore \"" + dir + "\"")
		return nil
	},
}

var mssqlRestoreCmd = &cobra.Command{
	Use:   "restore <file.bak|backup-folder> [database]",
	Short: "Restore a .bak file (into the database named after it, or the one given) or a backup folder",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		db := ""
		if len(args) == 2 {
			db = args[1]
		}
		if err := mustApp().RestoreMSSQL(args[0], db); err != nil {
			return err
		}
		fmt.Println("Restored from", args[0])
		return nil
	},
}

var (
	mssqlUninstallYes      bool
	mssqlUninstallKeepData bool
	mssqlUninstallNoBackup bool
)

var mssqlUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Back up SQL Server, then remove it (and Adminer)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		v := a.State.Config.MSSQL
		if v == "" {
			return fmt.Errorf("SQL Server is not installed")
		}
		question := fmt.Sprintf("Uninstall %s", mssql.Label(v))
		if mssqlUninstallKeepData {
			question += " (your databases are kept for a reinstall)?"
		} else {
			question += " AND delete all its databases?"
		}
		proceed, err := confirmDestructive(mssqlUninstallYes, question)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		dir, err := a.UninstallEngine(cmd.Context(), "mssql", !mssqlUninstallNoBackup, !mssqlUninstallKeepData)
		if err != nil {
			return err
		}
		if dir != "" {
			fmt.Println("Backup saved:", dir)
		}
		fmt.Println("SQL Server removed.")
		return nil
	},
}

func init() {
	mssqlUninstallCmd.Flags().BoolVar(&mssqlUninstallYes, "yes", false, "do not ask for confirmation")
	mssqlUninstallCmd.Flags().BoolVar(&mssqlUninstallKeepData, "keep-data", false, "keep the databases (the data volume / instance folder)")
	mssqlUninstallCmd.Flags().BoolVar(&mssqlUninstallNoBackup, "no-backup", false, "skip the database backup")
	mssqlPasswordCmd.Flags().BoolVar(&mssqlPasswordShow, "show", false, "print the current sa password")
	mssqlOrbstackCmd.AddCommand(mssqlOrbstackInstallCmd)
	mssqlDbCreateCmd.Flags().String("collation", "", "collation, e.g. SQL_Latin1_General_CP1_CI_AS or Arabic_100_CI_AS_SC_UTF8 (default: the server's)")
	mssqlDbCmd.AddCommand(mssqlDbListCmd, mssqlDbCreateCmd, mssqlDbDropCmd)
	mssqlCmd.AddCommand(mssqlInstallCmd, mssqlUninstallCmd, mssqlStartCmd, mssqlStopCmd, mssqlStatusCmd,
		mssqlBackupCmd, mssqlRestoreCmd, mssqlPasswordCmd, mssqlDbCmd, mssqlOrbstackCmd)
	rootCmd.AddCommand(mssqlCmd)
}
