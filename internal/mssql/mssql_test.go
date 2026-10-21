package mssql

import (
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestFindDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the OrbStack/Docker lookup is macOS-only (Unix paths)")
	}
	none := func(string) (string, error) { return "", errors.New("not found") }
	onPath := func(string) (string, error) { return "/somewhere/bin/docker", nil }
	noFiles := func(string) bool { return false }

	if got := findDocker(onPath, noFiles, "/Users/x"); got != "/somewhere/bin/docker" {
		t.Errorf("PATH hit: got %q", got)
	}
	orb := func(p string) bool { return p == "/Users/x/.orbstack/bin/docker" }
	if got := findDocker(none, orb, "/Users/x"); got != "/Users/x/.orbstack/bin/docker" {
		t.Errorf("OrbStack fallback: got %q", got)
	}
	brew := func(p string) bool { return p == "/opt/homebrew/bin/docker" }
	if got := findDocker(none, brew, "/Users/x"); got != "/opt/homebrew/bin/docker" {
		t.Errorf("Homebrew fallback: got %q", got)
	}
	// /usr/local/bin (OrbStack's symlink) wins over Homebrew.
	both := func(p string) bool { return p == "/opt/homebrew/bin/docker" || p == "/usr/local/bin/docker" }
	if got := findDocker(none, both, "/Users/x"); got != "/usr/local/bin/docker" {
		t.Errorf("order: got %q", got)
	}
	if got := findDocker(none, noFiles, "/Users/x"); got != "" {
		t.Errorf("none: got %q", got)
	}
}

func TestRunArgs(t *testing.T) {
	args := runArgs(14330, Image)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d",
		"--name " + ContainerName,
		"--platform linux/amd64",
		"--restart no",
		"-e ACCEPT_EULA=Y",
		"-e MSSQL_PID=EnterpriseDeveloper",
		"-p 127.0.0.1:14330:1433",
		"-v " + VolumeName + ":/var/opt/mssql",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run args miss %q:\n%s", want, joined)
		}
	}
	if args[len(args)-1] != Image {
		t.Errorf("image must be last, got %q", args[len(args)-1])
	}
	// The password is passed by name only, never by value.
	for i, a := range args {
		if strings.HasPrefix(a, "MSSQL_SA_PASSWORD") && a != "MSSQL_SA_PASSWORD" {
			t.Errorf("password value on the command line: %q", a)
		}
		if a == "-p" && !strings.HasPrefix(args[i+1], "127.0.0.1:") {
			t.Errorf("port must be published on loopback only: %q", args[i+1])
		}
	}
	if !strings.Contains(joined, "-e MSSQL_SA_PASSWORD -p") {
		t.Errorf("expected a value-less -e MSSQL_SA_PASSWORD:\n%s", joined)
	}
}

func TestSqlcmdArgs(t *testing.T) {
	joined := strings.Join(sqlcmdArgs(), " ")
	for _, want := range []string{"exec -i -e SQLCMDPASSWORD " + ContainerName, containerSqlcmd, "-U sa", " -C ", " -b ", "-i /dev/stdin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sqlcmd args miss %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, " -P ") {
		t.Errorf("password flag must not be used: %s", joined)
	}
}

func TestDockerEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the OrbStack socket is macOS-only (Unix paths)")
	}
	sock := "/h/.orbstack/run/docker.sock"
	exists := func(p string) bool { return p == sock }
	env := dockerEnv([]string{"PATH=/bin", "SQLCMDPASSWORD=stale"}, "/h", "Secret_123", exists)
	got := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/bin", "DOCKER_HOST=unix://" + sock, "MSSQL_SA_PASSWORD=Secret_123", "SQLCMDPASSWORD=Secret_123"} {
		if !strings.Contains(got, want) {
			t.Errorf("env misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stale") {
		t.Errorf("inherited password must be replaced:\n%s", got)
	}
	// An explicit DOCKER_HOST is respected; no socket = no DOCKER_HOST.
	env = dockerEnv([]string{"DOCKER_HOST=tcp://x:2375"}, "/h", "", exists)
	if strings.Count(strings.Join(env, "\n"), "DOCKER_HOST=") != 1 {
		t.Errorf("DOCKER_HOST duplicated: %v", env)
	}
	env = dockerEnv(nil, "/h", "", func(string) bool { return false })
	if len(env) != 0 {
		t.Errorf("expected empty env, got %v", env)
	}
}

func TestPublishedPort(t *testing.T) {
	if p := publishedPort("127.0.0.1:14330\n"); p != 14330 {
		t.Errorf("got %d", p)
	}
	if p := publishedPort(""); p != 0 {
		t.Errorf("got %d", p)
	}
}

