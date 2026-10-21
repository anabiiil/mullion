// Package mssql runs Microsoft SQL Server (Developer edition) for local
// development.
//
// macOS has no native SQL Server, so there it is Microsoft's official
// Linux container (mcr.microsoft.com/mssql/server) run under OrbStack —
// published on 127.0.0.1 only, its data in a named Docker volume.
// Windows gets a real, native named instance (MULLION) installed with
// Microsoft's own setup, started and stopped through its service.
//
// Both look the same from the outside: an `sa` login with a generated
// password on 127.0.0.1:<Port>.
package mssql

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"pm/internal/download"
	"pm/internal/pmdir"
)

const (
	// Series is the SQL Server release Mullion installs: SQL Server 2025
	// (17.x), generally available since November 2025.
	Series = "2025"
	// DefaultPort is SQL Server's standard port.
	DefaultPort = 1433
	// Login is the administrator login every install has.
	Login = "sa"

	// ContainerName / VolumeName are the macOS container and its data
	// volume. A named volume, not a bind mount: SQL Server needs a Linux
	// filesystem for /var/opt/mssql (bind mounts from macOS are known to
	// break it — O_DIRECT and file locking).
	ContainerName = "mullion-mssql"
	VolumeName    = "mullion-mssql"
	// Image is Microsoft's SQL Server 2025 Linux image. Source:
	// https://learn.microsoft.com/sql/linux/quickstart-install-connect-docker
	// (`docker pull mcr.microsoft.com/mssql/server:2025-latest`).
	Image = "mcr.microsoft.com/mssql/server:2025-latest"
	// ContainerEdition is the MSSQL_PID for the free, full-featured
	// developer edition. SQL Server 2025 split "Developer" into
	// StandardDeveloper and EnterpriseDeveloper; Enterprise Developer
	// is the one with every feature. Source:
	// https://learn.microsoft.com/sql/linux/sql-server-linux-configure-environment-variables
	ContainerEdition = "EnterpriseDeveloper"

	// InstanceName is the Windows named instance, and ServiceName its
	// Windows service ("MSSQL$<instance>").
	InstanceName = "MULLION"
	ServiceName  = `MSSQL$` + InstanceName
)

// RuntimeStatus is what the panel and doctor need to know about what
// SQL Server runs on.
type RuntimeStatus struct {
	// Kind is "container" (macOS: OrbStack) or "native" (Windows).
	Kind string `json:"kind"`
	// OrbStack: OrbStack.app is installed (macOS).
	OrbStack bool `json:"orbstack"`
	// Docker: a docker CLI was found (macOS).
	Docker bool `json:"docker"`
	// Daemon: the docker daemon answers (macOS).
	Daemon bool `json:"daemon"`
	// Container: Mullion's SQL Server container exists (macOS; only
	// checked when the daemon answers).
	Container bool `json:"container"`
	// Service: the MSSQL$MULLION Windows service exists (Windows).
	Service bool `json:"service"`
}

// ErrNoDocker means there is no docker CLI: OrbStack isn't installed.
var ErrNoDocker = errors.New("SQL Server on macOS runs in a container and needs OrbStack — install it first (Databases → SQL Server → Install OrbStack, or `mullion mssql orbstack install`)")

// downloadFile fetches a URL to a file (a seam for tests).
var downloadFile = download.ToFile

// Port is the port Mullion's server listens on: DefaultPort unless the
// user moved it (config's MSSQLPort, applied by app.ApplyPortConfig).
var Port = DefaultPort

// Password is the sa password (config's MSSQLPassword, set by app.New).
var Password string

// Label is the human name of an installed release, e.g. "SQL Server 2025".
func Label(series string) string {
	if series == "" {
		series = Series
	}
	return "SQL Server " + series
}

// Host is the address clients use: "127.0.0.1,<port>" is SQL Server's
// own server-name syntax (sqlcmd, SSMS, ODBC).
func Host() string { return fmt.Sprintf("127.0.0.1,%d", Port) }

// Running reports whether something accepts connections on the SQL
// Server port. Cheap enough for the panel's status poll.
func Running() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// backupStageDir is where the server writes BACKUP files (and reads
// RESTORE files) on the host side before they are copied to their final
// place: on Windows the SQL Server service account is granted write
// access to it at install time.
func backupStageDir(paths pmdir.Paths) string {
	return filepath.Join(paths.MssqlDir(), "backup")
}

