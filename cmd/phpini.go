package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/phpver"
)

var phpIniCmd = &cobra.Command{
	Use:   "ini [version]",
	Short: "Show the version's curated php.ini settings; defaults to the global version",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 0)
		if err != nil {
			return err
		}
		settings, err := phpver.ListIni(a.Paths, version)
		if err != nil {
			return err
		}
		fmt.Printf("PHP %s php.ini settings:\n", version)
		for _, s := range settings {
			fmt.Printf("  %s = %s\n", s.Key, s.Value)
		}
		fmt.Printf("\nChange with: mullion php ini set <key> <value> [%s]\n", version)
		return nil
	},
}

var phpIniSetCmd = &cobra.Command{
	Use:   "set <key> <value> [version]",
	Short: "Set a curated php.ini setting (e.g. memory_limit, display_errors) and restart PHP",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 2)
		if err != nil {
			return err
		}
		if err := phpver.SetIni(a.Paths, version, args[0], args[1]); err != nil {
			return err
		}
		// Restart the version's php-fpm/php-cgi so running sites see the change.
		if err := a.RestartPhp(version); err != nil {
			return err
		}
		fmt.Printf("%s = %s for PHP %s.\n", args[0], args[1], version)
		return nil
	},
}

var (
	opcacheOn, opcacheOff, opcacheStatusFlag, opcacheReset bool
	opcacheCLI, opcacheJIT, opcacheJITBuffer               string
	opcacheMemory, opcacheMaxFiles, opcacheRevalidate      int
	opcacheValidate                                        string
)

var phpOpcacheCmd = &cobra.Command{
	Use:   "opcache [version]",
	Short: "Show or change OPcache for a PHP version (defaults to the global one)",
	Long: `Without flags, prints the version's OPcache settings and — when its
php-fpm/php-cgi is running — the live status sites see (hit rate, memory,
cached scripts). Changes are written to that version's php.ini and its
php-fpm/php-cgi is restarted.

For development keep --validate on and --revalidate 0 (Mullion's default
for new installs): PHP then checks every file's timestamp on each request,
so an edit shows on the next reload.`,
	Example: `  mullion php opcache
  mullion php opcache 8.4 --off
  mullion php opcache --on --cli off --jit tracing
  mullion php opcache --memory 256 --revalidate 0
  mullion php opcache --reset`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 0)
		if err != nil {
			return err
		}
		if opcacheOn && opcacheOff {
			return fmt.Errorf("pass --on or --off, not both")
		}
		var c phpver.OpcacheChange
		if opcacheOn || opcacheOff {
			v := opcacheOn
			c.Enabled = &v
		}
		if cmd.Flags().Changed("cli") {
			v, err := parseOnOff(opcacheCLI)
			if err != nil {
				return fmt.Errorf("--cli: %w", err)
			}
			c.CLI = &v
		}
		if cmd.Flags().Changed("jit") {
			j := strings.ToLower(opcacheJIT)
			c.JIT = &j
		}
		if cmd.Flags().Changed("jit-buffer") {
			c.JITBuffer = &opcacheJITBuffer
		}
		if cmd.Flags().Changed("memory") {
			c.Memory = &opcacheMemory
		}
		if cmd.Flags().Changed("max-files") {
			c.MaxFiles = &opcacheMaxFiles
		}
		if cmd.Flags().Changed("validate") {
			v, err := parseOnOff(opcacheValidate)
			if err != nil {
				return fmt.Errorf("--validate: %w", err)
			}
			c.ValidateTimestamps = &v
		}
		if cmd.Flags().Changed("revalidate") {
			c.RevalidateFreq = &opcacheRevalidate
		}
		changed := !c.Empty()
		if changed {
			if err := a.SetOpcache(version, c); err != nil {
				return err
			}
			fmt.Printf("OPcache updated for PHP %s (its php-fpm/php-cgi restarted).\n", version)
		} else if opcacheReset {
			if err := a.ResetOpcache(version); err != nil {
				return err
			}
			fmt.Printf("OPcache reset for PHP %s (its php-fpm/php-cgi restarted).\n", version)
		}
		if changed || opcacheReset {
			if !opcacheStatusFlag {
				return nil
			}
			time.Sleep(300 * time.Millisecond)
		}
		info, err := a.OpcacheInfo(version)
		if err != nil {
			return err
		}
		printOpcache(info)
		return nil
	},
}

