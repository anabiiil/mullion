package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pm/internal/adminer"
	"pm/internal/config"
	"pm/internal/mssql"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

func TestMSSQLBackupNaming(t *testing.T) {
	paths := pmdir.Paths{Home: "/x/mullion"}
	dir := newBackupDir(paths, "mssql")
	if !strings.HasSuffix(dir, "-mssql") {
		t.Errorf("backup dir %q", dir)
	}
	if e, ok := backupEngineFromName(filepath.Base(dir)); !ok || e != "mssql" {
		t.Errorf("engine from %q = %q %v", filepath.Base(dir), e, ok)
	}
	// "-mssql" and "-mysql" must never be confused.
	if e, _ := backupEngineFromName("2026-01-01_000000-mysql"); e != "mysql" {
		t.Errorf("mysql backup read as %q", e)
	}
}

func TestMSSQLBackupFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"shop.bak", "blog.BAK", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	all, err := mssqlBackupFiles(dir, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %v, %v", all, err)
	}
	for _, db := range []string{"shop", "shop.bak"} {
		one, err := mssqlBackupFiles(dir, db)
		if err != nil || len(one) != 1 || filepath.Base(one[0]) != "shop.bak" {
			t.Errorf("%q -> %v, %v", db, one, err)
		}
	}
	if _, err := mssqlBackupFiles(dir, "missing"); err == nil {
		t.Error("missing database must fail")
	}
	if _, err := mssqlBackupFiles(t.TempDir(), ""); err == nil {
		t.Error("empty folder must fail")
	}
}

func TestDeleteMSSQLBackup(t *testing.T) {
	a := sandboxApp(t)
	dir := newBackupDir(a.Paths, "mssql")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "shop.bak"), []byte("x"), 0o644)
	list, err := a.ListBackups()
	if err != nil || len(list) != 1 || list[0].Engine != "mssql" || list[0].Files[0] != "shop.bak" {
		t.Fatalf("ListBackups = %+v, %v", list, err)
	}
	if err := a.DeleteBackup(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("backup still there")
	}
}

func TestUninstallMSSQLEngine(t *testing.T) {
	for _, deleteData := range []bool{false, true} {
		a := sandboxApp(t)
		var gotDelete *bool
		old := uninstallMSSQL
		uninstallMSSQL = func(ctx context.Context, paths pmdir.Paths, del bool) error { gotDelete = &del; return nil }
		t.Cleanup(func() { uninstallMSSQL = old })

		a.State.Config.MSSQL = mssql.Series
		a.State.Config.MSSQLPassword = "Secret_123x"
		a.State.Config.MSSQLStopped = true
		os.MkdirAll(adminer.Dir(a.Paths), 0o755)
		os.WriteFile(filepath.Join(adminer.Dir(a.Paths), "adminer.php"), []byte("x"), 0o644)
		a.State.AddSite(config.Site{Name: adminer.SiteName, Path: adminer.Dir(a.Paths), Secure: true})

		dir, err := a.UninstallEngine(context.Background(), "mssql", false, deleteData)
		if err != nil || dir != "" {
			t.Fatalf("UninstallEngine = %q, %v", dir, err)
		}
		if gotDelete == nil || *gotDelete != deleteData {
			t.Errorf("deleteData not passed through: %v", gotDelete)
		}
		c := a.State.Config
		if c.MSSQL != "" || c.MSSQLStopped {
			t.Errorf("config not cleared: %+v", c)
		}
		if keep := !deleteData; (c.MSSQLPassword != "") != keep {
			t.Errorf("deleteData=%v: password kept=%v (a kept volume needs it)", deleteData, c.MSSQLPassword != "")
		}
		if a.State.FindSite(adminer.SiteName) != nil || adminer.Installed(a.Paths) {
			t.Error("Adminer must go with SQL Server")
		}
	}
}

func TestAdminerSiteInCaddyfile(t *testing.T) {
	a := sandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	// Pre-place Adminer so EnsureAdminer downloads nothing.
	os.MkdirAll(adminer.Dir(a.Paths), 0o755)
	os.WriteFile(filepath.Join(adminer.Dir(a.Paths), "adminer.php"), []byte("<?php"), 0o644)

	url, err := a.EnsureAdminer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://adminer.test/?mssql=127.0.0.1%3A"+strconv.Itoa(mssql.Port)+"&username=sa" {
		t.Errorf("url = %s", url)
	}
	site := a.State.FindSite(adminer.SiteName)
	if site == nil || !site.Secure || site.Path != adminer.Dir(a.Paths) {
		t.Fatalf("site = %+v", site)
	}
	if _, err := os.Stat(filepath.Join(adminer.Dir(a.Paths), "index.php")); err != nil {
		t.Error("index.php wrapper missing")
	}
	if err := a.WriteCaddyfile(nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.Paths.Caddyfile())
	cf := string(b)
	port, _ := phpver.FcgiPort("8.4.1")
	block := cf[strings.Index(cf, "adminer.test {"):]
	for _, want := range []string{"adminer.test {", "\ttls internal\n", "root * \"" + adminer.Dir(a.Paths) + "\"", "php_fastcgi 127.0.0.1:" + strconv.Itoa(port)} {
		if !strings.Contains(block, want) {
			t.Errorf("Caddyfile misses %q:\n%s", want, cf)
		}
	}

	tools := a.AdminTools()
	var found bool
	for _, tool := range tools {
		if tool.Name == "adminer" {
			found = true
			if !tool.Installed || tool.URL == "" || tool.Kind != "site" {
				t.Errorf("adminer tool = %+v", tool)
			}
		}
	}
	if !found {
		t.Error("AdminTools lacks adminer")
	}
	if err := a.UninstallAdminTool("adminer"); err != nil || a.State.FindSite(adminer.SiteName) != nil {
		t.Errorf("UninstallAdminTool: %v", err)
	}
}

