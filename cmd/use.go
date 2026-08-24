package cmd

import (
	"fmt"
	"path/filepath"
	"runtime"

	"pm/internal/app"
	"pm/internal/console"

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
			reassertPathPriority(a)
			fmt.Print(shadowWarning(shadow))
		}
		return nil
	},
}

// reassertPathPriority makes Mullion win the PATH again when another
// php shadows it: re-writing the managed profile block moves it to the
// END of the shell files, after whatever prepended itself since — so
// the next terminal resolves Mullion's php first. No-op on Windows
// (there the fix is machine-PATH priority, done in setup).
func reassertPathPriority(a *app.App) {
	if runtime.GOOS == "windows" {
		return
	}
	shadow := a.PhpShadow()
	_ = app.EnsureUserPath(a.Paths.BinDir(), a.Paths.CurrentPhp())
	if shadow == "" {
		return
	}
	offerShadowFix(shadow)
}

// offerShadowFix disables the shadowing php's own PATH lines (with
// consent when a human is present), so Mullion wins permanently instead
// of re-fighting the ordering every time.
func offerShadowFix(shadow string) {
	dir := filepath.Dir(shadow)
	if console.Interactive() &&
		!askYesNo(fmt.Sprintf("Disable the PATH entry for %s so Mullion's php always wins?", dir), true) {
		return
	}
	disabled, err := app.DisableShadowEntries(shadow)
	if err != nil {
		fmt.Println("note:", err)
		return
	}
	if len(disabled) > 0 {
		fmt.Println("Disabled the shadowing PATH line(s) — commented with a marker, easy to restore:")
		for _, d := range disabled {
			fmt.Println("  " + d)
		}
	}
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
NOTE: in THIS terminal 'php' still resolves to
  %s
(another PHP install — a version manager or Homebrew). Mullion has
re-asserted its PATH entry so it loads last in your shell profiles:
open a NEW terminal and `+"`php -v`"+` will show Mullion's version. If it
still doesn't, remove the other php's PATH line from your shell config.
`, shadow)
}