func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "1", "true", "yes":
		return true, nil
	case "off", "0", "false", "no":
		return false, nil
	}
	return false, fmt.Errorf("say on or off, not %q", s)
}

func printOpcache(info app.OpcacheInfo) {
	s := info.Settings
	onoff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	jit := s.JIT
	if jit == "custom" {
		jit = "custom (" + s.JITRaw + ")"
	}
	fmt.Printf("PHP %s OPcache  (%s)\n", info.Version, s.IniPath)
	fmt.Printf("  sites (php-fpm/cgi): %s\n", onoff(s.Enabled))
	fmt.Printf("  cli:                 %s\n", onoff(s.CLI))
	fmt.Printf("  jit:                 %s  (buffer %s)\n", jit, s.JITBuffer)
	fmt.Printf("  memory:              %d MB\n", s.Memory)
	fmt.Printf("  max files:           %d\n", s.MaxFiles)
	fmt.Printf("  validate timestamps: %s, every %ds\n", onoff(s.ValidateTimestamps), s.RevalidateFreq)
	fmt.Printf("  note:                %s\n", s.ConfigNote)
	switch {
	case !info.Running:
		fmt.Println("status: no php-fpm/php-cgi runs for this version (no site uses it), so there is no live status")
	case info.StatusError != "":
		fmt.Println("status: could not be read —", info.StatusError)
	case info.Status != nil && !info.Status.Enabled:
		fmt.Printf("status: OPcache is off in %s\n", info.Status.SAPI)
	case info.Status != nil:
		st := info.Status
		fmt.Printf("status (%s): hit rate %.1f%% (%d hits, %d misses), %d scripts cached, %.1f of %.1f MB used",
			st.SAPI, st.HitRate, st.Hits, st.Misses, st.CachedScripts, float64(st.MemoryUsed)/1048576, float64(st.MemoryUsed+st.MemoryFree)/1048576)
		if st.JITEnabled {
			fmt.Printf(", JIT on (%.0f MB buffer)", float64(st.JITBuffer)/1048576)
		}
		fmt.Println()
	}
	if info.JITNote != "" {
		fmt.Println("jit:", info.JITNote)
	}
}

func init() {
	phpIniCmd.AddCommand(phpIniSetCmd)
	phpCmd.AddCommand(phpIniCmd)

	f := phpOpcacheCmd.Flags()
	f.BoolVar(&opcacheOn, "on", false, "enable OPcache for sites (opcache.enable=1)")
	f.BoolVar(&opcacheOff, "off", false, "disable OPcache for sites (opcache.enable=0)")
	f.StringVar(&opcacheCLI, "cli", "", "on|off — OPcache for the php CLI (opcache.enable_cli)")
	f.StringVar(&opcacheJIT, "jit", "", "off|tracing|function — the JIT mode (opcache.jit)")
	f.StringVar(&opcacheJITBuffer, "jit-buffer", "", "JIT buffer size, e.g. 64M (opcache.jit_buffer_size)")
	f.IntVar(&opcacheMemory, "memory", 0, "shared memory in MB (opcache.memory_consumption)")
	f.IntVar(&opcacheMaxFiles, "max-files", 0, "opcache.max_accelerated_files")
	f.StringVar(&opcacheValidate, "validate", "", "on|off — re-check file timestamps (opcache.validate_timestamps)")
	f.IntVar(&opcacheRevalidate, "revalidate", 0, "seconds between timestamp checks (opcache.revalidate_freq; 0 = every request)")
	f.BoolVar(&opcacheStatusFlag, "status", false, "print the settings and live status (also after a change)")
	f.BoolVar(&opcacheReset, "reset", false, "empty the cache by restarting the version's php-fpm/php-cgi")
	phpCmd.AddCommand(phpOpcacheCmd)
}
