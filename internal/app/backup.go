package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pm/internal/devserver"
	"pm/internal/mongodb"
	"pm/internal/mysql"
	"pm/internal/pgadmin"
	"pm/internal/pmdir"
	"pm/internal/postgres"
)

// phpMyAdminSite and mongoExpressSite are the fixed site names
// phpmyadmin.EnsureLinked and mongoexpress.Ensure link themselves as.
// They're duplicated here (rather than imported from those packages)
// because both internal/phpmyadmin and internal/mongoexpress import
// this package (App) to do their install-and-link work — importing
// them back would be a cycle. Same reasoning for mongoExpressDir below,
// which mirrors mongoexpress.Dir(paths).
const (
	phpMyAdminSite   = "phpmyadmin"
	mongoExpressSite = "mongo"
)

func mongoExpressDir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "mongo-express") }

// BackupInfo describes one backup on disk, for the control panel's
// backups list.
type BackupInfo struct {
	Engine string   `json:"engine"`
	Name   string   `json:"name"` // dir basename, e.g. 2026-09-24_153000-postgres
	Dir    string   `json:"dir"`
	Time   string   `json:"time"` // RFC3339
	Files  []string `json:"files"`
	Size   int64    `json:"size"`
}

// AdminToolInfo describes one of the three admin tools (phpMyAdmin,
// pgAdmin 4, mongo-express) for the control panel's top bar.
type AdminToolInfo struct {
	Name            string `json:"name"` // phpmyadmin|pgadmin|mongo-express
	Label           string `json:"label"`
	Installed       bool   `json:"installed"`
	EngineInstalled bool   `json:"engineInstalled"`
	Kind            string `json:"kind"` // "site"|"desktop"
	URL             string `json:"url,omitempty"`
}

// engineBackupSuffixes are the exact suffixes BackupEngine's directory
// names end in — also what tells ListBackups/RestoreBackup a directory
// under BackupsDir() is one of these (as opposed to, say, the
// "<timestamp>-migrate-<version>" directories `mullion mysql install
// <other-version>` leaves when it migrates databases across a version
// switch, which are a different feature and not listed here).
var engineBackupSuffixes = []string{"mysql", "postgres", "mongo"}

// newBackupDir names a fresh backup directory: <timestamp>-<engine>,
// sorted newest-first by plain string comparison.
func newBackupDir(paths pmdir.Paths, engine string) string {
	name := time.Now().Format("2006-01-02_150405") + "-" + engine
	return filepath.Join(paths.BackupsDir(), name)
}

// backupEngineFromName reports which engine a backup directory's
// basename belongs to, if any.
func backupEngineFromName(name string) (string, bool) {
	for _, engine := range engineBackupSuffixes {
		if strings.HasSuffix(name, "-"+engine) {
			return engine, true
		}
	}
	return "", false
}

// listBackupFiles lists the regular files directly inside dir (backups
// are always flat) and their total size.
func listBackupFiles(dir string) ([]string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	var files []string
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, e.Name())
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	sort.Strings(files)
	return files, total
}

// withEngineTempRunning starts an engine if it isn't already running,
// runs fn, then stops it again if this call was the one that started
// it — leaving the persisted Stopped config flag untouched either way,
// since this is a transient state change for a backup/restore, not a
// user decision to start or stop the server.
func withEngineTempRunning(wasRunning bool, start, stop func() error, fn func() error) error {
	if !wasRunning {
		if err := start(); err != nil {
			return err
		}
	}
	err := fn()
	if !wasRunning {
		if stopErr := stop(); stopErr != nil && err == nil {
			err = stopErr
		}
	}
	return err
}

// BackupEngine dumps every user database of the given engine
// ("mysql"|"postgres"|"mongo") into a fresh timestamped directory under
// paths.BackupsDir(), starting the server temporarily if it's stopped
// (restoring its prior running state afterwards — see
// withEngineTempRunning) and installing the MongoDB Database Tools on
// demand. Returns the backup directory.
func (a *App) BackupEngine(ctx context.Context, engine string) (string, error) {
	switch engine {
	case "mysql":
		return a.backupMySQL()
	case "postgres":
		return a.backupPostgres()
	case "mongo":
		return a.backupMongo(ctx)
	default:
		return "", fmt.Errorf("unknown engine %q (want mysql, postgres, or mongo)", engine)
	}
}

