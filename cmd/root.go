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
	Short: "The zero-setup local dev environment (PHP, Node, MySQL, .test domains, HTTPS)",
	Long: `Mullion — a local PHP & frontend development environment for
Windows and macOS.

Install and switch PHP and Node versions system-wide or per project,
serve local projects on .test domains through Caddy (frontend projects
get a managed dev server — just open the link), and secure them with
trusted local HTTPS certificates.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       version.Number,
}

func Execute() {
	term.Init()
	maybeExecFresher()
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