// ── passwords ─────────────────────────────────────────────────────

// GeneratePassword makes a random sa password that always satisfies SQL
// Server's default policy (8+ characters from at least three of:
// upper, lower, digits, symbols — see
// https://learn.microsoft.com/sql/relational-databases/security/password-policy).
// Symbols are limited to ones that need no quoting in a shell, a URL,
// a .env file or a Windows command line.
func GeneratePassword() (string, error) {
	const (
		upper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower   = "abcdefghijkmnopqrstuvwxyz"
		digits  = "23456789"
		symbols = "_-.@%"
	)
	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}
	all := upper + lower + digits + symbols
	out := make([]byte, 0, 20)
	// One of each class first, then shuffle in the rest.
	for _, set := range []string{upper, lower, digits, symbols} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < 20 {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	// A leading symbol reads oddly in some tools; start with a letter.
	for i, c := range out {
		if unicode.IsLetter(rune(c)) {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return string(out), nil
}

// ValidPassword checks a password against SQL Server's default policy,
// so a bad one is refused here with a clear message instead of failing
// deep inside setup (where the container simply stops).
func ValidPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 128 {
		return fmt.Errorf("the sa password must be 8-128 characters long")
	}
	if strings.ContainsAny(pw, "'\"`\r\n") {
		return fmt.Errorf("the sa password cannot contain quotes or line breaks")
	}
	var up, low, dig, sym int
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			up = 1
		case unicode.IsLower(r):
			low = 1
		case unicode.IsDigit(r):
			dig = 1
		default:
			sym = 1
		}
	}
	if up+low+dig+sym < 3 {
		return fmt.Errorf("the sa password needs characters from three of: uppercase, lowercase, digits, symbols")
	}
	return nil
}

// ── T-SQL helpers ─────────────────────────────────────────────────

var dbNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-$@#]{0,127}$`)

// systemDatabases can never be created, dropped or restored over.
var systemDatabases = map[string]bool{"master": true, "model": true, "msdb": true, "tempdb": true}

// ValidDatabaseName accepts the plain identifiers projects use — no
// spaces, brackets or quotes — so names are also safe as file names.
func ValidDatabaseName(name string) error {
	if !dbNameRe.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letters, digits, _ - $ @ #; must start with a letter or _)", name)
	}
	if systemDatabases[strings.ToLower(name)] {
		return fmt.Errorf("%s is a SQL Server system database", name)
	}
	return nil
}

// quoteIdent brackets an identifier: [name], with ] doubled.
func quoteIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// quoteString is a Unicode T-SQL string literal: N'text', ' doubled.
func quoteString(s string) string {
	return "N'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// listDatabasesSQL lists user databases (database_id > 4 skips master,
// tempdb, model and msdb).
const listDatabasesSQL = "SET NOCOUNT ON; SELECT name FROM sys.databases WHERE database_id > 4 ORDER BY name;"

func createDatabaseSQL(name string) string {
	return "CREATE DATABASE " + quoteIdent(name) + ";"
}

// dropDatabaseSQL kicks other sessions out first: DROP fails while
// anything (a Laravel queue worker, Adminer) is connected.
func dropDatabaseSQL(name string) string {
	return fmt.Sprintf("IF DB_ID(%s) IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END;",
		quoteString(name), quoteIdent(name), quoteIdent(name))
}

func setPasswordSQL(pw string) string {
	return "ALTER LOGIN [sa] WITH PASSWORD = " + quoteString(pw) + ";"
}

// backupSQL writes a full, self-contained backup of one database.
// COPY_ONLY leaves the database's own backup chain alone; INIT/FORMAT
// overwrite a stale file of the same name instead of appending.
func backupSQL(db, file string) string {
	return fmt.Sprintf("BACKUP DATABASE %s TO DISK = %s WITH COPY_ONLY, INIT, FORMAT;", quoteIdent(db), quoteString(file))
}

// parseRows splits tool output (one row per line, columns separated by
// tabs) into trimmed rows, skipping empty lines.
func parseRows(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}
		rows = append(rows, cols)
	}
	return rows
}

