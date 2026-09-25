package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/postgres"
)

var backupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "List database backups made with mullion mysql|postgres|mongo backup",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		backups, err := a.ListBackups()
		if err != nil {
			return err
		}
		if len(backups) == 0 {
			fmt.Println("No backups yet. Create one with: mullion mysql backup / mullion postgres backup / mullion mongo backup")
			return nil
		}
		for _, b := range backups {
			fmt.Printf("%-28s %-10s %8.1f MB  %d file(s)\n", b.Name, engineLabel(b.Engine), float64(b.Size)/1e6, len(b.Files))
			fmt.Println("  " + b.Dir)
		}
		return nil
	},
}

// engineLabel renders an engine key ("mysql"|"postgres"|"mongo") for
// humans; shared by every backup/restore/uninstall command.
func engineLabel(engine string) string {
	switch engine {
	case "mysql":
		return "MySQL"
	case "postgres":
		return "PostgreSQL"
	case "mongo":
		return "MongoDB"
	default:
		return engine
	}
}

// engineHasData reports whether an engine is installed and has an
// initialized data directory — used to decide whether it's worth
// offering (or attempting) a backup at all.
func engineHasData(a *app.App, engine string) bool {
	switch engine {
	case "mysql":
		return a.State.Config.MySQL != "" && mysql.DataInitialized(a.Paths)
	case "postgres":
		return a.State.Config.Postgres != "" && postgres.DataInitialized(a.Paths, a.State.Config.Postgres)
	case "mongo":
		return a.State.Config.Mongo != "" && mongodb.DataInitialized(a.Paths)
	default:
		return false
	}
}

// confirmDestructive asks before an irreversible action; yes (--yes)
// skips the prompt and answers true unconditionally. A non-interactive
// session without --yes refuses rather than silently guessing.
func confirmDestructive(yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	if !console.Interactive() {
		return false, fmt.Errorf("refusing without confirmation; pass --yes")
	}
	return askYesNo(question, false), nil
}

func init() {
	rootCmd.AddCommand(backupsCmd)
}
