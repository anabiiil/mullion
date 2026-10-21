package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/adminer"
	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/mssql"
	"pm/internal/phpver"
)

// errMSSQLNotInstalled is the shared "install it first" error.
var errMSSQLNotInstalled = fmt.Errorf("SQL Server is not installed (run: mullion mssql install)")

// InstallMSSQL installs SQL Server 2025 Developer — a container under
// OrbStack on macOS, a native MULLION instance on Windows — with a
// generated sa password (or the one a kept data volume already uses),
// activates it in the config and leaves it running. progress receives
// short status lines (nil = print them).
func (a *App) InstallMSSQL(ctx context.Context, progress func(string)) (string, error) {
	if progress == nil {
		progress = func(s string) { fmt.Println(s) }
	}
	pw := a.State.Config.MSSQLPassword
	if mssql.ValidPassword(pw) != nil {
		var err error
		if pw, err = mssql.GeneratePassword(); err != nil {
			return "", err
		}
	}
	// Save the password first: if the install dies halfway, the data it
	// created already uses it.
	a.State.Config.MSSQLPassword = pw
	if err := a.State.Save(); err != nil {
		return "", err
	}
	mssql.Password = pw
	if err := mssql.Install(ctx, a.Paths, pw, progress); err != nil {
		return "", err
	}
	a.State.Config.MSSQL = mssql.Series
	a.State.Config.MSSQLStopped = false
	if err := a.State.Save(); err != nil {
		return "", err
	}
	return mssql.Series, nil
}

// StartMSSQL starts SQL Server and clears the stopped flag.
func (a *App) StartMSSQL() error {
	if a.State.Config.MSSQL == "" {
		return errMSSQLNotInstalled
	}
	if err := mssql.Start(a.Paths); err != nil {
		return err
	}
	a.State.Config.MSSQLStopped = false
	return a.State.Save()
}

// StopMSSQL stops SQL Server and records the stop, so Apply/self-heal
// leave it down until StartMSSQL runs again.
func (a *App) StopMSSQL() error {
	if a.State.Config.MSSQL == "" {
		return nil
	}
	if err := mssql.Stop(a.Paths); err != nil {
		return err
	}
	a.State.Config.MSSQLStopped = true
	return a.State.Save()
}

// ensureMSSQLUp starts the server for a one-off operation when needed.
func (a *App) ensureMSSQLUp() error {
	if a.State.Config.MSSQL == "" {
		return errMSSQLNotInstalled
	}
	if mssql.Running() {
		return nil
	}
	return mssql.Start(a.Paths)
}

// MSSQLDatabases lists the user databases, starting the server if needed.
func (a *App) MSSQLDatabases() ([]string, error) {
	if err := a.ensureMSSQLUp(); err != nil {
		return nil, err
	}
	return mssql.UserDatabases(a.Paths)
}

// CreateMSSQLDB creates a database, starting the server if needed.
func (a *App) CreateMSSQLDB(name string) error {
	return a.CreateMSSQLDBWith(name, "")
}

// CreateMSSQLDBWith is CreateMSSQLDB with a collation ("" = the
// server's).
func (a *App) CreateMSSQLDBWith(name, collation string) error {
	if err := a.ensureMSSQLUp(); err != nil {
		return err
	}
	return mssql.CreateDatabaseWith(a.Paths, name, collation)
}

// DropMSSQLDB deletes a database and all its data.
func (a *App) DropMSSQLDB(name string) error {
	if err := a.ensureMSSQLUp(); err != nil {
		return err
	}
	return mssql.DropDatabase(a.Paths, name)
}

// SetMSSQLPassword changes the sa password ("" = generate a new one)
// and persists it. Returns the new password.
func (a *App) SetMSSQLPassword(pw string) (string, error) {
	if err := a.ensureMSSQLUp(); err != nil {
		return "", err
	}
	if pw == "" {
		var err error
		if pw, err = mssql.GeneratePassword(); err != nil {
			return "", err
		}
	}
	if err := mssql.SetPassword(a.Paths, pw); err != nil {
		return "", err
	}
	a.State.Config.MSSQLPassword = pw
	return pw, a.State.Save()
}

// RestoreMSSQL restores a .bak file (db "" = the file's name) or every
// .bak in a folder.
func (a *App) RestoreMSSQL(path, db string) error {
	if err := a.ensureMSSQLUp(); err != nil {
		return err
	}
	path = strings.Trim(strings.TrimSpace(path), `"`)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s does not exist", path)
	}
	if !info.IsDir() {
		return mssql.RestoreFile(a.Paths, path, db)
	}
	return a.restoreMSSQLBackup(path, db)
}

