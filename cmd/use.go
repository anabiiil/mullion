package cmd

import (
	"fmt"
	"runtime"

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
			fmt.Print(shadowWarning(shadow))
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(useCmd)
}

// shadowWarning explains a foreign php beating Mullion's on the PATH,
// with platform-appropriate directions.
func shadowWarning(shadow string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`
WARNING: 'php' currently resolves to
  %s
which shadows the version Mullion just activated. Another PHP install sits
earlier on your PATH — usually in the SYSTEM path, which Windows always
searches before the user PATH where Mullion lives. Remove that directory from
the system Path (Settings > System > About > Advanced system settings >
Environment Variables), then open a new terminal.
`, shadow)
	}
	return fmt.Sprintf(`
WARNING: 'php' currently resolves to
  %s
which shadows the version Mullion just activated. Another PHP install
(Homebrew?) sits earlier on your PATH. Open a NEW terminal so Mullion's
PATH entry takes effect, or remove the other php from your PATH.
`, shadow)
}
