package app

import (
	"context"
	"fmt"
	"strings"

	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/postgres"
	"pm/internal/vcredist"
)

// shouldSelfHeal reports whether Apply/self-heal/autostart may bring an
// engine back up: only when it is installed (version != "") and the
// user has not explicitly stopped it. This is the one rule that keeps
// an explicit stop "sticking" for MySQL, Postgres and MongoDB alike.
func shouldSelfHeal(version string, stopped bool) bool {
	return version != "" && !stopped
}

// filterLongTermMongoSeries drops MongoDB's rapid-release lines (odd
// minors like 8.1, 8.3, supported only until the next one ships a few
// months later) from a series list, keeping the long-term-supported
// ones (8.0, 7.0, 6.0, ...) for a version picker. It never runs on a
// full version the user types by hand — ResolveVersion/AvailableSeries
// handle those directly.
func filterLongTermMongoSeries(series []string) []string {
	out := make([]string, 0, len(series))
	for _, s := range series {
		_, minor, ok := strings.Cut(s, ".")
		if ok && minor != "0" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// PostgresSeries lists the installable PostgreSQL major series, newest
// first, for a version picker.
func (a *App) PostgresSeries(ctx context.Context) ([]string, error) {
	return postgres.AvailableSeries(ctx)
}

// MongoSeries lists the installable long-term MongoDB series, newest
// first, for a version picker (rapid releases are filtered out — see
// filterLongTermMongoSeries).
func (a *App) MongoSeries(ctx context.Context) ([]string, error) {
	all, err := mongodb.AvailableSeries(ctx)
	if err != nil {
		return nil, err
	}
	return filterLongTermMongoSeries(all), nil
}

// InstallPostgres resolves the version argument, installs it,
// initializes its data directory, activates it in the config (clearing
// any prior stop), and starts it. Returns the resolved full version.
//
// Switching to a different major stops the previously configured one
// first — Postgres data directories are per-major, so the old data is
// left untouched on disk (its location is printed).
func (a *App) InstallPostgres(ctx context.Context, arg string) (string, error) {
	version, err := postgres.ResolveVersion(ctx, arg)
	if err != nil {
		return "", err
	}

	prev := a.State.Config.Postgres
	switchingMajor := prev != "" && postgres.Major(prev) != postgres.Major(version)
	if switchingMajor {
		fmt.Printf("Stopping %s to switch to %s...\n", postgres.Label(prev), postgres.Label(version))
		if err := postgres.Stop(a.Paths, prev); err != nil {
			return "", err
		}
	}

	if err := postgres.Install(ctx, a.Paths, version); err != nil {
		return "", err
	}
	if err := postgres.EnsureInitialized(a.Paths, version); err != nil {
		return "", err
	}

	if switchingMajor {
		fmt.Printf("Old data directory for %s kept at %s\n", postgres.Label(prev), postgres.DataDir(a.Paths, prev))
	}

	a.State.Config.Postgres = version
	a.State.Config.PostgresStopped = false
	if err := a.State.Save(); err != nil {
		return "", err
	}
	if err := postgres.Start(a.Paths, version); err != nil {
		return "", err
	}
	return version, nil
}

// StartPostgres starts the configured Postgres server and clears the
// stopped flag, so self-heal keeps it up from now on.
func (a *App) StartPostgres() error {
	v := a.State.Config.Postgres
	if v == "" {
		return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.PostgresStopped = false
	return a.State.Save()
}

// StopPostgres stops the configured Postgres server and records that
// the user stopped it on purpose, so Apply/self-heal/autostart leave it
// down until StartPostgres runs again.
func (a *App) StopPostgres() error {
	v := a.State.Config.Postgres
	if v == "" {
		return nil
	}
	if err := postgres.Stop(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.PostgresStopped = true
	return a.State.Save()
}

// PostgresDatabases lists the user databases on the configured server,
// starting it first if needed.
func (a *App) PostgresDatabases() ([]string, error) {
	v := a.State.Config.Postgres
	if v == "" {
		return nil, fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return nil, err
	}
	return postgres.UserDatabases(a.Paths, v)
}

// CreatePostgresDB creates a database on the configured server,
// starting it first if needed.
func (a *App) CreatePostgresDB(name string) error {
	v := a.State.Config.Postgres
	if v == "" {
		return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return err
	}
	return postgres.CreateDatabase(a.Paths, v, name)
}

// DropPostgresDB deletes a database (and all its data) on the
// configured server, starting it first if needed.
func (a *App) DropPostgresDB(name string) error {
	v := a.State.Config.Postgres
	if v == "" {
		return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return err
	}
	return postgres.DropDatabase(a.Paths, v, name)
}

// SetPostgresPassword changes the superuser password ("" removes it),
// starting the server first if needed, and persists it in the config.
func (a *App) SetPostgresPassword(pw string) error {
	v := a.State.Config.Postgres
	if v == "" {
		return fmt.Errorf("PostgreSQL is not installed (run: mullion postgres install)")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return err
	}
	if err := postgres.SetPassword(a.Paths, v, pw); err != nil {
		return err
	}
	a.State.Config.PostgresPassword = pw
	return a.State.Save()
}

// InstallMongo resolves the version argument, installs it (fetching the
// Windows Visual C++ runtime first, a no-op on other platforms),
// initializes its data directory, activates it in the config (clearing
// any prior stop), and starts it. Returns the resolved full version.
func (a *App) InstallMongo(ctx context.Context, arg string) (string, error) {
	version, err := mongodb.ResolveVersion(ctx, arg)
	if err != nil {
		return "", err
	}
	// mongod.exe needs the Visual C++ runtime; unlike postgres.Install,
	// mongodb.Install does not fetch it itself. Ensure is a no-op on
	// non-Windows platforms.
	if err := vcredist.Ensure(ctx, a.Paths); err != nil {
		return "", err
	}
	if err := mongodb.Install(ctx, a.Paths, version); err != nil {
		return "", err
	}
	if err := mongodb.EnsureInitialized(a.Paths, version); err != nil {
		return "", err
	}

	a.State.Config.Mongo = version
	a.State.Config.MongoStopped = false
	if err := a.State.Save(); err != nil {
		return "", err
	}
	if err := mongodb.Start(a.Paths, version); err != nil {
		return "", err
	}
	return version, nil
}

// StartMongo starts the configured MongoDB server and clears the
// stopped flag, so self-heal keeps it up from now on.
func (a *App) StartMongo() error {
	v := a.State.Config.Mongo
	if v == "" {
		return fmt.Errorf("MongoDB is not installed (run: mullion mongo install)")
	}
	if err := mongodb.Start(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.MongoStopped = false
	return a.State.Save()
}

// StopMongo stops the configured MongoDB server and records that the
// user stopped it on purpose, so Apply/self-heal/autostart leave it
// down until StartMongo runs again.
func (a *App) StopMongo() error {
	v := a.State.Config.Mongo
	if v == "" {
		return nil
	}
	if err := mongodb.Stop(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.MongoStopped = true
	return a.State.Save()
}

// MongoDatabases lists the user databases on the configured server,
// starting it first if needed.
func (a *App) MongoDatabases() ([]string, error) {
	v := a.State.Config.Mongo
	if v == "" {
		return nil, fmt.Errorf("MongoDB is not installed (run: mullion mongo install)")
	}
	if err := mongodb.Start(a.Paths, v); err != nil {
		return nil, err
	}
	return mongodb.UserDatabases(a.Paths)
}

// CreateMongoDB creates a database on the configured server, starting
// it first if needed.
func (a *App) CreateMongoDB(name string) error {
	v := a.State.Config.Mongo
	if v == "" {
		return fmt.Errorf("MongoDB is not installed (run: mullion mongo install)")
	}
	if err := mongodb.Start(a.Paths, v); err != nil {
		return err
	}
	return mongodb.CreateDatabase(a.Paths, name)
}

// DropMongoDB deletes a database (and all its data) on the configured
// server, starting it first if needed.
func (a *App) DropMongoDB(name string) error {
	v := a.State.Config.Mongo
	if v == "" {
		return fmt.Errorf("MongoDB is not installed (run: mullion mongo install)")
	}
	if err := mongodb.Start(a.Paths, v); err != nil {
		return err
	}
	return mongodb.DropDatabase(a.Paths, name)
}

// StartMySQL starts the configured MySQL server and clears the stopped
// flag, so self-heal keeps it up from now on. It mirrors StartPostgres
// / StartMongo so all three engines behave the same way.
func (a *App) StartMySQL() error {
	v := a.State.Config.MySQL
	if v == "" {
		return fmt.Errorf("MySQL is not installed (run: mullion mysql install)")
	}
	if err := mysql.Start(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.MySQLStopped = false
	return a.State.Save()
}

// StopMySQL stops the configured MySQL server and records that the
// user stopped it on purpose, so Apply/self-heal/autostart leave it
// down until StartMySQL runs again.
func (a *App) StopMySQL() error {
	v := a.State.Config.MySQL
	if v == "" {
		return nil
	}
	if err := mysql.Stop(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.MySQLStopped = true
	return a.State.Save()
}
