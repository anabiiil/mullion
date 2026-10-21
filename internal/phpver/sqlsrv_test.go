package phpver

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestSQLServerExtensionsPerPlatform(t *testing.T) {
	got := SQLServerExtensions()
	want := []string{"pdo_dblib"}
	if runtime.GOOS == "windows" {
		want = []string{"sqlsrv", "pdo_sqlsrv"}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("SQLServerExtensions() = %v, want %v", got, want)
	}
}

func TestIsSQLServerExtension(t *testing.T) {
	for _, in := range []string{"sqlsrv", "PDO_SQLSRV", "php_pdo_sqlsrv.dll", "pdo_dblib", "pdo_dblib.so", "mssql", " dblib "} {
		if !IsSQLServerExtension(in) {
			t.Errorf("IsSQLServerExtension(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"ldap", "pdo_mysql", "odbc", ""} {
		if IsSQLServerExtension(in) {
			t.Errorf("IsSQLServerExtension(%q) = true, want false", in)
		}
	}
}

// releases mirrors the shape of the GitHub API listing (newest first).
var testReleases = []ghRelease{
	{TagName: "v5.14.0-beta1", Prerelease: true, Assets: []ghAsset{{Name: "Windows_5.14.0-beta1beta1.zip", URL: "u-beta"}}},
	{TagName: "v5.14.0", Draft: true, Assets: []ghAsset{{Name: "Windows_5.14.0RTW.zip", URL: "u-draft"}}},
	{TagName: "v5.13.3", Assets: []ghAsset{
		{Name: "Linux_5.13.3RTW.tar.gz", URL: "u-linux"},
		{Name: "pdo_sqlsrv-5.13.3.tgz", URL: "u-pecl"},
		{Name: "Windows_5.13.3RTW.zip", URL: "u-5133"},
	}},
	{TagName: "v5.11.1", Assets: []ghAsset{
		{Name: "Windows-8.0.zip", URL: "u-80"},
		{Name: "Windows-8.2.zip", URL: "u-82"},
	}},
}

func TestPickMsphpsqlAssetSkipsPrereleaseAndDraft(t *testing.T) {
	tag, a, ok := pickMsphpsqlAsset(testReleases, "8.4")
	if !ok || tag != "v5.13.3" || a.URL != "u-5133" {
		t.Fatalf("got (%q, %+v, %v), want v5.13.3 bundle", tag, a, ok)
	}
}

func TestPickMsphpsqlAssetLegacyPerBranch(t *testing.T) {
	// Only legacy releases: the per-branch zip for the branch wins, and a
	// release without one (or a bundle) is skipped.
	legacy := []ghRelease{
		{TagName: "v5.11.1", Assets: []ghAsset{{Name: "Windows-8.1.zip", URL: "u-81"}, {Name: "Windows-8.2.zip", URL: "u-82"}}},
		{TagName: "v5.10.0", Assets: []ghAsset{{Name: "Windows-8.0.zip", URL: "u-80"}}},
	}
	if tag, a, ok := pickMsphpsqlAsset(legacy, "8.0"); !ok || tag != "v5.10.0" || a.URL != "u-80" {
		t.Fatalf("8.0: got (%q, %+v, %v)", tag, a, ok)
	}
	if _, _, ok := pickMsphpsqlAsset(legacy, "8.4"); ok {
		t.Fatal("8.4: expected no asset in legacy-only listing")
	}
	if _, _, ok := pickMsphpsqlAsset(nil, "8.4"); ok {
		t.Fatal("empty listing: expected no asset")
	}
}

func TestMsphpsqlDLLName(t *testing.T) {
	cases := []struct {
		ext, branch string
		ts          bool
		want        string
	}{
		{"sqlsrv", "8.4", false, "php_sqlsrv_84_nts_x64.dll"},
		{"pdo_sqlsrv", "8.3", true, "php_pdo_sqlsrv_83_ts_x64.dll"},
		{"pdo_sqlsrv", "8.10", false, "php_pdo_sqlsrv_810_nts_x64.dll"},
	}
	for _, c := range cases {
		if got := msphpsqlDLLName(c.ext, c.branch, c.ts); got != c.want {
			t.Errorf("msphpsqlDLLName(%q,%q,%v) = %q, want %q", c.ext, c.branch, c.ts, got, c.want)
		}
	}
}

func TestMatchMsphpsqlDLL(t *testing.T) {
	yes := []struct {
		entry, ext, branch string
		ts                 bool
	}{
		{"Windows/php_sqlsrv_84_nts_x64.dll", "sqlsrv", "8.4", false},
		{"Windows/php_pdo_sqlsrv_85_ts_x64.dll", "pdo_sqlsrv", "8.5", true},
		{`x64\php_sqlsrv_82_nts.dll`, "sqlsrv", "8.2", false}, // legacy layout
	}
	for _, c := range yes {
		if !matchMsphpsqlDLL(c.entry, c.ext, c.branch, c.ts) {
			t.Errorf("expected %q to match %s %s ts=%v", c.entry, c.ext, c.branch, c.ts)
		}
	}
	no := []struct {
		entry, ext, branch string
		ts                 bool
	}{
		{"Windows/php_sqlsrv_84_ts_x64.dll", "sqlsrv", "8.4", false},      // TS vs NTS
		{"Windows/php_sqlsrv_84_nts_x86.dll", "sqlsrv", "8.4", false},     // x86
		{"Windows/php_pdo_sqlsrv_84_nts_x64.dll", "sqlsrv", "8.4", false}, // other ext
		{"Windows/php_sqlsrv_83_nts_x64.dll", "sqlsrv", "8.4", false},     // other branch
		{"x86/php_sqlsrv_82_nts.dll", "sqlsrv", "8.2", false},             // legacy x86
	}
	for _, c := range no {
		if matchMsphpsqlDLL(c.entry, c.ext, c.branch, c.ts) {
			t.Errorf("expected %q NOT to match %s %s ts=%v", c.entry, c.ext, c.branch, c.ts)
		}
	}
}

func TestMsphpsqlBranches(t *testing.T) {
	entries := []string{
		"Windows/",
		"Windows/php_sqlsrv_85_ts_x64.dll",
		"Windows/php_sqlsrv_84_nts_x64.dll",
		"Windows/php_sqlsrv_83_nts_x86.dll",
		"Windows/php_sqlsrv_83_nts_x64.dll",
		"Windows/php_pdo_sqlsrv_85_nts_x64.dll",
		"Windows/php_sqlsrv_85_nts_x64.dll",
	}
	got := msphpsqlBranches(entries, "sqlsrv", false)
	slices.Sort(got)
	if want := []string{"8.3", "8.4", "8.5"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseRegSZ(t *testing.T) {
	out := "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\ODBC\\ODBCINST.INI\\ODBC Driver 18 for SQL Server\r\n" +
		"    Driver    REG_SZ    C:\\Windows\\system32\\msodbcsql18.dll\r\n\r\n"
	if got, ok := parseRegSZ(out, "Driver"); !ok || got != `C:\Windows\system32\msodbcsql18.dll` {
		t.Fatalf("got (%q, %v)", got, ok)
	}
	spaced := "HKEY_LOCAL_MACHINE\\...\n    Driver    REG_EXPAND_SZ    C:\\Program Files\\Microsoft ODBC\\msodbcsql18.dll\n"
	if got, ok := parseRegSZ(spaced, "driver"); !ok || got != `C:\Program Files\Microsoft ODBC\msodbcsql18.dll` {
		t.Fatalf("spaced: got (%q, %v)", got, ok)
	}
	for _, bad := range []string{
		"",
		"ERROR: The system was unable to find the specified registry key or value.\n",
		"    UsageCount    REG_DWORD    0x1\n",
		"    Driver    REG_DWORD    0x1\n",
	} {
		if got, ok := parseRegSZ(bad, "Driver"); ok {
			t.Errorf("parseRegSZ(%q) = (%q, true), want not found", bad, got)
		}
	}
}

func TestODBCDriverRegKey(t *testing.T) {
	if got := odbcDriverRegKey(odbcDriverNames[0]); got != `HKLM\SOFTWARE\ODBC\ODBCINST.INI\ODBC Driver 18 for SQL Server` {
		t.Fatalf("got %q", got)
	}
}

func TestOdbcMsiArgs(t *testing.T) {
	got := odbcMsiArgs(`C:\t\msodbcsql18.msi`, `C:\l\odbc.log`)
	want := []string{"/i", `C:\t\msodbcsql18.msi`, "/quiet", "/norestart", "IACCEPTMSODBCSQLLICENSETERMS=YES", "/l*v", `C:\l\odbc.log`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := odbcMsiArgs("x.msi", ""); slices.Contains(got, "/l*v") {
		t.Fatalf("no log path should mean no /l*v: %v", got)
	}
	for code, want := range map[int]bool{0: true, 3010: true, 1641: true, 1603: false, 1602: false, 1: false} {
		if msiExitOK(code) != want {
			t.Errorf("msiExitOK(%d) = %v, want %v", code, !want, want)
		}
	}
}

func TestFreetdsConfigureArgs(t *testing.T) {
	args := freetdsConfigureArgs("/x/lib/freetds")
	for _, want := range []string{
		"--prefix=/x/lib/freetds", "--enable-static", "--disable-shared",
		"--disable-odbc", "--disable-apps", "--disable-server", "--disable-pool",
		"--without-openssl", "--without-gnutls", "--with-tdsver=7.4",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("FreeTDS configure args %v missing %q", args, want)
		}
	}
}

func TestPdoDblibConfigure(t *testing.T) {
	args := pdoDblibConfigureArgs("/x/lib/freetds")
	for _, want := range []string{"--disable-all", "--enable-pdo", "--with-pdo-dblib=shared,/x/lib/freetds"} {
		if !slices.Contains(args, want) {
			t.Errorf("php configure args %v missing %q", args, want)
		}
	}
	if env := pdoDblibConfigureEnv(); !slices.Contains(env, "LDFLAGS=-liconv") {
		t.Errorf("configure env %v must link iconv (libsybdb.a needs it)", env)
	}
}

func TestFreetdsCacheDirPerArch(t *testing.T) {
	p := pmdir.Paths{Home: "/h/.mullion"}
	a, b := freetdsCacheDir(p, "arm64"), freetdsCacheDir(p, "amd64")
	if a == b {
		t.Fatal("cache dir must differ per arch")
	}
	if want := filepath.Join("/h/.mullion", "lib", "freetds-"+freetdsVersion+"-arm64"); a != want {
		t.Fatalf("got %q, want %q", a, want)
	}
}

func TestFreetdsConfDisablesEncryption(t *testing.T) {
	if !strings.Contains(freetdsConf, "encryption = off") || !strings.Contains(freetdsConf, "tds version = 7.4") {
		t.Fatalf("unexpected freetds.conf:\n%s", freetdsConf)
	}
}

func countActive(t *testing.T, iniPath, so string) (active, commented int) {
	t.Helper()
	data, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		switch l {
		case `extension="` + so + `"`:
			active++
		case `;extension="` + so + `"`:
			commented++
		}
	}
	return
}

func TestSetSharedExtLineLifecycle(t *testing.T) {
	dir := t.TempDir()
	iniPath := filepath.Join(dir, "php.ini")
	ldap := filepath.Join(dir, "ext", "ldap.so")
	so := filepath.Join(dir, "ext", "pdo_dblib.so")
	base := "display_errors=On\n\nextension=\"" + ldap + "\"\n"
	if err := os.WriteFile(iniPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	// Enable twice: one active line, with the comment, ldap untouched.
	for i := 0; i < 2; i++ {
		if err := setSharedExtLine(iniPath, so, "SQL Server (pdo_dblib)", true); err != nil {
			t.Fatal(err)
		}
	}
	if a, c := countActive(t, iniPath, so); a != 1 || c != 0 {
		t.Fatalf("after enable: active=%d commented=%d", a, c)
	}
	data, _ := os.ReadFile(iniPath)
	if !strings.Contains(string(data), "; SQL Server (pdo_dblib)\n") || !strings.Contains(string(data), "display_errors=On") {
		t.Fatalf("missing comment or clobbered content:\n%s", data)
	}

	// Disable: commented out, ldap still active.
	if err := setSharedExtLine(iniPath, so, "", false); err != nil {
		t.Fatal(err)
	}
	if a, c := countActive(t, iniPath, so); a != 0 || c != 1 {
		t.Fatalf("after disable: active=%d commented=%d", a, c)
	}
	if a, _ := countActive(t, iniPath, ldap); a != 1 {
		t.Fatalf("ldap line was touched")
	}

	// Re-enable: the commented line is reused, not duplicated.
	if err := setSharedExtLine(iniPath, so, "SQL Server (pdo_dblib)", true); err != nil {
		t.Fatal(err)
	}
	if a, c := countActive(t, iniPath, so); a != 1 || c != 0 {
		t.Fatalf("after re-enable: active=%d commented=%d", a, c)
	}
	data, _ = os.ReadFile(iniPath)
	if n := strings.Count(string(data), "; SQL Server (pdo_dblib)"); n != 1 {
		t.Fatalf("comment duplicated (%d):\n%s", n, data)
	}

	info, err := os.Stat(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm() != 0o600) {
		t.Fatalf("mode changed to %v", info.Mode().Perm())
	}
}

func TestSetSharedExtLineMatchesByFileName(t *testing.T) {
	dir := t.TempDir()
	iniPath := filepath.Join(dir, "php.ini")
	// An older line pointing at another location of the same module.
	if err := os.WriteFile(iniPath, []byte("extension=\"/old/place/pdo_dblib.so\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	so := filepath.Join(dir, "ext", "pdo_dblib.so")
	if err := setSharedExtLine(iniPath, so, "c", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(iniPath)
	if strings.Contains(string(data), so) {
		t.Fatalf("an active line for the same module already existed; nothing should be added:\n%s", data)
	}
	// A missing ini is created.
	fresh := filepath.Join(dir, "new.ini")
	if err := setSharedExtLine(fresh, so, "", true); err != nil {
		t.Fatal(err)
	}
	if a, _ := countActive(t, fresh, so); a != 1 {
		t.Fatal("expected the extension line in a newly created ini")
	}
}

func TestParseExtLineSharedModule(t *testing.T) {
	name, active, zend := parseExtLine(`;extension="/Users/x/.mullion/php/8.4.23/ext/pdo_dblib.so"`)
	if name != "pdo_dblib" || active || zend {
		t.Fatalf("got (%q, %v, %v)", name, active, zend)
	}
	if name, _, _ := parseExtLine("; SQL Server (pdo_dblib) — built against php.net sources"); name != "" {
		t.Fatalf("a comment line parsed as %q", name)
	}
}
