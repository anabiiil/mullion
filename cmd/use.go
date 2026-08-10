package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/phpver"
)

var useCmd = &cobra.Command{
	Use:   "use <version>",
	Short: "Switch the system-wide PHP version (php on PATH)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		sel, err := phpver.ParseSelector(args[0])
		if err != nil {
			return err
		}
		full, err := phpver.FindInstalled(a.Paths, sel)
		if err != nil {
			return err
		}
		if err := a.UseGlobal(full); err != nil {
			return err
		}
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("System PHP is now %s (`php -v` in any new terminal)\n", full)
		if shadow := a.PhpShadow(); shadow != "" {
			fmt.Printf(`
WARNING: 'php' currently resolves to
  %s
which shadows the version Mullion just activated. Another PHP install sits
earlier on your PATH — usually in the SYSTEM path, which Windows always
searches before the user PATH where Mullion lives. Remove that directory from
the system Path (Settings > System > About > Advanced system settings >
Environment Variables), then open a new terminal.
`, shadow)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(useCmd)
}
