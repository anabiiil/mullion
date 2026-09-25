package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/ui"
)

// maybeRunExplorerWizard handles mullion.exe being double-clicked in Explorer:
// once set up, open the control panel like a regular program; otherwise
// offer to run first-time setup, pausing before the console disappears.
// Returns true when it handled the launch (Execute should not run cobra).
func maybeRunExplorerWizard() bool {
	if len(os.Args) > 1 || !console.LaunchedFromExplorer() {
		return false
	}

	// Already set up -> behave like a desktop app: open the panel, and
	// hide our console so only the panel window shows. A newer exe than
	// the installed one upgrades the install first (visibly).
	if a, err := app.New(); err == nil && a.State.Config.GlobalPHP != "" {
		selfUpdateIfNeeded(a)
		console.HideWindow()
		if err := ui.Run(context.Background()); err != nil {
			console.ShowWindow()
			fmt.Fprintln(os.Stderr, "Error:", err)
			fmt.Print("\nPress Enter to close this window...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		return true
	}

	// Written directly rather than via fmt.Println: the text contains
	// %USERPROFILE%, which the Print* checkers read as a format directive.
	os.Stdout.WriteString(`Mullion — PHP version manager & local dev server for Windows

This is a command-line tool: normally you use it from a terminal
(CMD or PowerShell), e.g.:

    mullion setup
    mullion php install 8.3
    mullion use 8.3

Since you opened it by double-clicking, I can run first-time setup
for you right now. It will:
  - create %USERPROFILE%\.mullion
  - download the Caddy web server
  - copy mullion.exe there and add it to your PATH
  - install the latest PHP and make it the system default
  - install Composer (available as ` + "`composer`" + `)
  - install and start the latest MySQL (root, no password)
  - serve phpMyAdmin at https://phpmyadmin.test
  - optionally start Mullion automatically when you sign in

Setup asks for administrator rights once (a single UAC prompt) and
continues in a new window.
`)

	fmt.Print("\nRun setup now? [Y/n] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))

	if answer == "" || answer == "y" || answer == "yes" {
		fmt.Println()
		setupCmd.SetContext(context.Background())
		if err := setupCmd.RunE(setupCmd, nil); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
	} else {
		fmt.Println("\nOK — open CMD or PowerShell and run `mullion` from there.")
	}

	fmt.Print("\nPress Enter to close this window...")
	_, _ = reader.ReadString('\n')
	return true
}

// maybeOfferFirstRunSetup handles a bare `mullion` in a terminal before
// setup has ever run (the typical first moment after `brew install
// mullion`): instead of dumping the command list, offer to set the
// stack up right away — the same welcome Windows users get when they
// double-click the exe. Returns true when it handled the launch.
func maybeOfferFirstRunSetup() bool {
	if len(os.Args) > 1 || !console.Interactive() {
		return false
	}
	a, err := app.New()
	if err != nil || a.State.Config.GlobalPHP != "" {
		return false
	}

	fmt.Println("Welcome to Mullion — your local PHP dev stack. It isn't set up on this machine yet.")
	fmt.Println("")
	fmt.Println("First-time setup will:")
	fmt.Println("  - install the Caddy web server and the latest PHP (system default)")
	fmt.Println("  - install Composer and the latest MySQL (root, no password)")
	fmt.Println("  - serve phpMyAdmin at https://phpmyadmin.test")
	fmt.Println("  - put mullion, php, and composer on your PATH")
	fmt.Println("")
	if !askYesNo("Run setup now?", true) {
		fmt.Println("\nOK — run `mullion setup` whenever you're ready, or `mullion --help` for all commands.")
		return true
	}
	fmt.Println("")
	setupCmd.SetContext(context.Background())
	if err := setupCmd.RunE(setupCmd, nil); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	return true
}
