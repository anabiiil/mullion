package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/autostart"
	"pm/internal/caddy"
	"pm/internal/composer"
	"pm/internal/console"
	"pm/internal/elevate"
	"pm/internal/heidisql"
	"pm/internal/mysql"
	"pm/internal/nodever"
	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/shortcut"
	"pm/internal/vcredist"
	"pm/internal/version"
)

var setupPause bool

// signInPhrase names the OS's login moment in questions and messages.
var signInPhrase = map[string]string{"windows": "sign in to Windows", "darwin": "log in to your Mac"}[runtime.GOOS]

func init() {
	if signInPhrase == "" {
		signInPhrase = "log in"
	}
}

// exampleProjectDir is the sample path in setup's final message.
func exampleProjectDir() string {
	if runtime.GOOS == "windows" {
		return `C:\code\myapp`
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "code", "myapp")
	}
	return "~/code/myapp"
}

func nodeSummary(a *app.App) string {
	if v := a.State.Config.GlobalNode; v != "" {
		return v
	}
	return "not installed (mullion node install lts)"
}

func panelHint() string {
	if runtime.GOOS == "windows" {
		return " — or just double-click mullion.exe"
	}
	return ""
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "First-time setup: PATH, Caddy, latest PHP + Composer + MySQL + phpMyAdmin",
	Long: `Sets up a complete local dev stack in one go: creates ~/.mullion, downloads
Caddy, puts mullion on your PATH, installs the latest PHP and makes it the
system default, installs Composer, installs and starts the latest MySQL,
and serves phpMyAdmin at https://phpmyadmin.<tld>.

Every step is skipped when it's already done, so re-running setup is safe.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		err := runSetup(cmd)
		if setupPause {
			if err != nil {
				fmt.Fprintln(os.Stderr, "\nError:", err)
				fmt.Fprintln(os.Stderr, "Re-running `mullion setup` is safe: completed steps are skipped.")
			}
			fmt.Print("\nPress Enter to close this window...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				// Propagate failure to the non-elevated parent window
				// without cobra printing the error a second time.
				os.Exit(1)
			}
		}
		return err
	},
}

func runSetup(cmd *cobra.Command) error {
	// One admin approval for the whole setup instead of a UAC prompt per
	// step (hosts file, trust store). Only when a human is at a console;
	// scripted runs keep the old per-action elevation behaviour.
	if !elevate.IsElevated() && console.Interactive() {
		if exe, err := os.Executable(); err == nil {
			fmt.Println("Setup needs administrator rights — please accept the UAC prompt.")
			fmt.Println("Setup will continue in a new window.")
			err := elevate.Relaunch(exe, "setup", "--pause")
			if err == nil {
				fmt.Println("\nSetup finished successfully in the elevated window.")
				return nil
			}
			fmt.Println("\nSetup did NOT finish — check the error shown in the elevated window.")
			fmt.Println("Re-running `mullion setup` is safe: completed steps are skipped.")
			return err
		}
	}

	wantAutostart := true
	dbChoice := "phpmyadmin"
	if console.Interactive() {
		wantAutostart = askYesNo("Start Mullion automatically when you "+signInPhrase+"?", true)

		fmt.Println("Which database manager do you want?")
		fmt.Println("  [1] phpMyAdmin  (in the browser, at https://phpmyadmin.test)")
		if runtime.GOOS == "windows" {
			fmt.Println("  [2] HeidiSQL    (desktop app)")
			fmt.Println("  [3] both")
			fmt.Println("  [4] none")
		} else {
			fmt.Println("  [2] none")
		}
		console.FlushInput()
		fmt.Print("Choice [1]: ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.TrimSpace(answer) {
		case "2":
			dbChoice = "heidisql"
			if runtime.GOOS != "windows" {
				dbChoice = "none"
			}
		case "3":
			dbChoice = "both"
		case "4":
			dbChoice = "none"
		}
	}

	return doSetup(cmd, wantAutostart, dbChoice)
}

func doSetup(cmd *cobra.Command, wantAutostart bool, dbChoice string) error {
	{
		a := mustApp()

		fmt.Println("Setting up Mullion in", a.Paths.Home)
		// PHP's Windows builds and mysqld both need the VC++ runtime,
		// which clean Windows 10/11 machines don't have.
		if err := vcredist.Ensure(cmd.Context(), a.Paths); err != nil {
			return err
		}
		if err := caddy.EnsureInstalled(cmd.Context(), a.Paths); err != nil {
			return err
		}

		// Install this executable into ~/.mullion/bin so `mullion` works
		// from anywhere. A package-manager install (Homebrew) is
		// SYMLINKED, not copied — the bin entry leads the PATH, and a
		// stale copy there would keep shadowing every `brew upgrade`.
		if exe, err := os.Executable(); err == nil {
			if err := installSelf(a, exe); err != nil {
				fmt.Println("note: could not install mullion into", a.Paths.BinDir(), "-", err)
			}
		}

		// Desktop shortcut to the installed copy — the file setup was
		// launched from may get deleted later.
		if err := shortcut.CreateDesktop(a.Paths); err != nil {
			fmt.Println("note:", err)
		} else {
			fmt.Println("Desktop shortcut created.")
		}

		if err := app.EnsureUserPath(a.Paths.BinDir(), a.Paths.CurrentPhp()); err != nil {
			return err
		}
		// A version manager (.pvm & friends) already owning `php` would
		// shadow Mullion forever — offer to disable its PATH line now.
		if runtime.GOOS != "windows" {
			if shadow := a.PhpShadow(); shadow != "" {
				fmt.Printf("\nAnother `php` is on your PATH: %s\n", shadow)
				offerShadowFix(shadow)
			}
		}
		// Ahead of Laragon/XAMPP on the system PATH, so `php` and
		// `composer` always mean Mullion's (possible because setup is
		// already elevated; without elevation the shadow warning covers it).
		if elevate.IsElevated() {
			if err := app.EnsureMachinePathPriority(a.Paths.BinDir(), a.Paths.CurrentPhp()); err != nil {
				fmt.Println("note:", err)
			}
		}

		// Other stacks squatting on ports 80/443/3306 would silently
		// swallow Mullion's sites and MySQL — take over with consent.
		resolvePortConflicts(a)

		// Register autostart before the big downloads: even if a later
		// step fails, the machine keeps a working command and the services
		// come up (and self-heal) at the next sign-in.
		if wantAutostart {
			if err := autostart.Enable(a.Paths); err != nil {
				fmt.Println("note: could not enable autostart -", err)
			} else {
				fmt.Println("Mullion will start automatically when you " + signInPhrase + " (turn off with `mullion autostart off`).")
			}
		}

		// Latest PHP, set as the system default.
		if a.State.Config.GlobalPHP == "" {
			releases, err := phpver.FetchCurrent(cmd.Context())
			if err != nil {
				return err
			}
			if len(releases) == 0 {
				return fmt.Errorf("no PHP releases found on %s", phpver.Source)
			}
			latest := releases[len(releases)-1]
			if _, err := phpver.Install(cmd.Context(), a.Paths, latest); err != nil {
				return err
			}
			if err := a.UseGlobal(latest.Version); err != nil {
				return err
			}
			fmt.Printf("PHP %s is now the system default.\n", latest.Version)
		} else {
			fmt.Printf("PHP %s already installed — keeping it.\n", a.State.Config.GlobalPHP)
		}
		if err := a.State.Save(); err != nil {
			return err
		}

		// Composer (runs on the active PHP via the `composer` shim).
		if err := composer.Ensure(cmd.Context(), a.Paths); err != nil {
			return err
		}

		// Latest LTS Node (frontend projects need it). Failures are
		// notes, not errors: the PHP stack must not die on nodejs.org.
		if a.State.Config.GlobalNode == "" {
			if rel, err := nodever.Resolve(cmd.Context(), "lts"); err != nil {
				fmt.Println("note: could not resolve the Node LTS -", err)
			} else if _, err := nodever.Install(cmd.Context(), a.Paths, rel); err != nil {
				fmt.Println("note: could not install Node -", err)
			} else if err := a.ActivateNode(rel.Version); err != nil {
				fmt.Println("note:", err)
			} else {
				fmt.Printf("Node %s (LTS, with npm) is now the default.\n", rel.Version)
			}
		} else {
			fmt.Printf("Node %s already installed — keeping it.\n", a.State.Config.GlobalNode)
		}

		// Newest LTS MySQL, initialized and running.
		if a.State.Config.MySQL == "" {
			fmt.Printf("Looking up the newest MySQL %s (LTS)...\n", mysql.DefaultSeries)
			v, err := mysql.DefaultVersion(cmd.Context())
			if err != nil {
				return err
			}
			if err := mysql.Install(cmd.Context(), a.Paths, v); err != nil {
				return err
			}

			// An existing MySQL (Laragon, XAMPP, a service...) already on
			// port 3306: offer to bring its databases along using the
			// freshly-installed client tools, then take the port over.
			importDump := importFromExistingMysql(a, v)

			a.State.Config.MySQL = v
			if err := a.State.Save(); err != nil {
				return err
			}
			if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
				return err
			}
			if err := mysql.Start(a.Paths, v); err != nil {
				return err
			}
			if importDump != "" {
				fmt.Println("Importing the databases from your old server...")
				if err := mysql.RestoreFile(a.Paths, v, importDump); err != nil {
					fmt.Println("warning:", err)
					fmt.Println("Import them later with: mullion mysql restore \"" + filepath.Dir(importDump) + "\"")
				} else {
					fmt.Println("Databases imported. Backup kept at", filepath.Dir(importDump))
				}
			}
			fmt.Printf("MySQL %s is running on 127.0.0.1:%d (user `root`, empty password).\n", v, mysql.Port)
		} else {
			fmt.Printf("%s already installed — your databases are untouched.\n", mysql.Label(a.State.Config.MySQL))
		}

		// Database manager(s), per the user's choice. phpMyAdmin's step
		// also converges everything via Apply; without it, start the
		// servers explicitly.
		dbLines := ""
		if dbChoice == "phpmyadmin" || dbChoice == "both" {
			if err := ensurePhpMyAdmin(cmd.Context(), a, ""); err != nil {
				return err
			}
			dbLines += "  phpmyadmin https://phpmyadmin." + a.State.Config.TLD + "\n"
		} else {
			if err := a.Apply(); err != nil {
				return err
			}
			if err := caddy.Start(a.Paths); err != nil {
				return err
			}
		}
		if dbChoice == "heidisql" || dbChoice == "both" {
			if err := heidisql.Install(cmd.Context(), a.Paths); err != nil {
				return err
			}
			dbLines += "  heidisql   `mullion heidisql` or the panel's HeidiSQL button\n"
		}

		if err := os.WriteFile(a.Paths.VersionFile(), []byte(version.Number), 0o644); err != nil {
			fmt.Println("note:", err)
		}

		// Put the tray icon up right away (also the sign-in behavior).
		// macOS has no tray icon (yet); the LaunchAgent covers sign-in.
		if runtime.GOOS == "windows" {
			if exe, err := os.Executable(); err == nil {
				trayExe := filepath.Join(a.Paths.BinDir(), pmdir.ExeName("mullion"))
				if _, statErr := os.Stat(trayExe); statErr != nil {
					trayExe = exe
				}
				spawnSelf(trayExe, "tray")
			}
		}

		fmt.Printf(`
Done! Your stack is running:
  php        %s (`+"`php -v`"+` in any NEW terminal)
  composer   latest (`+"`composer -V`"+` in any NEW terminal)
  node       %s (with npm — `+"`node -v`"+` in any NEW terminal)
  mysql      %s on 127.0.0.1:%d (user root, empty password)
%sServe a project:
  cd %s
  mullion link                serve it at http://myapp.%s
  mullion secure              upgrade it to https

Control panel: run `+"`mullion ui`"+`%s.
`, a.State.Config.GlobalPHP, nodeSummary(a), a.State.Config.MySQL, mysql.Port, dbLines+"\n", exampleProjectDir(), a.State.Config.TLD, panelHint())
		return nil
	}
}

// importFromExistingMysql handles a foreign MySQL (Laragon, XAMPP, a
// service) sitting on port 3306 during setup: with the user's OK it
// exports that server's databases (returning the dump to restore into
// Mullion's MySQL) and stops it so Mullion's server can take the port.
// Returns "" when there is nothing to import.
func importFromExistingMysql(a *app.App, version string) string {
	if !portBusy(mysql.Port) {
		return ""
	}
	owner, name := portOwner(mysql.Port)
	fmt.Printf("\nAnother MySQL server is already running on port %d", mysql.Port)
	if owner > 0 {
		fmt.Printf(" (%s, PID %d — Laragon/XAMPP?)", name, owner)
	}
	fmt.Println(".")

	dump := ""
	if console.Interactive() && askYesNo("Import its databases into Mullion?", true) {
		// Mullion's client tools work against whatever answers on 3306;
		// dev stacks almost always run root with an empty password.
		dbs, err := mysql.UserDatabases(a.Paths, version)
		switch {
		case err != nil:
			fmt.Println("warning: could not read that server's databases (password-protected root?) — skipping the import.")
			fmt.Println("You can export from it manually later and load the file with `mullion mysql restore`.")
		case len(dbs) == 0:
			fmt.Println("It has no user databases — nothing to import.")
		default:
			dir := filepath.Join(a.Paths.BackupsDir(),
				time.Now().Format("2006-01-02_150405")+"-imported")
			fmt.Printf("Exporting %d database(s) to %s: %s\n", len(dbs), dir, strings.Join(dbs, ", "))
			if err := mysql.BackupTo(a.Paths, version, dbs, dir); err != nil {
				fmt.Println("warning: export failed -", err)
			} else {
				dump = filepath.Join(dir, "all-databases.sql")
			}
		}
	}

	stop := true
	if console.Interactive() {
		stop = askYesNo(fmt.Sprintf("Stop it now so Mullion's MySQL can take port %d?", mysql.Port), true)
	}
	if !stop {
		fmt.Println("warning: Mullion's MySQL cannot start while the other server holds the port.")
		return dump
	}
	if owner > 0 {
		stopConflict(portConflict{Port: mysql.Port, PID: owner, Name: name})
	}
	for i := 0; i < 20 && portBusy(mysql.Port); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if portBusy(mysql.Port) {
		fmt.Println("warning: port", mysql.Port, "is still busy (the other stack may auto-restart its services — quit it fully).")
	}
	return dump
}

// installSelf puts the running executable at bin/mullion: a SYMLINK
// when it runs from a package manager's bin (brew keeps it current on
// upgrade), a copy otherwise (a downloaded file may get deleted later).
func installSelf(a *app.App, exe string) error {
	dest := filepath.Join(a.Paths.BinDir(), pmdir.ExeName("mullion"))
	if runtime.GOOS != "windows" && isPackageManagerBin(exe) {
		if target, err := os.Readlink(dest); err == nil && target == exe {
			return nil
		}
		os.Remove(dest)
		if err := os.Symlink(exe, dest); err != nil {
			return err
		}
		fmt.Println("mullion in", a.Paths.BinDir(), "now follows your package manager's install (brew upgrade keeps it current).")
		return nil
	}
	resolved := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = r
	}
	if sameFile(resolved, dest) {
		return nil
	}
	if err := copyFile(resolved, dest); err != nil {
		// An older Mullion (tray, panel) is running from bin —
		// this is an upgrade: replace it.
		for _, pid := range processesUnder(a.Paths.Home, pmdir.ExeName("mullion")) {
			if pid != os.Getpid() {
				killProcess(pid)
			}
		}
		time.Sleep(time.Second)
		if err := copyFile(resolved, dest); err != nil {
			return err
		}
	}
	fmt.Println("Updated the installed mullion binary.")
	return nil
}

// isPackageManagerBin reports whether the (unresolved) executable path
// lives in a package manager's bin directory.
func isPackageManagerBin(exe string) bool {
	dir := filepath.Dir(exe)
	switch dir {
	case "/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin":
		return true
	}
	return strings.Contains(exe, "/Cellar/")
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func init() {
	setupCmd.Flags().BoolVar(&setupPause, "pause", false, "wait for Enter before exiting (used by the elevated relaunch)")
	_ = setupCmd.Flags().MarkHidden("pause")
	rootCmd.AddCommand(setupCmd)
}