func (a *App) backupMySQL() (string, error) {
	v := a.State.Config.MySQL
	if v == "" {
		return "", fmt.Errorf("MySQL is not installed")
	}
	dir := newBackupDir(a.Paths, "mysql")
	var dbs []string
	err := withEngineTempRunning(mysql.Running(),
		func() error {
			if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
				return err
			}
			return mysql.Start(a.Paths, v)
		},
		func() error { return mysql.Stop(a.Paths, v) },
		func() error {
			var err error
			dbs, err = mysql.UserDatabases(a.Paths, v)
			if err != nil {
				return err
			}
			if len(dbs) == 0 {
				return os.MkdirAll(dir, 0o755)
			}
			return mysql.BackupTo(a.Paths, v, dbs, dir)
		})
	if err != nil {
		return "", err
	}
	return dir, nil
}

func (a *App) backupPostgres() (string, error) {
	v := a.State.Config.Postgres
	if v == "" {
		return "", fmt.Errorf("PostgreSQL is not installed")
	}
	dir := newBackupDir(a.Paths, "postgres")
	err := withEngineTempRunning(postgres.Running(),
		func() error { return postgres.Start(a.Paths, v) },
		func() error { return postgres.Stop(a.Paths, v) },
		func() error {
			_, err := postgres.BackupTo(a.Paths, v, dir)
			return err
		})
	if err != nil {
		return "", err
	}
	return dir, nil
}

func (a *App) backupMongo(ctx context.Context) (string, error) {
	v := a.State.Config.Mongo
	if v == "" {
		return "", fmt.Errorf("MongoDB is not installed")
	}
	if !mongodb.ToolsInstalled(a.Paths) {
		if err := mongodb.InstallTools(ctx, a.Paths); err != nil {
			return "", err
		}
	}
	dir := newBackupDir(a.Paths, "mongo")
	err := withEngineTempRunning(mongodb.Running(),
		func() error { return mongodb.Start(a.Paths, v) },
		func() error { return mongodb.Stop(a.Paths, v) },
		func() error {
			_, err := mongodb.BackupTo(a.Paths, dir)
			return err
		})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// ListBackups lists every backup BackupEngine created, newest first.
func (a *App) ListBackups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(a.Paths.BackupsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		engine, ok := backupEngineFromName(e.Name())
		if !ok {
			continue
		}
		dir := filepath.Join(a.Paths.BackupsDir(), e.Name())
		var t time.Time
		if info, err := e.Info(); err == nil {
			t = info.ModTime()
		}
		files, size := listBackupFiles(dir)
		out = append(out, BackupInfo{
			Engine: engine,
			Name:   e.Name(),
			Dir:    dir,
			Time:   t.UTC().Format(time.RFC3339),
			Files:  files,
			Size:   size,
		})
	}
	// Names start with a sortable timestamp, so newest-first is just a
	// reverse lexicographic sort.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// RestoreBackup restores a backup BackupEngine created. db == "" means
// every database in the backup; a name restores just that one. dir's
// basename must end in -mysql, -postgres or -mongo (as BackupEngine
// names them) so the right engine's restore logic is used.
// DeleteBackup permanently removes one backup directory. It refuses
// anything that isn't a mullion backup sitting directly inside
// BackupsDir() — a real directory (not a symlink) whose name ends in
// -mysql, -postgres or -mongo — so a bad path can't delete elsewhere.
func (a *App) DeleteBackup(dir string) error {
	clean := filepath.Clean(dir)
	root := filepath.Clean(a.Paths.BackupsDir())
	if filepath.Dir(clean) != root {
		return fmt.Errorf("%s is not inside the backups folder %s", dir, root)
	}
	if _, ok := backupEngineFromName(filepath.Base(clean)); !ok {
		return fmt.Errorf("%s doesn't look like a mullion backup", filepath.Base(clean))
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a backup directory", clean)
	}
	return os.RemoveAll(clean)
}

func (a *App) RestoreBackup(ctx context.Context, dir, db string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a backup directory", dir)
	}
	engine, ok := backupEngineFromName(filepath.Base(dir))
	if !ok {
		return fmt.Errorf("%s doesn't look like a mullion backup (expected a name ending in -mysql, -postgres, or -mongo)", filepath.Base(dir))
	}
	switch engine {
	case "mysql":
		return a.restoreMySQLBackup(dir, db)
	case "postgres":
		return a.restorePostgresBackup(dir, db)
	case "mongo":
		return a.restoreMongoBackup(ctx, dir, db)
	default:
		return fmt.Errorf("unknown engine %q", engine)
	}
}

