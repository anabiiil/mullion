package explain

// Which database a query ran on, from the project's .env — the same
// DB_* variables Laravel's config/database.php reads for its stock
// connections (mysql, mariadb, pgsql, sqlite, sqlsrv).

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Conn is a database to explain against. Password is never printed.
type Conn struct {
	Name     string `json:"name"`   // Laravel connection name
	Driver   string `json:"driver"` // mysql, mariadb, pgsql, sqlite, sqlsrv
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Socket   string `json:"socket,omitempty"`
	Database string `json:"database"`
	Username string `json:"username,omitempty"`
	Password string `json:"-"`
}

// Label describes the connection for the panel (never the password).
func (c Conn) Label() string {
	if c.Driver == "sqlite" {
		return "sqlite " + c.Database
	}
	where := c.Host
	if c.Socket != "" {
		where = c.Socket
	} else if c.Port > 0 {
		where += ":" + strconv.Itoa(c.Port)
	}
	return c.Driver + " " + where + "/" + c.Database
}

// Drivers are the drivers Mullion explains.
var Drivers = map[string]bool{"mysql": true, "mariadb": true, "pgsql": true, "sqlite": true, "sqlsrv": true}

var defaultPorts = map[string]int{"mysql": 3306, "mariadb": 3306, "pgsql": 5432, "sqlsrv": 1433}

// urlSchemes maps DB_URL schemes to drivers (Laravel's
// ConfigurationUrlParser).
var urlSchemes = map[string]string{
	"mysql": "mysql", "mysql2": "mysql", "mariadb": "mariadb", "pgsql": "pgsql", "postgres": "pgsql",
	"postgresql": "pgsql", "sqlite": "sqlite", "sqlite3": "sqlite", "sqlsrv": "sqlsrv", "mssql": "sqlsrv",
}

var connDriverRe = regexp.MustCompile(`(?s)['"]([A-Za-z0-9_\-]+)['"]\s*=>\s*\[\s*(?:[^\[\]]*?)['"]driver['"]\s*=>\s*['"]([a-z]+)['"]`)

// configDrivers reads connection name → driver from the project's
// config/database.php (a plain regex over the array — enough for the
// usual layout).
func configDrivers(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, "config", "database.php"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, m := range connDriverRe.FindAllStringSubmatch(string(data), -1) {
		if _, ok := out[m[1]]; !ok {
			out[m[1]] = m[2]
		}
	}
	return out
}

// ConnFromEnv resolves the connection a query ran on (queryConn, the
// connection name the watcher recorded; "" = the default) from the
// project's .env values. It returns notes for the panel when it had to
// guess.
func ConnFromEnv(env map[string]string, dir, queryConn string) (Conn, []string, error) {
	var notes []string
	def := strings.TrimSpace(env["DB_CONNECTION"])
	if def == "" {
		// Laravel 11+ defaults to sqlite; older apps to mysql.
		if _, err := os.Stat(filepath.Join(dir, "database", "database.sqlite")); err == nil {
			def = "sqlite"
		} else {
			def = "mysql"
		}
	}
	name := queryConn
	if name == "" {
		name = def
	}
	c := Conn{Name: name}
	cfg := configDrivers(dir)
	switch {
	case Drivers[name]:
		c.Driver = name
		if cfg[name] != "" && Drivers[cfg[name]] {
			c.Driver = cfg[name]
		}
	case cfg[name] != "" && Drivers[cfg[name]]:
		c.Driver = cfg[name]
	default:
		return c, nil, errUnknownConn(name)
	}
	if name != def {
		notes = append(notes, "This query ran on the \""+name+"\" connection; Mullion only reads the .env's DB_* settings (those of DB_CONNECTION="+def+"), so it explained the query with them — if that connection points elsewhere, the plan may not match.")
	} else if !Drivers[name] {
		notes = append(notes, "\""+name+"\" is a "+c.Driver+" connection in config/database.php; Mullion used the .env's DB_* settings for it.")
	}

	c.Host = firstNonEmpty(env["DB_HOST"], "127.0.0.1")
	if c.Driver == "sqlsrv" && env["DB_HOST"] == "" {
		c.Host = "localhost"
	}
	c.Port = defaultPorts[c.Driver]
	if p, err := strconv.Atoi(strings.TrimSpace(env["DB_PORT"])); err == nil && p > 0 {
		c.Port = p
	}
	c.Socket = env["DB_SOCKET"]
	c.Database = env["DB_DATABASE"]
	c.Username = env["DB_USERNAME"]
	c.Password = env["DB_PASSWORD"]
	if c.Driver != "sqlite" {
		if c.Database == "" {
			c.Database = "laravel"
		}
		if c.Username == "" {
			c.Username = "root"
		}
		if c.Driver != "mysql" && c.Driver != "mariadb" {
			c.Socket = ""
		}
	}
	if u := strings.TrimSpace(env["DB_URL"]); u != "" {
		applyURL(&c, u)
	}
	if c.Driver == "sqlite" {
		if c.Database == "" || c.Database == "laravel" {
			c.Database = filepath.Join("database", "database.sqlite")
		}
		if c.Database == ":memory:" {
			return c, notes, errMemorySQLite
		}
		if !filepath.IsAbs(c.Database) {
			c.Database = filepath.Join(dir, c.Database)
		}
		if _, err := os.Stat(c.Database); err != nil {
			return c, notes, errNoSQLiteFile(c.Database)
		}
	}
	return c, notes, nil
}

// applyURL lets DB_URL override the parts it names.
func applyURL(c *Conn, raw string) {
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if d, ok := urlSchemes[strings.ToLower(u.Scheme)]; ok {
		c.Driver = d
	}
	if d := c.Driver; d == "sqlite" {
		p := u.Path
		if u.Host != "" && u.Host != "localhost" {
			p = u.Host + p
		}
		if p != "" {
			c.Database = p
		}
		return
	}
	if h := u.Hostname(); h != "" {
		c.Host = h
	}
	if p, err := strconv.Atoi(u.Port()); err == nil {
		c.Port = p
	}
	if u.User != nil {
		c.Username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			c.Password = pw
		}
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		c.Database = db
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
