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
	// hide our console so only the panel window shows.
	if a, err := app.New(); err == nil && a.State.Config.GlobalPHP != "" {
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
