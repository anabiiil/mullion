package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/mysql"
	"pm/internal/pmdir"
)

// reclaimMysqlPort makes sure port 3306 is free or held by Mullion's
// OWN mysqld. A foreign server (a launchd/brew-managed MySQL that came
// back after setup, Laragon, ...) is stopped through its manager — with
// the user's consent when a human is present, or when the caller passes
// explicit consent (the user pressed a Start button). Returns true when
// Mullion's MySQL can use the port.
func reclaimMysqlPort(a *app.App, explicitConsent bool) bool {
	if !mysql.Running() {
		return true
	}
	if len(processesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) > 0 {
		return true // it's ours
	}
	pid, name := portOwner(mysql.Port)
	fmt.Printf("\nAnother MySQL server (%s, PID %d) is holding port %d — it is NOT Mullion's.\n", name, pid, mysql.Port)
	fmt.Println("That's why connections (phpMyAdmin included) hit the wrong server.")
	if !explicitConsent {
		if !console.Interactive() {
			fmt.Println("warning: run `mullion mysql start` in a terminal to stop it and take the port back.")
			return false
		}
		if !askYesNo("Stop it for good (via its manager) so Mullion's MySQL takes over?", true) {
			return false
		}
	}
	// Its databases must survive the takeover: export them FIRST (asking
	// for that server's root password when it has one).
	if console.Interactive() {
		if dump := exportForeignDatabases(a); dump != "" {
			pendingRestore = dump
		}
	}
	stopConflict(portConflict{Port: mysql.Port, PID: pid, Name: name})
	for i := 0; i < 20 && portBusy(mysql.Port); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	if portBusy(mysql.Port) {
		fmt.Println("warning: port", mysql.Port, "is still busy — the other server may need stopping manually (brew services list).")
		return false
	}
	fmt.Println("Port", mysql.Port, "is free — Mullion's MySQL takes over.")
	return true
}

// pendingRestore is an export taken from a replaced foreign MySQL,
// waiting to be imported once Mullion's own server is up. It normally
// holds the export FOLDER exportForeignDatabases wrote (one
// self-contained <name>.sql per database); restorePendingDump also
// accepts a path straight to a single .sql file, for backward
// compatibility with an older Mullion build that saved a combined
// all-databases.sql path here instead.
var pendingRestore string

// restorePendingDump imports the foreign server's exported databases
// into Mullion's now-running MySQL.
func restorePendingDump(a *app.App) {
	if pendingRestore == "" {
		return
	}
	dump := pendingRestore
	pendingRestore = ""
	fmt.Println("Importing the databases from the replaced server...")
	if info, err := os.Stat(dump); err == nil && !info.IsDir() {
		// Backward compatibility: an older Mullion saved a single
		// combined dump file here instead of a folder.
		if err := mysql.RestoreFile(a.Paths, a.State.Config.MySQL, dump); err != nil {
			fmt.Println("warning:", err)
			fmt.Println("Import them later with: mullion mysql restore \"" + dump + "\"")
			return
		}
		fmt.Println("Databases imported. The backup stays at", dump)
		return
	}
	restoreImportedDatabases(a, a.State.Config.MySQL, dump)
}

// exportForeignDatabases dumps the user databases of whatever answers
// on the MySQL port as one self-contained <name>.sql file per database
// (mysql.BackupDatabases — no combined all-databases.sql, which would
// double the disk and time a multi-GB export needs), prompting for its
// root password when the empty one is rejected. It uses the same
// backup-location choice/validation setup's import uses
// (chooseBackupBase, which honors --backup-dir when the current
// command defines it) and the same datadir free-space check
// (ensureDatadirSpace). Returns the export folder to restore from (""
// when nothing was saved).
func exportForeignDatabases(a *app.App) string {
	if a.State.Config.MySQL == "" {
		return ""
	}
	if !askYesNo("Export its databases first so nothing is lost?", true) {
		return ""
	}
	version := a.State.Config.MySQL
	savedPassword := mysql.RootPassword
	defer func() { mysql.RootPassword = savedPassword }()

	mysql.RootPassword = ""
	dbs, err := mysql.UserDatabases(a.Paths, version)
	for attempt := 0; err != nil && attempt < 3; attempt++ {
		fmt.Println("That server rejected the passwordless root login.")
		console.FlushInput()
		fmt.Print("Its root password (Enter to skip the backup): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			fmt.Println("Skipping the backup — the old data files stay untouched on disk.")
			return ""
		}
		mysql.RootPassword = pw
		dbs, err = mysql.UserDatabases(a.Paths, version)
	}
	if err != nil {
		fmt.Println("warning: could not read that server's databases —", err)
		fmt.Println("Skipping the backup — the old data files stay untouched on disk.")
		return ""
	}
	if len(dbs) == 0 {
		fmt.Println("It has no user databases — nothing to export.")
		return ""
	}

	base, estimate, skip := chooseBackupBase(a, version, dbs)
	if skip {
		fmt.Println("Skipping the backup — the old data files stay untouched on disk.")
		return ""
	}
	if !ensureDatadirSpace(a, estimate, base) {
		return ""
	}
	dir := filepath.Join(base, time.Now().Format("2006-01-02_150405")+"-replaced-server")
	fmt.Printf("Exporting %d database(s) to %s: %s\n", len(dbs), dir, strings.Join(dbs, ", "))
	if err := mysql.BackupDatabases(a.Paths, version, dbs, dir); err != nil {
		fmt.Println("warning: export failed -", err)
		return ""
	}
	return dir
}