func TestEnsureAdminerNeedsPHP(t *testing.T) {
	a := sandboxApp(t)
	if _, err := a.EnsureAdminer(context.Background()); err == nil || !strings.Contains(err.Error(), "PHP") {
		t.Errorf("err = %v", err)
	}
}

func TestPickSQLServerExt(t *testing.T) {
	cases := []struct {
		exts []phpver.Ext
		want string
	}{
		{nil, ""},
		{[]phpver.Ext{{Name: "pdo_mysql", Enabled: true}, {Name: "sqlsrv", Enabled: false}}, ""},
		{[]phpver.Ext{{Name: "pdo_dblib", Enabled: true}, {Name: "pdo_sqlsrv", Enabled: true}}, "pdo_sqlsrv"},
		{[]phpver.Ext{{Name: "sqlsrv", Enabled: true}, {Name: "pdo_sqlsrv", Enabled: true}}, "sqlsrv"},
		{[]phpver.Ext{{Name: "pdo_dblib", Enabled: true}}, "pdo_dblib"},
	}
	for _, c := range cases {
		if got := pickSQLServerExt(c.exts); got != c.want {
			t.Errorf("%+v -> %q, want %q", c.exts, got, c.want)
		}
	}
	a := sandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.1"
	old, oldReady := listPhpExtensions, sqlServerDriverReady
	listPhpExtensions = func(pmdir.Paths, string) ([]phpver.Ext, error) {
		return []phpver.Ext{{Name: "pdo_sqlsrv", Enabled: true}}, nil
	}
	ready := true
	sqlServerDriverReady = func(pmdir.Paths, string) bool { return ready }
	t.Cleanup(func() { listPhpExtensions, sqlServerDriverReady = old, oldReady })
	if got := a.SQLServerPHPExtension(); got != "pdo_sqlsrv" {
		t.Errorf("SQLServerPHPExtension = %q", got)
	}
	ready = false // e.g. Windows without the ODBC driver
	if got := a.SQLServerPHPExtension(); got != "" {
		t.Errorf("driver not ready, got %q", got)
	}
}

func TestMSSQLPortConfig(t *testing.T) {
	oldPort := mssql.Port
	t.Cleanup(func() { mssql.Port = oldPort })
	ApplyPortConfig(config.Config{MSSQLPort: 14333})
	if mssql.Port != 14333 {
		t.Errorf("mssql.Port = %d", mssql.Port)
	}
	ApplyPortConfig(config.Config{})
	if mssql.Port != mssql.DefaultPort {
		t.Errorf("default mssql.Port = %d", mssql.Port)
	}
	for _, in := range []string{"mssql", "sqlserver", "SQL-Server"} {
		if got, err := normalizeEngine(in); err != nil || got != "mssql" {
			t.Errorf("normalizeEngine(%q) = %q, %v", in, got, err)
		}
	}
	a := sandboxApp(t)
	a.State.Config.MSSQL = mssql.Series
	var listed bool
	for _, s := range a.services() {
		if s.key == "mssql" && s.info.Port == mssql.Port && s.info.Configurable {
			listed = true
		}
	}
	if !listed {
		t.Error("SQL Server's port is not on the Ports page")
	}
	if got := a.reservedBy(mssql.Port, "mysql"); got == "" {
		t.Error("SQL Server's port must be reserved")
	}
}

func TestMSSQLEnvSnippet(t *testing.T) {
	env := MSSQLEnvSnippet(14330, "Pw_1234x", true)
	for _, want := range []string{"DB_CONNECTION=sqlsrv\n", "DB_HOST=127.0.0.1\n", "DB_PORT=14330\n", "DB_USERNAME=sa\n", "DB_PASSWORD=Pw_1234x\n", "DB_TRUST_SERVER_CERTIFICATE=true\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env misses %q", want)
		}
	}
	if strings.Contains(MSSQLEnvSnippet(1433, "x", false), "TRUST") {
		t.Error("dblib (macOS) needs no TrustServerCertificate")
	}
}
