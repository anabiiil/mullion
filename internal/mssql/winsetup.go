package mssql

// Pure builders for the Windows backend: setup.exe switches, the TCP
// port registry plan and the PowerShell scripts that run elevated. They
// live in an untagged file so their tests run on every platform; the
// code that runs them is in native_windows.go.

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// SSEIURL is Microsoft's SQL Server 2025 Enterprise Developer
// bootstrapper (SSEI). The fwlink is the "Download Enterprise Developer"
// button on https://www.microsoft.com/sql-server/sql-server-downloads;
// it redirects (via aka.ms/sql2025EnterpriseDev) to
// https://download.microsoft.com/download/4ba126fc-a6a0-4810-80e9-c0182d3e1f62/SQL2025-SSEI-EntDev.exe
const SSEIURL = "https://go.microsoft.com/fwlink/?linkid=2344711"

// sseiDownloadArgs makes the bootstrapper download the full install
// media as an ISO without installing anything. These switches are the
// bootstrapper's own (`SQL2025-SSEI-EntDev.exe /?`); Microsoft Learn does
// not document them separately.
func sseiDownloadArgs(mediaDir string) []string {
	return []string{"/ACTION=Download", "/MEDIAPATH=" + winQuote(mediaDir), "/MEDIATYPE=ISO", "/QUIET"}
}

// SetupOptions are the values setup.exe needs for Mullion's instance.
type SetupOptions struct {
	Password string // sa password
	// SysAdmin is the Windows account made a sysadmin, "DOMAIN\user" —
	// the user who installed Mullion (not whichever admin approved UAC).
	SysAdmin string
}

// setupInstallArgs is the unattended install of the database engine as
// the MULLION named instance, with SQL logins enabled (sa), TCP on and
// the service on manual start (Mullion starts it). Every switch is from
// https://learn.microsoft.com/sql/database-engine/install-windows/install-sql-server-from-the-command-prompt :
//   - /Q needs /IACCEPTSQLSERVERLICENSETERMS and
//     /SUPPRESSPRIVACYSTATEMENTNOTICE ("Required, when the /Q or /QS
//     parameter is specified").
//   - /SECURITYMODE=SQL requires /SAPWD.
//   - /UPDATEENABLED=False: no slow online update scan during setup.
//   - Force Encryption stays at setup's default (off), so PHP drivers
//     with encryption off still connect.
//   - /BROWSERSVCSTARTUPTYPE=Disabled: the port is static, so the SQL
//     Browser service has nothing to do.
func setupInstallArgs(o SetupOptions) []string {
	return []string{
		"/Q",
		"/ACTION=Install",
		"/FEATURES=SQLEngine",
		"/INSTANCENAME=" + InstanceName,
		"/SECURITYMODE=SQL",
		"/SAPWD=" + winQuote(o.Password),
		"/SQLSYSADMINACCOUNTS=" + winQuote(o.SysAdmin),
		"/TCPENABLED=1",
		"/SQLSVCSTARTUPTYPE=Manual",
		"/AGTSVCSTARTUPTYPE=Manual",
		"/BROWSERSVCSTARTUPTYPE=Disabled",
		"/UPDATEENABLED=False",
		"/IACCEPTSQLSERVERLICENSETERMS",
		"/SUPPRESSPRIVACYSTATEMENTNOTICE",
	}
}

// setupUninstallArgs removes the MULLION instance (the "Uninstall
// parameters" table of the same page: /ACTION=Uninstall, /FEATURES,
// /INSTANCENAME).
func setupUninstallArgs() []string {
	return []string{"/Q", "/ACTION=Uninstall", "/FEATURES=SQLEngine", "/INSTANCENAME=" + InstanceName}
}

