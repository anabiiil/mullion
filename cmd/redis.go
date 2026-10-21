package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/redis"
	"pm/internal/term"
)

var redisCmd = &cobra.Command{
	Use:   "redis",
	Short: "Redis — a local Redis server for cache, sessions and queues",
	Long: `Redis runs on 127.0.0.1:6379 (no password) and starts with the rest of
Mullion. On macOS it is built from the official source with the Xcode
Command Line Tools (about a minute, on install only); on Windows Mullion
downloads the portable build from github.com/redis-windows/redis-windows.

Its data is snapshotted to dump.rdb (within 5 minutes of a write, and on
every stop); ` + "`mullion redis env`" + ` prints the Laravel .env lines.`,
	RunE: func(cmd *cobra.Command, args []string) error { return redisStatus(mustApp()) },
}

var redisInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install (or upgrade to) the latest Redis and start it",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		prev := a.State.Config.Redis
		v, err := a.InstallRedis(cmd.Context(), func(s string) { fmt.Println(s) })
		if err != nil {
			return err
		}
		switch {
		case prev == "":
			fmt.Println(term.Green("✓ Redis is running:"), redis.Label(v))
		case prev != v:
			fmt.Printf("%s %s → %s\n", term.Green("✓ Redis upgraded:"), prev, v)
		default:
			fmt.Println(term.Green("✓"), redis.Label(v), "is already the latest — it's running.")
		}
		return redisStatus(a)
	},
}

var (
	redisUninstallKeep, redisUninstallNoBackup, redisUninstallYes bool
)

var redisUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Back up Redis, then remove it and phpRedisAdmin (--keep-data keeps dump.rdb)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if a.State.Config.Redis == "" {
			fmt.Println("Redis is not installed.")
			return nil
		}
		q := "Remove Redis and all its data?"
		if redisUninstallKeep {
			q = "Remove Redis (keeping its data)?"
		}
		proceed, err := confirmDestructive(redisUninstallYes, q)
		if err != nil || !proceed {
			return err
		}
		dir, err := a.UninstallEngine(cmd.Context(), "redis", !redisUninstallNoBackup, !redisUninstallKeep)
		if dir != "" {
			fmt.Println("Backup:", dir)
		}
		if err != nil {
			return err
		}
		fmt.Println("Redis removed.")
		return nil
	},
}

var redisStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Redis",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.StartRedis(); err != nil {
			return err
		}
		return redisStatus(a)
	},
}

var redisStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Redis (saving a snapshot; it stays stopped until `mullion redis start`)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := mustApp().StopRedis(); err != nil {
			return err
		}
		fmt.Println("Redis stopped.")
		return nil
	},
}

var redisRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart Redis",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.RestartRedis(); err != nil {
			return err
		}
		return redisStatus(a)
	},
}

var redisStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Redis's state, memory and keys per database",
	RunE:  func(cmd *cobra.Command, args []string) error { return redisStatus(mustApp()) },
}

func redisStatus(a *app.App) error {
	c := a.State.Config
	if c.Redis == "" {
		fmt.Println("Redis is not installed (run: mullion redis install)")
		return nil
	}
	fmt.Println("redis:    " + redisStatusLine(c.Redis, redis.Owned(a.Paths) && redis.Running(), c.RedisStopped))
	if u := a.RedisAdminURL(-1); u != "" {
		fmt.Println("web ui:   " + u)
	}
	cl, err := redis.Connect(2 * time.Second)
	if err != nil {
		return nil
	}
	defer cl.Close()
	info, err := cl.Info("")
	if err != nil {
		return nil
	}
	fmt.Printf("memory:   %s (max %dmb, %s)\n", info["used_memory_human"], redis.MaxMemoryMB, info["maxmemory_policy"])
	fmt.Printf("clients:  %s   ops/sec: %s   hit rate: %s\n", info["connected_clients"], info["instantaneous_ops_per_sec"], hitRate(info))
	ks := redis.ParseKeyspace(info)
	if len(ks) == 0 {
		fmt.Println("keys:     none")
	}
	for _, d := range ks {
		fmt.Printf("db%-2d      %d keys (%d with a TTL)\n", d.DB, d.Keys, d.Expires)
	}
	return nil
}

// hitRate renders keyspace_hits/(hits+misses).
func hitRate(info map[string]string) string {
	h, _ := strconv.ParseFloat(info["keyspace_hits"], 64)
	m, _ := strconv.ParseFloat(info["keyspace_misses"], 64)
	if h+m == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", h*100/(h+m))
}