// firstColumn is parseRows' first column of every row.
func firstColumn(out string) []string {
	var out2 []string
	for _, r := range parseRows(out) {
		out2 = append(out2, r[0])
	}
	return out2
}

// backupFileName is the file a database is backed up to (and, read
// back by RestoreBackup, the database it restores into).
func backupFileName(db string) string { return db + ".bak" }

// DatabaseFromBackupFile is the database a backup file restores into:
// its name without .bak ("shop.bak" -> "shop").
func DatabaseFromBackupFile(file string) string {
	base := filepath.Base(file)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// backupFile is one row of RESTORE FILELISTONLY: a logical file inside
// the backup and its type (D data, L log, S filestream, F full-text).
type backupFile struct {
	Logical string
	Type    string
}

// parseFileList reads RESTORE FILELISTONLY's output (tab-separated:
// LogicalName, PhysicalName, Type, ...).
func parseFileList(out string) ([]backupFile, error) {
	var files []backupFile
	for _, r := range parseRows(out) {
		if len(r) < 3 {
			continue
		}
		files = append(files, backupFile{Logical: r[0], Type: strings.ToUpper(r[2])})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("the backup lists no database files — is it a SQL Server .bak?")
	}
	return files, nil
}

// restoreSQL restores a backup into db, moving every file into the
// server's default data/log folders — a .bak made on another machine
// (or the other OS: C:\ paths vs /var/opt/mssql) would otherwise try to
// recreate its original paths. sep is the server's path separator.
func restoreSQL(db, file string, files []backupFile, dataDir, logDir, sep string) string {
	dataDir = strings.TrimRight(dataDir, `\/`) + sep
	logDir = strings.TrimRight(logDir, `\/`) + sep
	var moves []string
	dataN := 0
	for _, f := range files {
		var target string
		switch f.Type {
		case "L":
			target = logDir + db + "_" + sanitizeFile(f.Logical) + ".ldf"
		default:
			ext := ".ndf"
			if f.Type == "D" && dataN == 0 {
				ext = ".mdf"
			}
			if f.Type == "D" {
				dataN++
			}
			target = dataDir + db + "_" + sanitizeFile(f.Logical) + ext
		}
		moves = append(moves, "MOVE "+quoteString(f.Logical)+" TO "+quoteString(target))
	}
	return fmt.Sprintf("IF DB_ID(%s) IS NOT NULL ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
		"RESTORE DATABASE %s FROM DISK = %s WITH REPLACE, RECOVERY, %s; "+
		"ALTER DATABASE %s SET MULTI_USER;",
		quoteString(db), quoteIdent(db), quoteIdent(db), quoteString(file), strings.Join(moves, ", "), quoteIdent(db))
}

var unsafeFileRe = regexp.MustCompile(`[^A-Za-z0-9_\-]+`)

func sanitizeFile(s string) string {
	s = unsafeFileRe.ReplaceAllString(s, "_")
	if s == "" {
		return "file"
	}
	return s
}

// ── database operations (platform backends do the transport) ─────

// UserDatabases lists the databases on the running server, sorted.
func UserDatabases(paths pmdir.Paths) ([]string, error) {
	out, err := query(paths, listDatabasesSQL)
	if err != nil {
		return nil, err
	}
	dbs := firstColumn(out)
	sort.Strings(dbs)
	return dbs, nil
}

// CreateDatabase creates an empty database (the server's collation).
func CreateDatabase(paths pmdir.Paths, name string) error {
	return CreateDatabaseWith(paths, name, "")
}

// CreateDatabaseWith creates an empty database with the given collation
// ("" = the server's), checked against sys.fn_helpcollations() first.
func CreateDatabaseWith(paths pmdir.Paths, name, collation string) error {
	if err := ValidDatabaseName(name); err != nil {
		return err
	}
	if collation != "" {
		info, err := Collations(paths)
		if err != nil {
			return err
		}
		if collation, err = info.Resolve(collation); err != nil {
			return err
		}
	}
	stmt, err := createDatabaseWithSQL(name, collation)
	if err != nil {
		return err
	}
	_, err = query(paths, stmt)
	return err
}

// DropDatabase deletes a database and all its data.
func DropDatabase(paths pmdir.Paths, name string) error {
	if systemDatabases[strings.ToLower(name)] {
		return fmt.Errorf("%s is a SQL Server system database", name)
	}
	_, err := query(paths, dropDatabaseSQL(name))
	return err
}

// SetPassword changes the sa password on the running server (connecting
// with the current one, Password) and updates Password on success.
func SetPassword(paths pmdir.Paths, newPassword string) error {
	if err := ValidPassword(newPassword); err != nil {
		return err
	}
	if _, err := query(paths, setPasswordSQL(newPassword)); err != nil {
		return err
	}
	Password = newPassword
	return nil
}

// BackupTo writes one <db>.bak per user database into dir and returns
// the file names. The server writes each backup into its staging folder
// first (it cannot write to an arbitrary user folder), then it is moved
// out.
func BackupTo(paths pmdir.Paths, dir string) ([]string, error) {
	dbs, err := UserDatabases(paths)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var files []string
	for _, db := range dbs {
		name := backupFileName(sanitizeFile(db))
		if sanitizeFile(db) != db {
			fmt.Printf("note: %q is saved as %s (its name is not file-safe; restore it with that database name)\n", db, name)
		}
		fmt.Println("Backing up", db, "...")
		serverPath, err := stagePath(paths, name)
		if err != nil {
			return files, err
		}
		if _, err := query(paths, backupSQL(db, serverPath)); err != nil {
			return files, fmt.Errorf("backing up %s: %w", db, err)
		}
		if err := copyOut(paths, serverPath, filepath.Join(dir, name)); err != nil {
			return files, fmt.Errorf("copying the backup of %s: %w", db, err)
		}
		files = append(files, name)
	}
	return files, nil
}

// RestoreFile restores a .bak into database db ("" = the file's name
// without .bak), replacing it if it exists.
func RestoreFile(paths pmdir.Paths, file, db string) error {
	if db == "" {
		db = DatabaseFromBackupFile(file)
	}
	if err := ValidDatabaseName(db); err != nil {
		return err
	}
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("%s does not exist", file)
	}
	serverPath, err := copyIn(paths, file)
	if err != nil {
		return fmt.Errorf("handing %s to SQL Server: %w", filepath.Base(file), err)
	}
	defer removeStaged(paths, serverPath)

	out, err := query(paths, "RESTORE FILELISTONLY FROM DISK = "+quoteString(serverPath)+";")
	if err != nil {
		return err
	}
	files, err := parseFileList(out)
	if err != nil {
		return err
	}
	dirs, err := query(paths, "SET NOCOUNT ON; SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(4000)), CAST(SERVERPROPERTY('InstanceDefaultLogPath') AS nvarchar(4000));")
	if err != nil {
		return err
	}
	rows := parseRows(dirs)
	if len(rows) == 0 || len(rows[0]) < 2 {
		return fmt.Errorf("could not read the server's data folders")
	}
	fmt.Printf("Restoring %s into database %s ...\n", filepath.Base(file), db)
	_, err = query(paths, restoreSQL(db, serverPath, files, rows[0][0], rows[0][1], serverSep()))
	return err
}

// ServerVersion asks the running server for its full version, e.g.
// "17.0.1000.7".
func ServerVersion(paths pmdir.Paths) (string, error) {
	out, err := query(paths, "SET NOCOUNT ON; SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(64));")
	if err != nil {
		return "", err
	}
	v := firstColumn(out)
	if len(v) == 0 {
		return "", fmt.Errorf("the server did not report its version")
	}
	return v[0], nil
}

// Query runs T-SQL as sa on the running server and returns its rows,
// one per line with tab-separated columns.
func Query(paths pmdir.Paths, sql string) (string, error) { return query(paths, sql) }

// waitReady polls until the server answers a query, up to timeout.
func waitReady(paths pmdir.Paths, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if _, err := queryQuiet(paths, "SELECT 1;"); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(2 * time.Second)
	}
	if last == nil {
		last = fmt.Errorf("no answer")
	}
	return fmt.Errorf("SQL Server did not become ready within %s: %w", timeout, last)
}
