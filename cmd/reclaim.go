package cmd

import (
	"fmt"
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