// winQuote wraps a value in double quotes for a Windows command line
// when it needs them (spaces, tabs, quotes); embedded quotes are
// doubled, which is how setup.exe's parser reads them.
func winQuote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\"") {
		return v
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// RegValue is one registry value to write.
type RegValue struct {
	Key   string // full PowerShell path, e.g. HKLM:\SOFTWARE\...
	Name  string
	Value string
}

// instanceNamesKey maps instance names to instance IDs
// (MULLION -> MSSQL17.MULLION).
const instanceNamesKey = `HKLM:\SOFTWARE\Microsoft\Microsoft SQL Server\Instance Names\SQL`

// tcpPortPlan is the registry change that pins the instance to a static
// TCP port: under SuperSocketNetLib\Tcp\IPAll set TcpPort and blank
// TcpDynamicPorts (a named instance otherwise picks a dynamic port).
// Source: https://learn.microsoft.com/sql/database-engine/configure-windows/configure-a-server-to-listen-on-a-specific-tcp-port
// (the "IPAll" section, done through the registry SQL Server
// Configuration Manager edits).
func tcpPortPlan(instanceID string, port int) []RegValue {
	key := `HKLM:\SOFTWARE\Microsoft\Microsoft SQL Server\` + instanceID + `\MSSQLServer\SuperSocketNetLib\Tcp\IPAll`
	return []RegValue{
		{Key: key, Name: "TcpPort", Value: strconv.Itoa(port)},
		{Key: key, Name: "TcpDynamicPorts", Value: ""},
	}
}

// psQuote is a single-quoted PowerShell string literal.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// psPrelude starts every elevated script: stop on errors and log each
// step to a file the non-elevated side tails for progress.
func psPrelude(logFile string) string {
	return "$ErrorActionPreference = 'Stop'\n" +
		"$log = " + psQuote(logFile) + "\n" +
		"function Log([string]$m) { Add-Content -LiteralPath $log -Value $m -Encoding UTF8 }\n" +
		"$svc = " + psQuote(ServiceName) + "\n"
}

// psPortBlock writes tcpPortPlan for the installed instance (its ID is
// looked up, not guessed: MSSQL17.MULLION for SQL Server 2025).
func psPortBlock(port int) string {
	var b strings.Builder
	b.WriteString("$id = (Get-ItemProperty -LiteralPath " + psQuote(instanceNamesKey) + ")." + InstanceName + "\n")
	b.WriteString("if (-not $id) { throw 'SQL Server instance " + InstanceName + " is not registered' }\n")
	for _, v := range tcpPortPlan("__ID__", port) {
		key := strings.Replace(psQuote(v.Key), "__ID__", "' + $id + '", 1)
		b.WriteString(fmt.Sprintf("Set-ItemProperty -LiteralPath (%s) -Name %s -Value %s\n", key, psQuote(v.Name), psQuote(v.Value)))
	}
	return b.String()
}

// psServiceACLBlock lets the installing user start, stop and query the
// SQL Server service without administrator rights from then on: it
// appends an allow ACE for their SID to the service's DACL (SDDL rights
// CC LC SW RP WP DT LO CR RC = query config/status, enumerate
// dependents, start, stop, pause, interrogate, user control, read).
// Syntax: `sc.exe sdshow` / `sc.exe sdset`,
// https://learn.microsoft.com/windows-server/administration/windows-commands/sc-sdset
func psServiceACLBlock(userSID string) string {
	return "$sid = " + psQuote(userSID) + "\n" +
		"$sd = (& sc.exe sdshow $svc | Out-String).Trim()\n" +
		"if ($sd -and $sd -notmatch [regex]::Escape(';;;' + $sid + ')')) {\n" +
		"  $ace = '(A;;CCLCSWRPWPDTLOCRRC;;;' + $sid + ')'\n" +
		"  $i = $sd.IndexOf('S:')\n" +
		"  if ($i -ge 0) { $sd = $sd.Substring(0, $i) + $ace + $sd.Substring($i) } else { $sd = $sd + $ace }\n" +
		"  & sc.exe sdset $svc $sd | Out-Null\n" +
		"  if ($LASTEXITCODE -ne 0) { Log 'note: could not let your account start/stop the service; Mullion will ask for administrator rights each time' }\n" +
		"}\n"
}

// psStageACLBlock lets the service account write backups into (and
// read restores from) Mullion's staging folder. "NT SERVICE\MSSQL$X" is
// the instance's virtual service account (the setup default).
func psStageACLBlock(stageDir string) string {
	return "New-Item -ItemType Directory -Force -Path " + psQuote(stageDir) + " | Out-Null\n" +
		"& icacls.exe " + psQuote(stageDir) + " /grant " + psQuote(`NT SERVICE\`+ServiceName+`:(OI)(CI)M`) + " | Out-Null\n"
}

// InstallScriptParams are the inputs of the elevated install script.
type InstallScriptParams struct {
	LogFile  string
	SSEI     string // the downloaded bootstrapper
	MediaDir string // where it puts the ISO
	StageDir string // backup staging folder
	Port     int
	UserSID  string
	Setup    SetupOptions
}

// installScript is the one elevated PowerShell script an install runs —
// one UAC prompt for: downloading the media, mounting the ISO, running
// setup, pinning the TCP port, granting the user start/stop rights and
// the service access to the backup folder, and restarting the service
// on the new port.
func installScript(p InstallScriptParams) string {
	var b strings.Builder
	b.WriteString(psPrelude(p.LogFile))
	b.WriteString("try {\n")
	b.WriteString("Log 'Downloading the SQL Server 2025 installation media (about 1.5 GB)…'\n")
	b.WriteString("New-Item -ItemType Directory -Force -Path " + psQuote(p.MediaDir) + " | Out-Null\n")
	b.WriteString("$iso = Get-ChildItem -LiteralPath " + psQuote(p.MediaDir) + " -Filter *.iso -Recurse -ErrorAction SilentlyContinue | Sort-Object Length -Descending | Select-Object -First 1\n")
	b.WriteString("if (-not $iso) {\n")
	b.WriteString("  $p = Start-Process -FilePath " + psQuote(p.SSEI) + " -ArgumentList " + psQuote(strings.Join(sseiDownloadArgs(p.MediaDir), " ")) + " -Wait -PassThru\n")
	b.WriteString("  if ($p.ExitCode -ne 0) { throw ('the SQL Server downloader exited with code ' + $p.ExitCode) }\n")
	b.WriteString("  $iso = Get-ChildItem -LiteralPath " + psQuote(p.MediaDir) + " -Filter *.iso -Recurse | Sort-Object Length -Descending | Select-Object -First 1\n")
	b.WriteString("  if (-not $iso) { throw 'the SQL Server downloader produced no .iso file' }\n")
	b.WriteString("}\n")
	b.WriteString("Log 'Mounting the installation media…'\n")
	b.WriteString("$img = Mount-DiskImage -ImagePath $iso.FullName -PassThru\n")
	b.WriteString("try {\n")
	b.WriteString("  $drive = ($img | Get-Volume).DriveLetter\n")
	b.WriteString("  Log 'Running SQL Server setup (usually 5-15 minutes)…'\n")
	b.WriteString("  $p = Start-Process -FilePath ($drive + ':\\setup.exe') -ArgumentList " + psQuote(strings.Join(setupInstallArgs(p.Setup), " ")) + " -Wait -PassThru\n")
	// 3010 = success, reboot required to finish (winget's manifest for
	// this package lists it as such).
	b.WriteString("  if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw ('SQL Server setup exited with code ' + $p.ExitCode + ' - its log is under ' + $env:ProgramFiles + '\\Microsoft SQL Server\\*\\Setup Bootstrap\\Log') }\n")
	b.WriteString("  if ($p.ExitCode -eq 3010) { Log 'note: Windows wants a restart to finish the SQL Server install' }\n")
	b.WriteString("} finally { Dismount-DiskImage -ImagePath $iso.FullName | Out-Null }\n")
	b.WriteString("Log 'Configuring the TCP port and permissions…'\n")
	b.WriteString(psPortBlock(p.Port))
	b.WriteString(psServiceACLBlock(p.UserSID))
	b.WriteString(psStageACLBlock(p.StageDir))
	b.WriteString("if ((Get-Service -Name $svc).Status -eq 'Running') { Restart-Service -Name $svc -Force } else { Start-Service -Name $svc }\n")
	b.WriteString("Remove-Item -LiteralPath " + psQuote(p.MediaDir) + " -Recurse -Force -ErrorAction SilentlyContinue\n")
	b.WriteString("Log 'DONE'\n")
	b.WriteString("} catch { Log ('ERROR: ' + $_.Exception.Message); exit 1 }\n")
	return b.String()
}

// uninstallScript removes the instance with the setup.exe SQL Server
// leaves installed under "Setup Bootstrap" (the media is gone by then),
// and with deleteData also its folder (the user databases' files that
// setup leaves behind).
func uninstallScript(logFile string, deleteData bool) string {
	var b strings.Builder
	b.WriteString(psPrelude(logFile))
	b.WriteString("try {\n")
	b.WriteString("$id = (Get-ItemProperty -LiteralPath " + psQuote(instanceNamesKey) + " -ErrorAction SilentlyContinue)." + InstanceName + "\n")
	b.WriteString("$root = $null\n")
	b.WriteString("if ($id) { $root = (Get-ItemProperty -LiteralPath ('HKLM:\\SOFTWARE\\Microsoft\\Microsoft SQL Server\\' + $id + '\\Setup') -ErrorAction SilentlyContinue).SQLPath }\n")
	b.WriteString("if (Get-Service -Name $svc -ErrorAction SilentlyContinue) { Stop-Service -Name $svc -Force -ErrorAction SilentlyContinue }\n")
	b.WriteString("$setup = Get-ChildItem -Path ($env:ProgramFiles + '\\Microsoft SQL Server\\*\\Setup Bootstrap\\SQL*\\setup.exe') -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1\n")
	b.WriteString("if ($id) {\n")
	b.WriteString("  if (-not $setup) { throw 'SQL Server setup.exe was not found under Program Files - remove the instance from Settings > Apps' }\n")
	b.WriteString("  Log 'Running SQL Server setup to remove the instance…'\n")
	b.WriteString("  $p = Start-Process -FilePath $setup.FullName -ArgumentList " + psQuote(strings.Join(setupUninstallArgs(), " ")) + " -Wait -PassThru\n")
	b.WriteString("  if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw ('SQL Server setup exited with code ' + $p.ExitCode) }\n")
	b.WriteString("}\n")
	if deleteData {
		b.WriteString("if ($root) { Remove-Item -LiteralPath (Split-Path -Parent $root) -Recurse -Force -ErrorAction SilentlyContinue }\n")
	}
	b.WriteString("Log 'DONE'\n")
	b.WriteString("} catch { Log ('ERROR: ' + $_.Exception.Message); exit 1 }\n")
	return b.String()
}

// portScript pins the instance to a new port (elevated: the key is
// under HKLM). The service is restarted by the caller.
func portScript(logFile string, port int) string {
	return psPrelude(logFile) + "try {\n" + psPortBlock(port) + "Log 'DONE'\n} catch { Log ('ERROR: ' + $_.Exception.Message); exit 1 }\n"
}

// queryScript runs T-SQL over .NET's System.Data.SqlClient, which every
// Windows PowerShell 5.1 has — no sqlcmd to download or find. The SQL
// and the password arrive in environment variables (never on a command
// line); rows come back one per line, columns tab-separated, matching
// the container's sqlcmd output.
func queryScript(port int) string {
	return `$ErrorActionPreference = 'Stop'
try {
  $b = New-Object System.Data.SqlClient.SqlConnectionStringBuilder
  $b['Data Source'] = 'tcp:127.0.0.1,` + strconv.Itoa(port) + `'
  $b['User ID'] = 'sa'
  $b['Password'] = $env:MULLION_MSSQL_PW
  $b['Encrypt'] = $false
  $b['TrustServerCertificate'] = $true
  $b['Connect Timeout'] = 10
  $c = New-Object System.Data.SqlClient.SqlConnection $b.ConnectionString
  $c.Open()
  try {
    $cmd = $c.CreateCommand()
    $cmd.CommandTimeout = 0
    $cmd.CommandText = $env:MULLION_MSSQL_SQL
    $r = $cmd.ExecuteReader()
    do {
      while ($r.Read()) {
        $vals = @()
        for ($i = 0; $i -lt $r.FieldCount; $i++) { $vals += [string]$r.GetValue($i) }
        [Console]::Out.WriteLine(($vals -join "` + "`t" + `"))
      }
    } while ($r.NextResult())
    $r.Close()
  } finally { $c.Close() }
} catch {
  $e = $_.Exception
  while ($e.InnerException) { $e = $e.InnerException }
  [Console]::Error.WriteLine($e.Message)
  exit 1
}
`
}

// encodeCommand is a script in -EncodedCommand form: base64 of its
// UTF-16LE text (the PowerShell.exe docs, about_PowerShell_exe). The
// script itself holds no secrets — those travel in the environment.
func encodeCommand(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
