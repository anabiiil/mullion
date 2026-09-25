package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/mongodb"
	"pm/internal/pmdir"
	"pm/internal/postgres"
)

// hostsSnapshot fingerprints the REAL /etc/hosts (mtime + content hash):
// this test's App uses a sandbox Paths.Home, but Apply() — which
// BackupEngine/RestoreBackup/UninstallEngine/UninstallAdminTool would
// call if App.skipApply weren't set — reconciles the actual system
// hosts file regardless of Paths.Home. Any drift here means a real
// Apply() slipped through and this test could have wiped or rewritten
// the developer's actual Mullion hosts entries.
func hostsSnapshot(t *testing.T) (time.Time, string) {
	t.Helper()
	info, err := os.Stat("/etc/hosts")
	if err != nil {
		t.Fatalf("stat /etc/hosts: %v", err)
	}
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		t.Fatalf("reading /etc/hosts: %v", err)
	}
	sum := sha256.Sum256(data)
	return info.ModTime(), hex.EncodeToString(sum[:])
}

// TestBackupRestoreUninstallEndToEnd drives the whole backup/restore/
// uninstall lifecycle against real PostgreSQL and MongoDB servers: it
// installs both (downloading them), creates one row/document, backs the
// engine up, drops the database, restores from the backup, checks the
// data came back, then uninstalls the engine (backing it up again and
// deleting its data) and checks the binaries and data are gone and the
// port is free.
//
// Skipped unless MULLION_BACKUP_E2E=1. MULLION_BACKUP_E2E_HOME picks the
// Mullion home to use (default: a temp dir; set it to reuse downloads
// between runs). Ports 5432 and 27017 must be free.
func TestBackupRestoreUninstallEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_BACKUP_E2E") != "1" {
		t.Skip("set MULLION_BACKUP_E2E=1 to run (downloads PostgreSQL and MongoDB)")
	}
	if postgres.Running() {
		t.Fatalf("port %d is already in use — refusing to test against someone else's server", postgres.Port)
	}
	if mongodb.Running() {
		t.Fatalf("port %d is already in use — refusing to test against someone else's server", mongodb.Port)
	}

	// SAFETY: this must never touch the real /etc/hosts (see
	// hostsSnapshot's doc comment and App.skipApply). Checked in
	// t.Cleanup so it still runs on a failure or panic partway through.
	hostsMTimeBefore, hostsHashBefore := hostsSnapshot(t)
	t.Cleanup(func() {
		hostsMTimeAfter, hostsHashAfter := hostsSnapshot(t)
		if !hostsMTimeAfter.Equal(hostsMTimeBefore) || hostsHashAfter != hostsHashBefore {
			t.Errorf("SAFETY VIOLATION: /etc/hosts changed during this test (mtime %v -> %v) — a real Apply() call reached the system hosts file", hostsMTimeBefore, hostsMTimeAfter)
		} else {
			t.Log("/etc/hosts unchanged (mtime and content hash both match)")
		}
	})

	home := os.Getenv("MULLION_BACKUP_E2E_HOME")
	if home == "" {
		home = t.TempDir()
	}
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	postgres.Password = ""
	t.Cleanup(func() { postgres.Password = "" })

	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Paths: paths, State: state, skipApply: true}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// ---- install ----
	pgVersion, err := postgres.ResolveVersion(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PostgreSQL version: %s", pgVersion)
	if err := postgres.Install(ctx, paths, pgVersion); err != nil {
		t.Fatalf("postgres.Install: %v", err)
	}
	// The EDB zip filter (internal/postgres/fetch.go wantEntry) is meant
	// to keep pg_dump/pg_restore/pg_dumpall alongside the server — verify
	// that's actually true of what got installed.
	for _, tool := range []string{"pg_dump", "pg_restore", "pg_dumpall"} {
		if _, err := os.Stat(filepath.Join(postgres.BinDir(paths, pgVersion), pmdir.ExeName(tool))); err != nil {
			t.Fatalf("%s missing from the installed PostgreSQL bin dir: %v", tool, err)
		}
	}
	if err := postgres.EnsureInitialized(paths, pgVersion); err != nil {
		t.Fatal(err)
	}
	a.State.Config.Postgres = pgVersion
	if err := postgres.Start(paths, pgVersion); err != nil {
		t.Fatal(err)
	}

	mongoVersion, err := mongodb.ResolveVersion(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MongoDB version: %s", mongoVersion)
	if err := mongodb.Install(ctx, paths, mongoVersion); err != nil {
		t.Fatalf("mongodb.Install: %v", err)
	}
	if err := mongodb.EnsureInitialized(paths, mongoVersion); err != nil {
		t.Fatal(err)
	}
	a.State.Config.Mongo = mongoVersion
	if err := mongodb.Start(paths, mongoVersion); err != nil {
		t.Fatal(err)
	}
	if err := a.State.Save(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = postgres.Stop(paths, pgVersion)
		_ = mongodb.Stop(paths, mongoVersion)
	})

	// ---- seed one row / one document ----
	const db = "mullion_backup_e2e"
	if err := postgres.CreateDatabase(paths, pgVersion, db); err != nil {
		t.Fatal(err)
	}
	if out, err := psqlRun(paths, pgVersion, db, "CREATE TABLE t (id int); INSERT INTO t VALUES (42);"); err != nil {
		t.Fatalf("seeding postgres: %v: %s", err, out)
	}
	if err := mongodb.CreateDatabase(paths, db); err != nil {
		t.Fatal(err)
	}
	if out, err := mongoshRun(paths, fmt.Sprintf(`db.getSiblingDB(%s).things.insertOne({n: 42})`, jsString(db))); err != nil {
		t.Fatalf("seeding mongo: %v: %s", err, out)
	}

	// ---- backup ----
	pgBackupDir, err := a.BackupEngine(ctx, "postgres")
	if err != nil {
		t.Fatalf("BackupEngine(postgres): %v", err)
	}
	t.Logf("postgres backup: %s", pgBackupDir)
	if !strings.HasSuffix(pgBackupDir, "-postgres") {
		t.Errorf("postgres backup dir %q does not end in -postgres", pgBackupDir)
	}
	if _, err := os.Stat(filepath.Join(pgBackupDir, db+".dump")); err != nil {
		t.Errorf("no %s.dump in %s", db, pgBackupDir)
	}
	if _, err := os.Stat(filepath.Join(pgBackupDir, "globals.sql")); err != nil {
		t.Errorf("no globals.sql in %s", pgBackupDir)
	}

	mongoBackupDir, err := a.BackupEngine(ctx, "mongo")
	if err != nil {
		t.Fatalf("BackupEngine(mongo): %v", err)
	}
	t.Logf("mongo backup: %s", mongoBackupDir)
	if _, err := os.Stat(filepath.Join(mongoBackupDir, db+".archive.gz")); err != nil {
		t.Errorf("no %s.archive.gz in %s", db, mongoBackupDir)
	}

	// ---- drop, restore, verify ----
	if err := postgres.DropDatabase(paths, pgVersion, db); err != nil {
		t.Fatal(err)
	}
	if err := mongodb.DropDatabase(paths, db); err != nil {
		t.Fatal(err)
	}

	if err := a.RestoreBackup(ctx, pgBackupDir, ""); err != nil {
		t.Fatalf("RestoreBackup(postgres): %v", err)
	}
	if out, err := psqlRun(paths, pgVersion, db, "SELECT id FROM t;"); err != nil || strings.TrimSpace(out) != "42" {
		t.Fatalf("after postgres restore: SELECT id FROM t = %q, %v", out, err)
	}

	if err := a.RestoreBackup(ctx, mongoBackupDir, ""); err != nil {
		t.Fatalf("RestoreBackup(mongo): %v", err)
	}
	if out, err := mongoshRun(paths, fmt.Sprintf(`print(db.getSiblingDB(%s).things.findOne().n)`, jsString(db))); err != nil || !strings.Contains(out, "42") {
		t.Fatalf("after mongo restore: things.findOne().n -> %q, %v", out, err)
	}

	// ---- uninstall (backup first, delete data) ----
	pgUninstallBackup, err := a.UninstallEngine(ctx, "postgres", true, true)
	if err != nil {
		t.Fatalf("UninstallEngine(postgres): %v", err)
	}
	t.Logf("postgres uninstall backup: %s", pgUninstallBackup)
	if _, err := os.Stat(postgres.BaseDir(paths)); !os.IsNotExist(err) {
		t.Errorf("postgres base dir still exists after uninstall with deleteData=true: %v", err)
	}
	if postgres.Running() {
		t.Error("postgres still listening after UninstallEngine")
	}
	if a.State.Config.Postgres != "" {
		t.Error("config.Postgres not cleared after UninstallEngine")
	}

	mongoUninstallBackup, err := a.UninstallEngine(ctx, "mongo", true, true)
	if err != nil {
		t.Fatalf("UninstallEngine(mongo): %v", err)
	}
	t.Logf("mongo uninstall backup: %s", mongoUninstallBackup)
	if _, err := os.Stat(mongodb.BaseDir(paths)); !os.IsNotExist(err) {
		t.Errorf("mongo base dir still exists after uninstall with deleteData=true: %v", err)
	}
	if mongodb.Running() {
		t.Error("mongod still listening after UninstallEngine")
	}
	if a.State.Config.Mongo != "" {
		t.Error("config.Mongo not cleared after UninstallEngine")
	}

	backups, err := a.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("final backups: %d", len(backups))
}

// psqlRun runs one statement/query against db as the superuser,
// returning unaligned tuples-only output for queries.
func psqlRun(paths pmdir.Paths, version, db, sql string) (string, error) {
	exe := filepath.Join(postgres.BinDir(paths, version), pmdir.ExeName("psql"))
	cmd := exec.Command(exe, "-h", "127.0.0.1", "-p", strconv.Itoa(postgres.Port), "-U", postgres.Superuser,
		"-d", db, "-X", "-w", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("psql: %w", err)
	}
	return string(out), nil
}

// mongoshRun evaluates one script with the installed mongosh.
func mongoshRun(paths pmdir.Paths, js string) (string, error) {
	sh := mongodb.MongoshPath(paths)
	if sh == "" {
		return "", fmt.Errorf("mongosh is not installed")
	}
	cmd := exec.Command(sh, "--quiet", "--norc", "--host", "127.0.0.1", "--port", strconv.Itoa(mongodb.Port), "--eval", js)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("mongosh: %w", err)
	}
	return string(out), nil
}

// jsString renders a Go string as a JavaScript string literal.
func jsString(s string) string { return strconv.Quote(s) }
