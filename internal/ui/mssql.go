package ui

// The panel's SQL Server (and Adminer) endpoints and state.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"time"

	"pm/internal/adminer"
	"pm/internal/app"
	"pm/internal/mssql"
	"pm/internal/phpver"
	"pm/internal/postgres"
)

// phpExtCache remembers which SQL Server PHP extension the global PHP
// has: finding out runs `php -m`, too slow for every status poll.
var phpExtCache struct {
	sync.Mutex
	php string
	ext string
	at  time.Time
}

func cachedSQLServerExt(a *app.App) string {
	phpExtCache.Lock()
	defer phpExtCache.Unlock()
	v := a.State.Config.GlobalPHP
	if phpExtCache.php == v && time.Since(phpExtCache.at) < 20*time.Second {
		return phpExtCache.ext
	}
	phpExtCache.php, phpExtCache.ext, phpExtCache.at = v, a.SQLServerPHPExtension(), time.Now()
	return phpExtCache.ext
}

// forgetSQLServerExt drops the cache (after an extension toggle the
// next poll re-checks).
func forgetSQLServerExt() {
	phpExtCache.Lock()
	phpExtCache.at = time.Time{}
	phpExtCache.Unlock()
}

// mssqlState is getState's "mssql" entry.
func mssqlState(a *app.App) map[string]any {
	c := a.State.Config
	st := map[string]any{
		"installed":       c.MSSQL != "",
		"platform":        runtime.GOOS,
		"adminerLinked":   a.State.FindSite(adminer.SiteName) != nil,
		"adminerFiles":    adminer.Installed(a.Paths),
		"phpExtHint":      app.PHPExtensionHint,
		"containerEngine": runtime.GOOS != "windows",
	}
	running := c.MSSQL != "" && mssql.Running()
	if c.MSSQL != "" {
		st["series"] = c.MSSQL
		st["label"] = mssql.Label(c.MSSQL)
		st["port"] = mssql.Port
		st["running"] = running
		st["stopped"] = c.MSSQLStopped
		st["password"] = c.MSSQLPassword
		st["env"] = app.MSSQLEnvSnippet(mssql.Port, c.MSSQLPassword, runtime.GOOS == "windows")
		st["adminerUrl"] = adminer.URL(c.TLD, mssql.Port)
		st["phpExt"] = cachedSQLServerExt(a)
	}
	// The runtime check (OrbStack installed / running) only matters
	// when the server isn't answering — skip it on the common path.
	if !running {
		st["runtime"] = mssql.QuickStatus(a.Paths)
	}
	return st
}