func (a *App) backupMSSQL() (string, error) {
	if a.State.Config.MSSQL == "" {
		return "", fmt.Errorf("SQL Server is not installed")
	}
	dir := newBackupDir(a.Paths, "mssql")
	err := withEngineTempRunning(mssql.Running(),
		func() error { return mssql.Start(a.Paths) },
		func() error { return mssql.Stop(a.Paths) },
		func() error {
			_, err := mssql.BackupTo(a.Paths, dir)
			return err
		})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// mssqlBackupFiles picks the .bak files a restore of dir covers: one
// database (named with or without .bak) or all of them.
func mssqlBackupFiles(dir, db string) ([]string, error) {
	if db != "" {
		name := strings.TrimSuffix(db, ".bak") + ".bak"
		file := filepath.Join(dir, name)
		if _, err := os.Stat(file); err != nil {
			return nil, fmt.Errorf("no %s in %s", name, dir)
		}
		return []string{file}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".bak") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .bak files in %s", dir)
	}
	return files, nil
}

func (a *App) restoreMSSQLBackup(dir, db string) error {
	if err := a.ensureMSSQLUp(); err != nil {
		return err
	}
	files, err := mssqlBackupFiles(dir, db)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := mssql.RestoreFile(a.Paths, f, ""); err != nil {
			return err
		}
	}
	return nil
}

// InstallOrbStack installs OrbStack (macOS) — only on the user's request.
func (a *App) InstallOrbStack(ctx context.Context, progress func(string)) error {
	return mssql.InstallOrbStack(ctx, a.Paths, progress)
}

// EnsureAdminer installs Adminer (keeping an existing copy), links it as
// the secured site adminer.<tld>, converges the machine, and returns the
// URL that opens its SQL Server login.
func (a *App) EnsureAdminer(ctx context.Context) (string, error) {
	if a.State.Config.GlobalPHP == "" {
		return "", fmt.Errorf("Adminer needs PHP: run `mullion php install 8.4` and `mullion use 8.4` first")
	}
	if _, err := adminer.Install(ctx, a.Paths); err != nil {
		return "", err
	}
	if site := a.State.FindSite(adminer.SiteName); site == nil {
		a.State.AddSite(config.Site{Name: adminer.SiteName, Path: adminer.Dir(a.Paths), Secure: true})
	} else {
		site.Secure = true
		site.Path = adminer.Dir(a.Paths)
	}
	if err := a.applyIfEnabled(); err != nil {
		return "", err
	}
	if !a.skipApply {
		// Padlock in the browser (one-time prompt); not fatal.
		_ = caddy.TrustCA(a.Paths)
	}
	return adminer.URL(a.State.Config.TLD, mssql.Port), nil
}

// sqlServerPHPExtensions are the extensions Adminer's MS SQL driver can
// use, in its order of preference (Windows builds get sqlsrv/pdo_sqlsrv,
// macOS builds pdo_dblib over FreeTDS — see phpver.SQLServerExtensions).
var sqlServerPHPExtensions = []string{"sqlsrv", "pdo_sqlsrv", "pdo_dblib"}

// Seams for tests.
var (
	listPhpExtensions    = phpver.ListExtensions
	sqlServerDriverReady = phpver.HasSQLServerDriver
)

// SQLServerPHPExtension reports which SQL Server extension the global
// PHP can use right now ("" = none, or — on Windows — no Microsoft ODBC
// driver behind it: Adminer and Laravel then cannot reach SQL Server).
func (a *App) SQLServerPHPExtension() string {
	v := a.State.Config.GlobalPHP
	if v == "" || !sqlServerDriverReady(a.Paths, v) {
		return ""
	}
	exts, err := listPhpExtensions(a.Paths, v)
	if err != nil {
		return ""
	}
	return pickSQLServerExt(exts)
}

// EnableSQLServerPHP installs/enables the global PHP's SQL Server
// driver (phpver.EnsureSQLServer: pdo_dblib built against FreeTDS on
// macOS, Microsoft's sqlsrv + ODBC Driver 18 on Windows — which may ask
// for administrator rights) and restarts that PHP.
func (a *App) EnableSQLServerPHP(ctx context.Context) error {
	v := a.State.Config.GlobalPHP
	if v == "" {
		return fmt.Errorf("no PHP installed yet (run: mullion php install 8.4)")
	}
	if err := phpver.EnsureSQLServer(ctx, a.Paths, v); err != nil {
		return err
	}
	return a.RestartPhp(v)
}

