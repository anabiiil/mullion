//go:build windows

package mssql

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/elevate"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// OrbStackInstalled is always false on Windows (SQL Server is native).
func OrbStackInstalled() bool { return false }

// EnsureDaemon is a no-op on Windows: there is no container runtime.
func EnsureDaemon(ctx context.Context, timeout time.Duration) error { return nil }

// InstallOrbStack is macOS-only.
func InstallOrbStack(ctx context.Context, paths pmdir.Paths, progress func(string)) error {
	return fmt.Errorf("OrbStack is a macOS app — on Windows SQL Server installs natively")
}

// serviceState returns the service's STATE word from `sc.exe query`
// ("RUNNING", "STOPPED", ...; "" = no such service).
func serviceState() string {
	out, err := proc.Quiet("sc.exe", "query", ServiceName).CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "STATE" {
			continue
		}
		f := strings.Fields(v)
		if len(f) >= 2 {
			return f[1]
		}
	}
	return ""
}

// Status reports whether the instance's service exists.
func Status(paths pmdir.Paths) RuntimeStatus {
	return RuntimeStatus{Kind: "native", Service: serviceState() != ""}
}

// QuickStatus is Status (one `sc query`, cheap enough to poll).
func QuickStatus(paths pmdir.Paths) RuntimeStatus { return Status(paths) }

// Owned reports whether Mullion's instance is running.
func Owned() bool { return serviceState() == "RUNNING" }

// runElevatedScript writes a PowerShell script, runs it through one UAC
// prompt, and relays the lines it logs to progress while it runs. The
// script file is removed afterwards (it can hold the sa password).
func runElevatedScript(paths pmdir.Paths, name, script string, progress func(string)) error {
	dir := paths.MssqlDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	scriptPath := filepath.Join(dir, name+".ps1")
	logPath := filepath.Join(dir, name+".log")
	os.Remove(logPath)
	// A BOM makes Windows PowerShell 5.1 read the script as UTF-8.
	if err := os.WriteFile(scriptPath, append([]byte{0xEF, 0xBB, 0xBF}, []byte(script)...), 0o600); err != nil {
		return err
	}
	defer os.Remove(scriptPath)

	done := make(chan struct{})
	go func() {
		seen := 0
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
			lines := readLines(logPath)
			for ; seen < len(lines); seen++ {
				if l := lines[seen]; l != "DONE" && !strings.HasPrefix(l, "ERROR: ") && progress != nil {
					progress(l)
				}
			}
		}
	}()
	psArgs := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
	var err error
	if elevate.IsElevated() {
		// Already elevated (e.g. inside `mullion uninstall`'s elevated
		// window): no second UAC prompt.
		err = proc.Quiet("powershell.exe", psArgs...).Run()
	} else {
		err = elevate.Relaunch("powershell.exe", psArgs...)
	}
	close(done)
	for _, l := range readLines(logPath) {
		if strings.HasPrefix(l, "ERROR: ") {
			return fmt.Errorf("%s", strings.TrimPrefix(l, "ERROR: "))
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, strings.TrimPrefix(strings.TrimSpace(sc.Text()), "\ufeff"))
	}
	return out
}

// Install downloads Microsoft's SQL Server 2025 Enterprise Developer
// bootstrapper and, in one elevated script, downloads the media and
// installs the MULLION instance (see installScript), then waits for it.
func Install(ctx context.Context, paths pmdir.Paths, password string, progress func(string)) error {
	if err := ValidPassword(password); err != nil {
		return err
	}
	Password = password
	if progress == nil {
		progress = func(string) {}
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("looking up your Windows account: %w", err)
	}
	dir := paths.MssqlDir()
	stage := backupStageDir(paths)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	ssei := filepath.Join(dir, "SQL2025-SSEI-EntDev.exe")
	if !fileExists(ssei) {
		progress("Downloading Microsoft's SQL Server installer…")
		if err := downloadFile(ctx, SSEIURL, ssei); err != nil {
			return err
		}
	}
	progress("Installing SQL Server 2025 — accept the administrator (UAC) prompt; this takes 10-20 minutes…")
	script := installScript(InstallScriptParams{
		LogFile:  filepath.Join(dir, "install.log"),
		SSEI:     ssei,
		MediaDir: filepath.Join(dir, "media"),
		StageDir: stage,
		Port:     Port,
		UserSID:  u.Uid,
		Setup:    SetupOptions{Password: password, SysAdmin: u.Username},
	})
	if err := runElevatedScript(paths, "install", script, progress); err != nil {
		return fmt.Errorf("installing SQL Server: %w", err)
	}
	os.Remove(ssei)
	progress("Waiting for SQL Server to answer…")
	return waitReady(paths, 3*time.Minute)
}

