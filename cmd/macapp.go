package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"pm/internal/macapp"
)

var macappCmd = &cobra.Command{
	Use:   "app",
	Short: "Install Mullion.app into Applications (macOS)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if runtime.GOOS != "darwin" {
			fmt.Println("mullion app is macOS-only.")
			return nil
		}
		if !macapp.Available() {
			fmt.Println("this build doesn't include the Mac app — build it with `bash macapp/build.sh` then rebuild mullion")
			return nil
		}
		path, err := macapp.Install()
		if err != nil {
			return err
		}
		fmt.Println("Mullion.app installed at", path)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(macappCmd)
}