func pickSQLServerExt(exts []phpver.Ext) string {
	enabled := map[string]bool{}
	for _, e := range exts {
		if e.Enabled {
			enabled[strings.ToLower(e.Name)] = true
		}
	}
	for _, n := range sqlServerPHPExtensions {
		if enabled[n] {
			return n
		}
	}
	return ""
}

// PHPExtensionHint is the panel/doctor hint shown when no SQL Server
// extension is enabled.
const PHPExtensionHint = "Enable the SQL Server PHP extension in PHP → Extensions (or run `mullion php ext get sqlsrv`) — Adminer and Laravel need it to talk to SQL Server"

// MSSQLEnvSnippet is the Laravel .env block for Mullion's SQL Server.
// trustCert adds DB_TRUST_SERVER_CERTIFICATE=true, needed on Windows:
// pdo_sqlsrv over ODBC Driver 18 encrypts by default and SQL Server's
// certificate is self-signed. Laravel's sqlsrv connection reads it as
// 'trust_server_certificate' => env('DB_TRUST_SERVER_CERTIFICATE', 'false')
// (config/database.php). macOS's pdo_dblib (FreeTDS without TLS) never
// encrypts, so it doesn't apply there — which is also why Mullion never
// turns on SQL Server's "force encryption".
func MSSQLEnvSnippet(port int, password string, trustCert bool) string {
	s := fmt.Sprintf("DB_CONNECTION=sqlsrv\nDB_HOST=127.0.0.1\nDB_PORT=%d\nDB_DATABASE=laravel\nDB_USERNAME=sa\nDB_PASSWORD=%s\n", port, password)
	if trustCert {
		s += "DB_TRUST_SERVER_CERTIFICATE=true\n"
	}
	return s
}

// mssqlDoctorChecks is `mullion doctor`'s SQL Server section.
func (a *App) mssqlDoctorChecks() []DoctorCheck {
	c := a.State.Config
	if c.MSSQL == "" {
		return nil
	}
	var checks []DoctorCheck
	st := mssql.Status(a.Paths)
	name := "mssql " + c.MSSQL
	if runtime.GOOS == "darwin" {
		switch {
		case !st.OrbStack && !st.Docker:
			checks = append(checks, DoctorCheck{Name: "orbstack", Status: DoctorFail, Detail: "not installed — SQL Server needs it (`mullion mssql orbstack install`)"})
		case !st.Daemon:
			checks = append(checks, DoctorCheck{Name: "orbstack", Status: DoctorWarn, Detail: "installed but not running — `mullion mssql start` starts it"})
		default:
			checks = append(checks, DoctorCheck{Name: "orbstack", Status: DoctorOK, Detail: "docker engine answers"})
			if !st.Container {
				checks = append(checks, DoctorCheck{Name: name, Status: DoctorWarn, Detail: "container " + mssql.ContainerName + " is missing — `mullion mssql start` recreates it (data is in volume " + mssql.VolumeName + ")"})
			}
		}
	}
	if runtime.GOOS == "windows" && !st.Service {
		checks = append(checks, DoctorCheck{Name: name, Status: DoctorFail, Detail: "service " + mssql.ServiceName + " is missing — reinstall with `mullion mssql install`"})
		return checks
	}
	switch {
	case mssql.Running():
		checks = append(checks, DoctorCheck{Name: name, Status: DoctorOK, Detail: fmt.Sprintf("listening on 127.0.0.1:%d", mssql.Port)})
	case c.MSSQLStopped:
		checks = append(checks, DoctorCheck{Name: name, Status: DoctorWarn, Detail: "stopped by you"})
	default:
		checks = append(checks, DoctorCheck{Name: name, Status: DoctorFail, Detail: "not running — `mullion mssql start`"})
	}
	if a.State.FindSite(adminer.SiteName) != nil || c.GlobalPHP != "" {
		if ext := a.SQLServerPHPExtension(); ext != "" {
			checks = append(checks, DoctorCheck{Name: "php sqlsrv", Status: DoctorOK, Detail: ext + " enabled"})
		} else if c.GlobalPHP != "" {
			checks = append(checks, DoctorCheck{Name: "php sqlsrv", Status: DoctorWarn, Detail: PHPExtensionHint})
		}
	}
	return checks
}