// redisStatusLine renders Redis's `mullion status` line.
func redisStatusLine(version string, running, stoppedByUser bool) string {
	state := term.Red("stopped") + " (run `mullion redis start`)"
	switch {
	case running:
		state = term.Green("running")
	case stoppedByUser:
		state = term.Red("stopped") + " by you (run `mullion redis start`)"
	}
	return fmt.Sprintf("%-14s port %d  %s", "redis "+version, redis.Port, state)
}

var redisCliCmd = &cobra.Command{
	Use:                "cli [redis-cli arguments]",
	Short:              "Open redis-cli on Mullion's Redis (extra arguments are passed through)",
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		v := a.State.Config.Redis
		if v == "" {
			return redis.ErrNotInstalled
		}
		cliArgs := append([]string{"-h", "127.0.0.1", "-p", strconv.Itoa(redis.Port)}, args...)
		c := exec.Command(redis.CliExe(a.Paths, v), cliArgs...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			return err
		}
		return nil
	},
}

var (
	redisFlushDB  int
	redisFlushYes bool
)

var redisFlushCmd = &cobra.Command{
	Use:   "flush",
	Short: "Delete every key (FLUSHALL), or one database's with --db N (FLUSHDB)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		what := "every key in every database"
		if redisFlushDB >= 0 {
			what = fmt.Sprintf("every key in database %d", redisFlushDB)
		}
		proceed, err := confirmDestructive(redisFlushYes, "Delete "+what+"?")
		if err != nil || !proceed {
			return err
		}
		if err := a.FlushRedis(redisFlushDB); err != nil {
			return err
		}
		fmt.Println("Flushed", what+".")
		return nil
	},
}

var redisBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Snapshot Redis (SAVE) and copy dump.rdb into the backups folder",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := mustApp().BackupEngine(cmd.Context(), "redis")
		if err != nil {
			return err
		}
		fmt.Println("Backed up to", dir)
		return nil
	},
}

var redisRestoreYes bool

var redisRestoreCmd = &cobra.Command{
	Use:   "restore <backup folder | dump.rdb>",
	Short: "Replace Redis's data with a backup (the current data is kept as dump.rdb.before-restore)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		path, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		proceed, err := confirmDestructive(redisRestoreYes, "Replace every key in Redis with "+filepath.Base(path)+"?")
		if err != nil || !proceed {
			return err
		}
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			err = a.RestoreBackup(cmd.Context(), path, "")
		} else {
			err = a.RestoreRedisFile(path)
		}
		if err != nil {
			return err
		}
		fmt.Println("Restored.")
		return nil
	},
}

var redisEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Print the Laravel .env lines that use Mullion's Redis",
	RunE: func(cmd *cobra.Command, args []string) error {
		mustApp()
		fmt.Print(app.RedisEnvSnippet(redis.Port))
		fmt.Fprintln(os.Stderr, "# Laravel 11+ reads CACHE_STORE, 10 and older CACHE_DRIVER — keep the one your version uses.")
		return nil
	},
}

var redisOpenDB int

var redisOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open phpRedisAdmin at https://redis.<tld> (installing it first if needed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		u, err := a.EnsureRedisAdmin(cmd.Context(), redisOpenDB)
		if err != nil {
			return err
		}
		fmt.Println("phpRedisAdmin:", u)
		openURL(u)
		return nil
	},
}

func init() {
	redisUninstallCmd.Flags().BoolVar(&redisUninstallKeep, "keep-data", false, "keep dump.rdb for a reinstall")
	redisUninstallCmd.Flags().BoolVar(&redisUninstallNoBackup, "no-backup", false, "skip the backup taken first")
	redisUninstallCmd.Flags().BoolVarP(&redisUninstallYes, "yes", "y", false, "don't ask for confirmation")
	redisFlushCmd.Flags().IntVar(&redisFlushDB, "db", -1, "only this database (0-15)")
	redisFlushCmd.Flags().BoolVarP(&redisFlushYes, "yes", "y", false, "don't ask for confirmation")
	redisRestoreCmd.Flags().BoolVarP(&redisRestoreYes, "yes", "y", false, "don't ask for confirmation")
	redisOpenCmd.Flags().IntVar(&redisOpenDB, "db", -1, "open this database")
	redisCmd.AddCommand(redisInstallCmd, redisUninstallCmd, redisStartCmd, redisStopCmd, redisRestartCmd,
		redisStatusCmd, redisCliCmd, redisFlushCmd, redisBackupCmd, redisRestoreCmd, redisEnvCmd, redisOpenCmd)
	rootCmd.AddCommand(redisCmd)
}