func registerMSSQL(api func(path string, h func(a *app.App, r *http.Request) (any, error))) {
	// jobFor wraps an install-class background job that gets a fresh
	// App (the request's one must not outlive it).
	jobFor := func(fn func(a2 *app.App, setStatus func(string)) (any, error)) (any, error) {
		id, err := jobs.startInstall(func(setStatus func(string)) (any, error) {
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			return fn(a2, setStatus)
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"job": id}, nil
	}

	api("/api/mssql/install", func(a *app.App, r *http.Request) (any, error) {
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Installing SQL Server 2025…")
			series, err := a2.InstallMSSQL(context.Background(), setStatus)
			if err != nil {
				return nil, err
			}
			return mssql.Label(series), nil
		})
	})
	api("/api/mssql/orbstack/install", func(a *app.App, r *http.Request) (any, error) {
		if runtime.GOOS != "darwin" {
			return nil, errors.New("OrbStack is macOS-only")
		}
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Downloading OrbStack…")
			return nil, a2.InstallOrbStack(context.Background(), setStatus)
		})
	})
	api("/api/mssql/orbstack/start", func(a *app.App, r *http.Request) (any, error) {
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Starting OrbStack — finish its setup window if one opens…")
			return nil, mssql.EnsureDaemon(context.Background(), 2*time.Minute)
		})
	})
	api("/api/mssql/start", func(a *app.App, r *http.Request) (any, error) {
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Starting SQL Server (it takes a little while to accept logins)…")
			return nil, a2.StartMSSQL()
		})
	})
	api("/api/mssql/stop", func(a *app.App, r *http.Request) (any, error) {
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Stopping SQL Server…")
			return nil, a2.StopMSSQL()
		})
	})
	api("/api/mssql/db/list", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.MSSQL == "" || !mssql.Running() {
			return []mssql.DatabaseInfo{}, nil
		}
		dbs, err := mssql.UserDatabaseDetails(a.Paths)
		if err != nil {
			return nil, err
		}
		if dbs == nil {
			dbs = []mssql.DatabaseInfo{}
		}
		return dbs, nil
	})
	api("/api/mssql/db/collations", func(a *app.App, r *http.Request) (any, error) {
		if a.State.Config.MSSQL == "" {
			return nil, errors.New("SQL Server is not installed")
		}
		if !mssql.Running() {
			return nil, errors.New("SQL Server is not running — start it first")
		}
		return mssql.Collations(a.Paths)
	})
	api("/api/mssql/db/create", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name, Collation string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.CreateMSSQLDBWith(in.Name, in.Collation)
	})
	api("/api/mssql/db/drop", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return nil, a.DropMSSQLDB(in.Name)
	})
	api("/api/mssql/password", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Password string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return a.SetMSSQLPassword(in.Password)
	})
	api("/api/mssql/restore", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Path, Db string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			setStatus("Restoring the backup into SQL Server…")
			return nil, a2.RestoreMSSQL(in.Path, in.Db)
		})
	})
	api("/api/mssql/php-ext", func(a *app.App, r *http.Request) (any, error) {
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			if runtime.GOOS == "windows" {
				setStatus("Installing the SQL Server PHP driver — accept the administrator prompt for Microsoft's ODBC driver if one appears…")
			} else {
				setStatus("Building the SQL Server PHP driver (pdo_dblib, a few minutes the first time)…")
			}
			defer forgetSQLServerExt()
			return nil, a2.EnableSQLServerPHP(context.Background())
		})
	})
	// /api/adminer/open links Adminer (downloading it the first time)
	// and returns its SQL Server login URL. With {"engine": "pgsql"} it
	// is for PostgreSQL instead: that needs the global PHP's pgsql or
	// pdo_pgsql extension, checked first so the link never lands on
	// Adminer's "none of the supported PHP extensions" page.
	api("/api/adminer/open", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Engine string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Engine == "pgsql" {
			if a.State.Config.Postgres == "" {
				return nil, errors.New("PostgreSQL is not installed")
			}
			if !phpHasPgsql(a) {
				return nil, errors.New("Adminer needs PHP's pgsql or pdo_pgsql extension for PostgreSQL — enable one in PHP → Extensions")
			}
		}
		return jobFor(func(a2 *app.App, setStatus func(string)) (any, error) {
			if !adminer.Installed(a2.Paths) {
				setStatus("Downloading Adminer…")
			} else {
				setStatus("Linking Adminer…")
			}
			a2.Progress = setStatus
			forgetSQLServerExt()
			u, err := a2.EnsureAdminer(context.Background())
			if err != nil || in.Engine != "pgsql" {
				return u, err
			}
			if !postgres.Running() {
				setStatus("Starting PostgreSQL…")
				if err := a2.StartPostgres(); err != nil {
					return nil, err
				}
			}
			return adminerPgURL(a2.State.Config.TLD, postgres.Port), nil
		})
	})
}

// adminerPgURL is Adminer's PostgreSQL login, prefilled: the driver and
// server come from ?pgsql=<host:port>, the user from &username= (the
// panel appends &db=<name> to land on one database).
func adminerPgURL(tld string, port int) string {
	return "https://" + adminer.SiteName + "." + tld + "/?pgsql=" +
		url.QueryEscape("127.0.0.1:"+strconv.Itoa(port)) + "&username=" + url.QueryEscape(postgres.Superuser)
}

// phpHasPgsql reports whether the global PHP has a PostgreSQL driver
// Adminer can use.
func phpHasPgsql(a *app.App) bool {
	v := a.State.Config.GlobalPHP
	if v == "" {
		return false
	}
	exts, err := phpver.ListExtensions(a.Paths, v)
	if err != nil {
		return false
	}
	for _, e := range exts {
		if e.Enabled && (e.Name == "pgsql" || e.Name == "pdo_pgsql") {
			return true
		}
	}
	return false
}