func (a *App) restoreMySQLBackup(dir, db string) error {
	v := a.State.Config.MySQL
	if v == "" {
		return fmt.Errorf("MySQL is not installed")
	}
	if err := mysql.EnsureInitialized(a.Paths, v); err != nil {
		return err
	}
	if err := mysql.Start(a.Paths, v); err != nil {
		return err
	}
	if db != "" {
		file := filepath.Join(dir, db+".sql")
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("no %s.sql in %s", db, dir)
		}
		return mysql.RestoreFile(a.Paths, v, file)
	}
	if all := filepath.Join(dir, "all-databases.sql"); fileExists(all) {
		return mysql.RestoreFile(a.Paths, v, all)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	restored := false
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
			continue
		}
		if err := mysql.RestoreFile(a.Paths, v, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
		restored = true
	}
	if !restored {
		return fmt.Errorf("no .sql files in %s", dir)
	}
	return nil
}

func (a *App) restorePostgresBackup(dir, db string) error {
	v := a.State.Config.Postgres
	if v == "" {
		return fmt.Errorf("PostgreSQL is not installed")
	}
	if err := postgres.Start(a.Paths, v); err != nil {
		return err
	}
	if db != "" {
		file := filepath.Join(dir, db+".dump")
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("no %s.dump in %s", db, dir)
		}
		return postgres.RestoreFile(a.Paths, v, file, db)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	restored := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".dump") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".dump")
		if err := postgres.RestoreFile(a.Paths, v, filepath.Join(dir, e.Name()), name); err != nil {
			return err
		}
		restored = true
	}
	if globals := filepath.Join(dir, "globals.sql"); fileExists(globals) {
		if err := postgres.RestoreGlobals(a.Paths, v, globals); err != nil {
			fmt.Println("note: could not restore globals.sql (roles/tablespaces) -", err)
		}
	}
	if !restored {
		return fmt.Errorf("no .dump files in %s", dir)
	}
	return nil
}