// Uninstall removes the MULLION instance (elevated); deleteData also
// removes its leftover folder and Mullion's staging folder.
func Uninstall(ctx context.Context, paths pmdir.Paths, deleteData bool) error {
	if serviceState() == "" {
		if deleteData {
			os.RemoveAll(paths.MssqlDir())
		}
		return nil
	}
	if err := runElevatedScript(paths, "uninstall", uninstallScript(filepath.Join(paths.MssqlDir(), "uninstall.log"), deleteData), func(s string) { fmt.Println(s) }); err != nil {
		return fmt.Errorf("uninstalling SQL Server: %w", err)
	}
	if deleteData {
		os.RemoveAll(paths.MssqlDir())
	}
	return nil
}

// netService runs `net start|stop <service>` (which waits for the state
// change), retrying through UAC when this account was not granted
// start/stop rights.
func netService(verb string) error {
	out, err := proc.Quiet("net.exe", verb, ServiceName).CombinedOutput()
	if err == nil {
		return nil
	}
	text := string(out)
	if strings.Contains(text, "System error 5") || strings.Contains(strings.ToLower(text), "access is denied") {
		if err := elevate.Relaunch("net.exe", verb, ServiceName); err != nil {
			return fmt.Errorf("net %s %s: %w", verb, ServiceName, err)
		}
		return nil
	}
	return fmt.Errorf("net %s %s: %v: %s", verb, ServiceName, err, strings.TrimSpace(text))
}

// Ensure starts the service without waiting for SQL Server to accept
// logins.
func Ensure(paths pmdir.Paths) error {
	switch serviceState() {
	case "":
		return fmt.Errorf("the SQL Server service %s is missing — reinstall with `mullion mssql install`", ServiceName)
	case "RUNNING", "START_PENDING":
		return nil
	}
	return netService("start")
}

// Start starts the service and waits until SQL Server answers.
func Start(paths pmdir.Paths) error {
	if err := Ensure(paths); err != nil {
		return err
	}
	return waitReady(paths, 2*time.Minute)
}

// Stop stops the service (no-op when it isn't running or installed).
func Stop(paths pmdir.Paths) error {
	switch serviceState() {
	case "", "STOPPED", "STOP_PENDING":
		return nil
	}
	return netService("stop")
}

// StopFast asks the service to stop without waiting for it (`sc stop`
// returns at once), for a whole-stack `mullion stop`. Without the
// rights to stop it, it falls back to Stop (an administrator prompt).
func StopFast(paths pmdir.Paths) error {
	switch serviceState() {
	case "", "STOPPED", "STOP_PENDING":
		return nil
	}
	if err := proc.Quiet("sc.exe", "stop", ServiceName).Run(); err != nil {
		return Stop(paths)
	}
	return nil
}

// ApplyPort pins the instance's TCP port to Port in the registry
// (elevated). The caller restarts the service.
func ApplyPort(paths pmdir.Paths) error {
	if serviceState() == "" {
		return nil
	}
	return runElevatedScript(paths, "port", portScript(filepath.Join(paths.MssqlDir(), "port.log"), Port), nil)
}

// ── queries ───────────────────────────────────────────────────────

func query(paths pmdir.Paths, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return runSQL(ctx, sql)
}

func queryQuiet(paths pmdir.Paths, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runSQL(ctx, sql)
}

func runSQL(ctx context.Context, sql string) (string, error) {
	if Password == "" {
		return "", fmt.Errorf("no sa password is configured for SQL Server")
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(queryScript(Port)))
	proc.HideConsole(cmd)
	cmd.Env = append(os.Environ(), "MULLION_MSSQL_PW="+Password, "MULLION_MSSQL_SQL="+sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("SQL Server: %s", msg)
	}
	return stdout.String(), nil
}

// ── backup staging (a Mullion folder the service may write) ───────

func serverSep() string { return `\` }

func stagePath(paths pmdir.Paths, name string) (string, error) {
	dir := backupStageDir(paths)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func copyOut(paths pmdir.Paths, serverPath, local string) error {
	if err := copyFile(serverPath, local); err != nil {
		return err
	}
	os.Remove(serverPath)
	return nil
}

func copyIn(paths pmdir.Paths, local string) (string, error) {
	dst, err := stagePath(paths, "restore-"+sanitizeFile(DatabaseFromBackupFile(local))+".bak")
	if err != nil {
		return "", err
	}
	return dst, copyFile(local, dst)
}

func removeStaged(paths pmdir.Paths, serverPath string) { os.Remove(serverPath) }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
