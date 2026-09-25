package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [site] -- <command...>",
	Short: "Run a command in a site's folder with the site's PHP and Node",
	Long: `Runs one command in a linked site's folder with the site's own PHP
version, Node version and composer first on PATH — exactly what serves
the site, even when it is pinned to a different version than the global
one. Without a site name, the site linked to the current folder is used.`,
	Example: `  mullion run shop -- php artisan migrate
  mullion run shop -- npm run build
  mullion run -- composer install`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dash := cmd.ArgsLenAtDash()
		var siteArgs, command []string
		if dash < 0 {
			// No "--": the first argument is the site, the rest the command.
			if len(args) < 2 {
				return fmt.Errorf("usage: mullion run <site> -- <command...>")
			}
			siteArgs, command = args[:1], args[1:]
		} else {
			siteArgs, command = args[:dash], args[dash:]
		}
		if len(siteArgs) > 1 {
			return fmt.Errorf("expected one site name before --, got %d", len(siteArgs))
		}
		if len(command) == 0 {
			return fmt.Errorf("no command given (mullion run <site> -- <command...>)")
		}
		a := mustApp()
		site, err := siteNameFromArgOrCwd(a, siteArgs)
		if err != nil {
			return err
		}
		c, err := a.ProjectCommandCmd(site, shellJoin(command), true)
		if err != nil {
			return err
		}
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		// Ctrl+C goes to the command (same terminal); we just wait for
		// it and pass its exit status on.
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		defer signal.Stop(sigs)
		err = c.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			if code < 0 {
				code = 130
			}
			os.Exit(code)
		}
		return err
	},
}

// shellJoin rebuilds one shell command line from separate arguments,
// quoting the ones the shell would otherwise split or interpret. A
// single argument is taken as a ready-made command line
// (mullion run shop -- "php artisan migrate && npm run build").
func shellJoin(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'`$&|;<>()*?!~#{}[]\\%^") {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func init() {
	rootCmd.AddCommand(runCmd)
}