func TestOrbStackDownloadURL(t *testing.T) {
	if u := OrbStackDownloadURL("arm64"); u != "https://orbstack.dev/download/stable/latest/arm64" {
		t.Errorf("arm64: %s", u)
	}
	if u := OrbStackDownloadURL("amd64"); u != "https://orbstack.dev/download/stable/latest/amd64" {
		t.Errorf("amd64: %s", u)
	}
}

func TestSetupInstallArgs(t *testing.T) {
	args := setupInstallArgs(SetupOptions{Password: "Pa ss_9x", SysAdmin: `DESKTOP-1\Jane Doe`})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"/Q", "/ACTION=Install", "/FEATURES=SQLEngine", "/INSTANCENAME=MULLION",
		"/SECURITYMODE=SQL", `/SAPWD="Pa ss_9x"`, `/SQLSYSADMINACCOUNTS="DESKTOP-1\Jane Doe"`,
		"/TCPENABLED=1", "/SQLSVCSTARTUPTYPE=Manual", "/IACCEPTSQLSERVERLICENSETERMS",
		"/SUPPRESSPRIVACYSTATEMENTNOTICE", "/UPDATEENABLED=False",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("setup args miss %q:\n%s", want, joined)
		}
	}
	if args[0] != "/Q" {
		t.Errorf("/Q first, got %q", args[0])
	}
	// No-space values stay unquoted.
	plain := strings.Join(setupInstallArgs(SetupOptions{Password: "Abc_1234", SysAdmin: `PC\jane`}), " ")
	if !strings.Contains(plain, "/SAPWD=Abc_1234 ") || !strings.Contains(plain, `/SQLSYSADMINACCOUNTS=PC\jane `) {
		t.Errorf("plain values should not be quoted: %s", plain)
	}
	un := strings.Join(setupUninstallArgs(), " ")
	if un != "/Q /ACTION=Uninstall /FEATURES=SQLEngine /INSTANCENAME=MULLION" {
		t.Errorf("uninstall args: %s", un)
	}
	if got := strings.Join(sseiDownloadArgs(`C:\Mullion\mssql\media`), " "); got != `/ACTION=Download /MEDIAPATH=C:\Mullion\mssql\media /MEDIATYPE=ISO /QUIET` {
		t.Errorf("ssei args: %s", got)
	}
}

func TestWinQuote(t *testing.T) {
	cases := map[string]string{"abc": "abc", "a b": `"a b"`, `a"b`: `"a""b"`, "": `""`}
	for in, want := range cases {
		if got := winQuote(in); got != want {
			t.Errorf("winQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTcpPortPlan(t *testing.T) {
	plan := tcpPortPlan("MSSQL17.MULLION", 14333)
	want := `HKLM:\SOFTWARE\Microsoft\Microsoft SQL Server\MSSQL17.MULLION\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`
	if len(plan) != 2 {
		t.Fatalf("plan: %+v", plan)
	}
	if plan[0].Key != want || plan[0].Name != "TcpPort" || plan[0].Value != "14333" {
		t.Errorf("TcpPort: %+v", plan[0])
	}
	if plan[1].Key != want || plan[1].Name != "TcpDynamicPorts" || plan[1].Value != "" {
		t.Errorf("TcpDynamicPorts: %+v", plan[1])
	}
	block := psPortBlock(14333)
	if !strings.Contains(block, `'HKLM:\SOFTWARE\Microsoft\Microsoft SQL Server\' + $id + '\MSSQLServer\SuperSocketNetLib\Tcp\IPAll'`) {
		t.Errorf("port block does not splice the instance id:\n%s", block)
	}
	if !strings.Contains(block, "-Name 'TcpPort' -Value '14333'") || !strings.Contains(block, "-Name 'TcpDynamicPorts' -Value ''") {
		t.Errorf("port block values:\n%s", block)
	}
}

func TestInstallScript(t *testing.T) {
	s := installScript(InstallScriptParams{
		LogFile: `C:\Mullion\mssql\install.log`, SSEI: `C:\Mullion\mssql\SQL2025-SSEI-EntDev.exe`,
		MediaDir: `C:\Mullion\mssql\media`, StageDir: `C:\Mullion\mssql\backup`, Port: 1433,
		UserSID: "S-1-5-21-1-2-3-1001", Setup: SetupOptions{Password: "O'Neil_99", SysAdmin: `PC\o'neil`},
	})
	for _, want := range []string{
		"$ErrorActionPreference = 'Stop'",
		"Mount-DiskImage",
		"/MEDIATYPE=ISO",
		"/SAPWD=O''Neil_99", // the single quote is doubled inside the PS literal
		"-Name 'TcpPort' -Value '1433'",
		"(A;;CCLCSWRPWPDTLOCRRC;;;' + $sid + ')",
		"$sid = 'S-1-5-21-1-2-3-1001'",
		`'NT SERVICE\MSSQL$MULLION:(OI)(CI)M'`,
		"Restart-Service -Name $svc -Force",
		"Log 'DONE'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install script misses %q", want)
		}
	}
	u := uninstallScript(`C:\l.log`, true)
	if !strings.Contains(u, "/ACTION=Uninstall") || !strings.Contains(u, "Split-Path -Parent $root") {
		t.Errorf("uninstall script:\n%s", u)
	}
	if strings.Contains(uninstallScript(`C:\l.log`, false), "Split-Path -Parent $root") {
		t.Error("keep-data uninstall must not delete the instance folder")
	}
}

func TestQueryScriptKeepsSecretsOut(t *testing.T) {
	s := queryScript(1433)
	if !strings.Contains(s, "$env:MULLION_MSSQL_PW") || !strings.Contains(s, "$env:MULLION_MSSQL_SQL") {
		t.Error("query script must read the password and SQL from the environment")
	}
	if !strings.Contains(s, "'tcp:127.0.0.1,1433'") {
		t.Error("query script must target the configured port")
	}
	enc := encodeCommand("Write-Output 'hé'")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(u)); got != "Write-Output 'hé'" {
		t.Errorf("encodeCommand round trip: %q", got)
	}
}

