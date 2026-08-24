package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/autostart"
	"pm/internal/caddy"
	"pm/internal/console"
	"pm/internal/devserver"
	"pm/internal/elevate"
	"pm/internal/fcgi"
	"pm/internal/hosts"
	"pm/internal/mysql"
	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/shortcut"
)

var (
	uninstallYes      bool
	uninstallBackup   bool
	uninstallNoBackup bool
	uninstallPause    bool
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Mullion completely (PHP, MySQL, phpMyAdmin, Composer, config)",
	Long: `Stops every service and removes everything Mullion put on this machine:
~/.mullion (PHP versions, MySQL and its databases, phpMyAdmin, Composer),
the hosts-file entries, the PATH entries, the trusted root certificate,
and the autostart registration.

Your project folders are NOT touched — Mullion only ever links to them.

Before deleting, it offers to export all your MySQL databases to a
backup .sql file in your user folder.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runUninstall()
		if uninstallPause {
			if err != nil {
				fmt.Fprintln(os.Stderr, "\nError:", err)
			}
			fmt.Print("\nPress Enter to close this window...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				os.Exit(1)
			}
		}
		return err
	},
}

func runUninstall() error {
	a := mustApp()

	// Decisions are made here, before any elevation, so the questions are
	// asked exactly once.
	if !uninstallYes {
		if !console.Interactive() {
			return fmt.Errorf("refusing to uninstall without confirmation; pass --yes")
		}
		fmt.Println("This removes Mullion completely: PHP versions, MySQL AND its databases,")
		fmt.Println("phpMyAdmin, Composer, hosts entries, PATH entries, and autostart.")
		fmt.Println("Your project folders are NOT touched.")
		if !askYesNo("Continue?", false) {
			fmt.Println("Aborted — nothing was removed.")
			return nil
		}
	}

	wantBackup := uninstallBackup
	if !uninstallBackup && !uninstallNoBackup {
		wantBackup = false
		if a.State.Config.MySQL != "" && mysql.DataInitialized(a.Paths) && console.Interactive() {
			wantBackup = askYesNo("Export your MySQL databases to a backup .sql file first?", true)
		}
	}

	// One elevation for the whole removal (hosts file, system PATH,
	// possibly elevated service processes). Fire-and-forget: the child
	// kills every Mullion process, so this parent must be gone by then —
	// waiting would mean getting killed mid-wait or keeping
	// bin\mullion.exe locked forever.
	if !elevate.IsElevated() && console.Interactive() {
		if exe, err := os.Executable(); err == nil {
			args := []string{"uninstall", "--yes", "--pause"}
			if wantBackup {
				args = append(args, "--backup")
			} else {
				args = append(args, "--no-backup")
			}
			fmt.Println("Uninstall needs administrator rights — please accept the UAC prompt.")
			if err := elevate.RelaunchAsync(exe, args...); err != nil {
				return err
			}
			fmt.Println("Uninstall is continuing in the elevated window.")
			return nil
		}
	}

	return doUninstall(a, wantBackup)
}

func doUninstall(a *app.App, wantBackup bool) error {
	// 1. Database backup — a failed backup aborts the uninstall rather
	// than deleting data the user asked to keep.
	if wantBackup && a.State.Config.MySQL != "" && mysql.DataInitialized(a.Paths) {
		v := a.State.Config.MySQL
		// Port 3306 answering while no Mullion mysqld runs means another
		// server owns it — dumping would export THEIR data. Offer to
		// stop it (via its manager) so Mullion's own data gets backed up.
		if mysql.Running() && len(processesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) == 0 {
			if !reclaimMysqlPort(a, false) {
				return fmt.Errorf("another MySQL server is on port %d — stop it and re-run, or pass --no-backup", mysql.Port)
			}
		}
		if !mysql.Running() {
			fmt.Println("Starting MySQL to export your databases...")
			if err := mysql.Start(a.Paths, v); err != nil {
				return fmt.Errorf("could not start MySQL for the backup: %w (re-run with --no-backup to skip)", err)
			}
		}
		dbs, err := mysql.UserDatabases(a.Paths, v)
		if err != nil {
			return fmt.Errorf("backup failed: %w (re-run with --no-backup to skip)", err)
		}
		if len(dbs) > 0 {
			dir := filepath.Join(a.Paths.BackupsDir(), time.Now().Format("2006-01-02_150405"))
			fmt.Printf("Exporting %d database(s) to %s ...\n", len(dbs), dir)
			if err := mysql.BackupTo(a.Paths, v, dbs, dir); err != nil {
				return fmt.Errorf("backup failed: %w (re-run with --no-backup to skip)", err)
			}
			fmt.Println("Backup saved:", dir)
			fmt.Println("  one .sql per database + all-databases.sql")
			fmt.Println("  restore later with: mullion mysql restore \"" + dir + "\"")
		} else {
			fmt.Println("No user databases found — nothing to back up.")
		}
	}

	// 2. Stop everything — but ONLY Mullion's own processes: killing by
	// image name would take down Laragon's/XAMPP's servers too. Scope
	// strictly to executables living under ~/.mullion.
	fmt.Println("Stopping services...")
	_ = caddy.Stop(a.Paths)
	_ = fcgi.StopAll(a.Paths)
	devserver.StopAll(a.Paths)
	if v := a.State.Config.MySQL; v != "" && len(processesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) > 0 {
		_ = mysql.Stop(a.Paths, v)
	}
	for _, pid := range processesUnder(a.Paths.Home, "") {
		// Never kill ourselves — this very process may run from the
		// installed bin directory.
		if pid == os.Getpid() {
			continue
		}
		killProcess(pid)
	}

	// 3. Hosts entries, trust store, autostart, PATH.
	if err := hosts.Sync(nil); err != nil {
		fmt.Println("note: could not clean the hosts file -", err)
	}
	if _, err := os.Stat(a.Paths.CaddyExe()); err == nil {
		if out, err := proc.Quiet(a.Paths.CaddyExe(), "untrust").CombinedOutput(); err != nil {
			fmt.Println("note: could not remove the root certificate -", strings.TrimSpace(string(out)))
		}
	}
	if err := autostart.Disable(); err != nil {
		fmt.Println("note:", err)
	}
	dirs := []string{a.Paths.BinDir(), a.Paths.CurrentPhp()}
	if err := app.RemovePathEntries("User", dirs...); err != nil {
		fmt.Println("note:", err)
	}
	if elevate.IsElevated() {
		if err := app.RemovePathEntries("Machine", dirs...); err != nil {
			fmt.Println("note:", err)
		}
	}

	if err := shortcut.RemoveDesktop(); err != nil {
		fmt.Println("note:", err)
	}

	// 4. Delete ~/.mullion. Everything except bin/ goes right now; bin
	// holds the running mullion.exe (this process, and possibly the
	// non-elevated window that spawned us), so a detached helper waits
	// until every Mullion process has actually exited before removing
	// the remainder — a fixed sleep loses that race.
	fmt.Println("Removing", a.Paths.Home, "...")
	if entries, err := os.ReadDir(a.Paths.Home); err == nil {
		for _, e := range entries {
			if strings.EqualFold(e.Name(), "bin") {
				continue
			}
			if err := os.RemoveAll(filepath.Join(a.Paths.Home, e.Name())); err != nil {
				fmt.Println("note: could not remove", e.Name(), "-", err)
			}
		}
	}

	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if !strings.HasPrefix(strings.ToLower(exe), strings.ToLower(a.Paths.Home)+string(os.PathSeparator)) {
		if err := os.RemoveAll(a.Paths.Home); err != nil {
			return err
		}
	} else {
		if err := scheduleHomeRemoval(a.Paths.Home); err != nil {
			return err
		}
		fmt.Println("(the bin folder removes itself moments after the last Mullion window closes)")
	}

	fmt.Println("\nMullion has been removed. Your project folders were not touched.")
	return nil
}

func askYesNo(question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	// Drop keystrokes queued during long steps — they are not an answer.
	console.FlushInput()
	fmt.Printf("%s %s ", question, hint)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return def
	}
	return answer == "y" || answer == "yes"
}

func init() {
	uninstallCmd.Flags().BoolVar(&uninstallYes, "yes", false, "do not ask for confirmation")
	uninstallCmd.Flags().BoolVar(&uninstallBackup, "backup", false, "export databases before removing")
	uninstallCmd.Flags().BoolVar(&uninstallNoBackup, "no-backup", false, "skip the database backup")
	uninstallCmd.Flags().BoolVar(&uninstallPause, "pause", false, "wait for Enter before exiting (used by the elevated relaunch)")
	_ = uninstallCmd.Flags().MarkHidden("pause")
	rootCmd.AddCommand(uninstallCmd)
}
