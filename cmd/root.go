// Package cmd defines the mullion command-line interface.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/term"
	"pm/internal/version"
)

var rootCmd = &cobra.Command{
	Use:   "mullion",
	Short: "PHP version manager & local dev server",
	Long: `Mullion — a local PHP development environment for Windows and macOS.

Install and switch PHP versions system-wide or per project, serve local
projects on .test domains through Caddy, and secure them with trusted
local HTTPS certificates.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       version.Number,
}

func Execute() {
	term.Init()
	if maybeRunExplorerWizard() {
		return
	}
	if maybeOfferFirstRunSetup() {
		return
	}
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, term.Red("Error:"), err)
		os.Exit(1)
	}
}

// mustApp loads paths + state or exits with a clear error.
func mustApp() *app.App {
	a, err := app.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	return a
}