func TestPasswords(t *testing.T) {
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidPassword(pw); err != nil {
			t.Fatalf("generated %q is invalid: %v", pw, err)
		}
		if strings.ContainsAny(pw, " '\"`$&;|\\") {
			t.Fatalf("generated %q has shell-hostile characters", pw)
		}
	}
	for _, bad := range []string{"short1A", "alllowercase", "ALLUPPER123", "has'quote1A"} {
		if ValidPassword(bad) == nil {
			t.Errorf("ValidPassword(%q) should fail", bad)
		}
	}
	if err := ValidPassword("Lower_and_1"); err != nil {
		t.Errorf("valid password refused: %v", err)
	}
}

func TestDatabaseNames(t *testing.T) {
	for _, ok := range []string{"shop", "my_app", "app-2", "_x"} {
		if err := ValidDatabaseName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1abc", "a b", "x]; DROP", "master", "TempDB"} {
		if ValidDatabaseName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if quoteIdent("a]b") != "[a]]b]" || quoteString("O'k") != "N'O''k'" {
		t.Error("quoting")
	}
}

func TestBackupNaming(t *testing.T) {
	if backupFileName("shop") != "shop.bak" {
		t.Error(backupFileName("shop"))
	}
	if DatabaseFromBackupFile("/x/y/shop.bak") != "shop" {
		t.Error(DatabaseFromBackupFile("/x/y/shop.bak"))
	}
	if sql := backupSQL("shop", "/var/opt/mssql/backup/shop.bak"); sql != "BACKUP DATABASE [shop] TO DISK = N'/var/opt/mssql/backup/shop.bak' WITH COPY_ONLY, INIT, FORMAT;" {
		t.Error(sql)
	}
}

func TestRestoreSQLMovesFiles(t *testing.T) {
	out := "shop\tC:\\Data\\shop.mdf\tD\tPRIMARY\n" +
		"shop_2\tC:\\Data\\shop_2.ndf\tD\tPRIMARY\n" +
		"shop_log\tC:\\Data\\shop_log.ldf\tL\tNULL\n"
	files, err := parseFileList(out)
	if err != nil {
		t.Fatal(err)
	}
	sql := restoreSQL("shop", "/var/opt/mssql/backup/r.bak", files, "/var/opt/mssql/data/", "/var/opt/mssql/data", "/")
	for _, want := range []string{
		"ALTER DATABASE [shop] SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"RESTORE DATABASE [shop] FROM DISK = N'/var/opt/mssql/backup/r.bak' WITH REPLACE, RECOVERY",
		"MOVE N'shop' TO N'/var/opt/mssql/data/shop_shop.mdf'",
		"MOVE N'shop_2' TO N'/var/opt/mssql/data/shop_shop_2.ndf'",
		"MOVE N'shop_log' TO N'/var/opt/mssql/data/shop_shop_log.ldf'",
		"SET MULTI_USER",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("restore SQL misses %q:\n%s", want, sql)
		}
	}
	if _, err := parseFileList(""); err == nil {
		t.Error("empty file list must fail")
	}
}

func TestParseRows(t *testing.T) {
	rows := parseRows("a\tb \r\n\n c\t d\n")
	if len(rows) != 2 || rows[0][1] != "b" || rows[1][0] != "c" || rows[1][1] != "d" {
		t.Errorf("%q", rows)
	}
}