func (a *App) restoreMongoBackup(ctx context.Context, dir, db string) error {
	v := a.State.Config.Mongo
	if v == "" {
		return fmt.Errorf("MongoDB is not installed")
	}
	if !mongodb.ToolsInstalled(a.Paths) {
		if err := mongodb.InstallTools(ctx, a.Paths); err != nil {
			return err
		}
	}
	if err := mongodb.Start(a.Paths, v); err != nil {
		return err
	}
	if db != "" {
		file := filepath.Join(dir, db+".archive.gz")
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("no %s.archive.gz in %s", db, dir)
		}
		return mongodb.RestoreFile(a.Paths, file)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	restored := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".archive.gz") {
			continue
		}
		if err := mongodb.RestoreFile(a.Paths, filepath.Join(dir, e.Name())); err != nil {
			return err
		}
		restored = true
	}
	if !restored {
		return fmt.Errorf("no .archive.gz files in %s", dir)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// removeEngineBinaries deletes an engine's base directory, sparing its
// data ("data" and "data-*" entries — see postgres.BaseDir/mongodb.BaseDir's
// docs) unless deleteData is set.
func removeEngineBinaries(base string, deleteData bool) error {
	if deleteData {
		return os.RemoveAll(base)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "data" || strings.HasPrefix(name, "data-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, name)); err != nil {
			return err
		}
	}
	return nil
}

// engineDataLocation describes where an engine's data is kept, for the
// message UninstallEngine prints when it's told to keep it.
func engineDataLocation(paths pmdir.Paths, engine string) string {
	switch engine {
	case "mysql":
		return paths.MysqlDataDir()
	case "postgres":
		return postgres.BaseDir(paths) + " (data-* subdirectories)"
	case "mongo":
		return mongodb.DataDir(paths)
	default:
		return ""
	}
}

// engineAdminTool maps an engine to the admin tool UninstallEngine also
// removes.
func engineAdminTool(engine string) string {
	switch engine {
	case "mysql":
		return "phpmyadmin"
	case "postgres":
		return "pgadmin"
	case "mongo":
		return "mongo-express"
	default:
		return ""
	}
}

// UninstallEngine stops an engine, optionally backs it up first
// (aborting the whole uninstall if that backup fails — data must never
// be destroyed because a backup silently didn't happen), removes its
// binaries (and, unless deleteData is false, its data), removes its
// admin tool, clears its config, and re-converges the machine. Returns
// the backup directory ("" if backupFirst was false).
func (a *App) UninstallEngine(ctx context.Context, engine string, backupFirst, deleteData bool) (string, error) {
	switch engine {
	case "mysql", "postgres", "mongo":
	default:
		return "", fmt.Errorf("unknown engine %q (want mysql, postgres, or mongo)", engine)
	}

	backupDir := ""
	if backupFirst {
		dir, err := a.BackupEngine(ctx, engine)
		if err != nil {
			return "", fmt.Errorf("backup failed, aborting uninstall: %w", err)
		}
		backupDir = dir
	}

	switch engine {
	case "mysql":
		if v := a.State.Config.MySQL; v != "" {
			if err := mysql.Stop(a.Paths, v); err != nil {
				return backupDir, err
			}
		}
	case "postgres":
		if v := a.State.Config.Postgres; v != "" {
			if err := postgres.Stop(a.Paths, v); err != nil {
				return backupDir, err
			}
		}
	case "mongo":
		if v := a.State.Config.Mongo; v != "" {
			if err := mongodb.Stop(a.Paths, v); err != nil {
				return backupDir, err
			}
		}
	}

	var base string
	switch engine {
	case "mysql":
		base = a.Paths.MysqlDir()
	case "postgres":
		base = postgres.BaseDir(a.Paths)
	case "mongo":
		base = mongodb.BaseDir(a.Paths)
	}
	if err := removeEngineBinaries(base, deleteData); err != nil {
		return backupDir, err
	}
	if !deleteData {
		fmt.Println("Data kept at", engineDataLocation(a.Paths, engine))
	}

	switch engine {
	case "mysql":
		a.State.Config.MySQL = ""
		a.State.Config.MySQLPassword = ""
		a.State.Config.MySQLStopped = false
		mysql.RootPassword = ""
	case "postgres":
		a.State.Config.Postgres = ""
		a.State.Config.PostgresPassword = ""
		a.State.Config.PostgresStopped = false
		postgres.Password = ""
	case "mongo":
		a.State.Config.Mongo = ""
		a.State.Config.MongoStopped = false
	}
	if err := a.State.Save(); err != nil {
		return backupDir, err
	}

	if tool := engineAdminTool(engine); tool != "" {
		if err := a.UninstallAdminTool(tool); err != nil {
			fmt.Println("note: could not remove", tool, "-", err)
		}
	}

	if err := a.applyIfEnabled(); err != nil {
		return backupDir, err
	}
	return backupDir, nil
}

// UninstallAdminTool removes one of the three admin tools: unlinking
// its site (and stopping its dev server, for the node-based ones) and
// deleting its install directory. pgAdmin's own user data (~/.pgadmin)
// is left untouched — only Mullion's copy of the app goes.
func (a *App) UninstallAdminTool(tool string) error {
	switch tool {
	case "phpmyadmin":
		a.State.RemoveSite(phpMyAdminSite)
		if err := os.RemoveAll(a.Paths.PhpMyAdminDir()); err != nil {
			return err
		}
	case "pgadmin":
		if err := pgadmin.Remove(a.Paths); err != nil {
			return err
		}
	case "mongo-express":
		if a.State.FindSite(mongoExpressSite) != nil {
			devserver.Stop(a.Paths, mongoExpressSite)
		}
		a.State.RemoveSite(mongoExpressSite)
		if err := os.RemoveAll(mongoExpressDir(a.Paths)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown admin tool %q (want phpmyadmin, pgadmin, or mongo-express)", tool)
	}
	return a.applyIfEnabled()
}

// AdminTools reports the install/reachability state of all three admin
// tools, for the control panel's top bar.
func (a *App) AdminTools() []AdminToolInfo {
	tld := a.State.Config.TLD
	phpInstalled := a.State.FindSite(phpMyAdminSite) != nil
	meInstalled := a.State.FindSite(mongoExpressSite) != nil
	return []AdminToolInfo{
		{
			Name:            "phpmyadmin",
			Label:           "phpMyAdmin",
			Installed:       phpInstalled,
			EngineInstalled: a.State.Config.MySQL != "",
			Kind:            "site",
			URL:             adminSiteURL(phpInstalled, phpMyAdminSite, tld),
		},
		{
			Name:            "pgadmin",
			Label:           "pgAdmin 4",
			Installed:       pgadmin.Installed(a.Paths),
			EngineInstalled: a.State.Config.Postgres != "",
			Kind:            "desktop",
		},
		{
			Name:            "mongo-express",
			Label:           "mongo-express",
			Installed:       meInstalled,
			EngineInstalled: a.State.Config.Mongo != "",
			Kind:            "site",
			URL:             adminSiteURL(meInstalled, mongoExpressSite, tld),
		},
	}
}

func adminSiteURL(installed bool, name, tld string) string {
	if !installed {
		return ""
	}
	return "https://" + name + "." + tld
}
