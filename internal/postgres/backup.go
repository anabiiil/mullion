package postgres

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

// globalsFileName holds pg_dumpall --globals-only: roles and
// tablespaces, which live outside any one database and so aren't in
// any of the per-database dumps.
const globalsFileName = "globals.sql"

// pgClientEnv is the environment for pg_dump/pg_restore/pg_dumpall:
// cleanEnv (see postgres.go) plus a connect timeout and, when a
// password is set, PGPASSWORD — the same way psql() authenticates.
func pgClientEnv(password string) []string {
	env := append(cleanEnv(), "PGCONNECT_TIMEOUT=10")
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	return env
}

// BackupTo dumps every user database on the running server into dir:
// one <name>.dump per database, in pg_dump's custom format (-Fc — the
// only format pg_restore can selectively restore from and that
// survives cross-version restores cleanly), plus globals.sql (roles,
// tablespaces) from pg_dumpall --globals-only. Returns the files
// written, in the order they were created.
func BackupTo(paths pmdir.Paths, version, dir string) ([]string, error) {
	dbs, err := UserDatabases(paths, version)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var files []string
	for i, db := range dbs {
		fmt.Printf("database %d of %d: %s\n", i+1, len(dbs), db)
		out := filepath.Join(dir, db+".dump")
		if err := pgDump(paths, version, db, out); err != nil {
			return files, err
		}
		files = append(files, out)
	}
	fmt.Println("globals (roles, tablespaces):")
	globals := filepath.Join(dir, globalsFileName)
	if err := pgDumpAllGlobals(paths, version, globals); err != nil {
		return files, err
	}
	files = append(files, globals)
	return files, nil
}

// pgDump writes one database to outFile in pg_dump's custom format.
func pgDump(paths pmdir.Paths, version, db, outFile string) error {
	cmd := proc.Quiet(exe(paths, version, "pg_dump"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(Port), "-U", Superuser,
		"-Fc", "-f", outFile, db)
	cmd.Env = pgClientEnv(Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outFile)
		return fmt.Errorf("pg_dump %s: %v: %s", db, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pgDumpAllGlobals writes the cluster-wide objects (roles, tablespaces)
// no per-database dump carries.
func pgDumpAllGlobals(paths pmdir.Paths, version, outFile string) error {
	cmd := proc.Quiet(exe(paths, version, "pg_dumpall"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(Port), "-U", Superuser,
		"--globals-only", "-f", outFile)
	cmd.Env = pgClientEnv(Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(outFile)
		return fmt.Errorf("pg_dumpall --globals-only: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RestoreFile replays a pg_dump custom-format file into db, creating
// the database first if it doesn't exist yet. --clean --if-exists
// drops the dump's own objects before recreating them (so restoring
// twice, or into a database that already has other objects, doesn't
// fail on "already exists"); --no-owner skips ownership statements,
// which would otherwise fail unless the dump's original role exists on
// this server.
func RestoreFile(paths pmdir.Paths, version, file, db string) error {
	if err := checkName(db); err != nil {
		return err
	}
	if systemDBs[strings.ToLower(db)] {
		return fmt.Errorf("%s is a system database", db)
	}
	if err := CreateDatabase(paths, version, db); err != nil {
		return err
	}
	cmd := proc.Quiet(exe(paths, version, "pg_restore"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(Port), "-U", Superuser,
		"-d", db, "--clean", "--if-exists", "--no-owner", file)
	cmd.Env = pgClientEnv(Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_restore: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RestoreGlobals replays a globals.sql (pg_dumpall --globals-only)
// through psql. Best-effort by design: a role that already exists on
// this server makes a CREATE ROLE statement fail, so a caller restoring
// a whole backup should treat this as advisory, not fatal.
func RestoreGlobals(paths pmdir.Paths, version, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	_, err = psql(paths, version, Password, string(data))
	return err
}
