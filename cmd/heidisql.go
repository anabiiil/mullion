package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/heidisql"
)

var heidisqlCmd = &cobra.Command{
	Use:   "heidisql",
	Short: "Install (if needed) and open the HeidiSQL desktop client",
	Long: `Downloads the latest portable HeidiSQL into ~/.mullion/heidisql on
first use — preconfigured with a session for the local MySQL server
(127.0.0.1, root, no password) — and opens it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if !heidisql.Installed(a.Paths) {
			if err := heidisql.Install(cmd.Context(), a.Paths); err != nil {
				return err
			}
		}
		if err := heidisql.Launch(a.Paths); err != nil {
			return err
		}
		fmt.Println("HeidiSQL opened — use the `Mullion` session to connect.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(heidisqlCmd)
}
