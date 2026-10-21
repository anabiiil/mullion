package mssql

// Pure helpers for the container backend (macOS). They live in an
// untagged file so their tests run on every platform; the code that
// actually runs docker is in docker_unix.go.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// containerSqlcmd is the sqlcmd shipped inside the image: since SQL
// Server 2022 CU14 the images carry mssql-tools18 under
// /opt/mssql-tools18/bin (source: the Docker quickstart above).
const containerSqlcmd = "/opt/mssql-tools18/bin/sqlcmd"

// containerStageDir is where BACKUP/RESTORE files live inside the
// container — under the data volume, owned by the mssql user.
const containerStageDir = "/var/opt/mssql/backup"

// managedLabel marks the container as Mullion's.
const managedLabel = "dev.mullion.managed=mssql"

// findDocker locates the docker CLI: PATH first, then where OrbStack
// puts it (~/.orbstack/bin, symlinked into /usr/local/bin), then
// Homebrew. "" when none exists. lookPath and exists are seams for tests.
func findDocker(lookPath func(string) (string, error), exists func(string) bool, home string) string {
	if p, err := lookPath("docker"); err == nil && p != "" {
		return p
	}
	for _, p := range []string{
		filepath.Join(home, ".orbstack", "bin", "docker"),
		"/usr/local/bin/docker",
		"/opt/homebrew/bin/docker",
	} {
		if exists(p) {
			return p
		}
	}
	return ""
}

// dockerEnv is the environment docker commands run with: when DOCKER_HOST
// is unset and OrbStack's socket exists, point the CLI at it explicitly,
// so a stale default context (say, a Docker Desktop that isn't running)
// can't send Mullion's commands elsewhere. password is exported as
// MSSQL_SA_PASSWORD / SQLCMDPASSWORD for `-e NAME` flags that read it
// from this environment — it never appears on a command line.
func dockerEnv(base []string, home, password string, exists func(string) bool) []string {
	env := make([]string, 0, len(base)+3)
	hasHost := false
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "MSSQL_SA_PASSWORD", "SQLCMDPASSWORD":
			continue
		case "DOCKER_HOST":
			hasHost = true
		}
		env = append(env, kv)
	}
	if !hasHost {
		if sock := filepath.Join(home, ".orbstack", "run", "docker.sock"); exists(sock) {
			env = append(env, "DOCKER_HOST=unix://"+sock)
		}
	}
	if password != "" {
		env = append(env, "MSSQL_SA_PASSWORD="+password, "SQLCMDPASSWORD="+password)
	}
	return env
}

// runArgs is `docker run` for Mullion's SQL Server container:
//   - linux/amd64: the image is x86-64 only; on Apple Silicon OrbStack
//     runs it through Rosetta.
//   - published on 127.0.0.1 only, never the LAN.
//   - --restart no: Mullion starts and stops it (an explicit stop must
//     stick, like every other engine).
//   - `-e MSSQL_SA_PASSWORD` without a value: docker copies it from its
//     own environment (dockerEnv), keeping the password out of `ps`.
//
// Encryption is left at the server default (not forced): macOS's PHP
// driver, pdo_dblib over FreeTDS, is built without TLS, so
// MSSQL/mssql-conf network.forceencryption must never be set.
//
// Environment variables per
// https://learn.microsoft.com/sql/linux/sql-server-linux-configure-environment-variables
func runArgs(port int, image string) []string {
	return []string{
		"run", "-d",
		"--name", ContainerName,
		"--hostname", ContainerName,
		"--platform", "linux/amd64",
		"--restart", "no",
		"--label", managedLabel,
		"-e", "ACCEPT_EULA=Y",
		"-e", "MSSQL_PID=" + ContainerEdition,
		"-e", "MSSQL_SA_PASSWORD",
		"-p", "127.0.0.1:" + strconv.Itoa(port) + ":1433",
		"-v", VolumeName + ":/var/opt/mssql",
		image,
	}
}

// sqlcmdArgs is `docker exec` running sqlcmd inside the container, the
// script read from stdin (-i /dev/stdin):
//   - -C trusts the server's self-signed certificate (ODBC 18 encrypts
//     by default), -b fails on errors, -h -1 drops headers, -W trims
//     padding, -s TAB separates columns, -r 1 sends errors to stderr.
//   - SQLCMDPASSWORD comes from docker's environment via `-e NAME`.
//
// Flags per https://learn.microsoft.com/sql/tools/sqlcmd/sqlcmd-utility
func sqlcmdArgs() []string {
	return []string{
		"exec", "-i", "-e", "SQLCMDPASSWORD", ContainerName,
		containerSqlcmd, "-S", "localhost", "-U", Login, "-C", "-b",
		"-h", "-1", "-W", "-s", "\t", "-r", "1", "-i", "/dev/stdin",
	}
}

// publishedPort extracts the host port from `docker port <c> 1433/tcp`
// output ("127.0.0.1:1433"), 0 when absent.
func publishedPort(out string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, ":"); i >= 0 {
			if p, err := strconv.Atoi(line[i+1:]); err == nil {
				return p
			}
		}
	}
	return 0
}

// ── OrbStack ──────────────────────────────────────────────────────

// orbStackApps are where OrbStack.app may be installed.
func orbStackApps(home string) []string {
	return []string{"/Applications/OrbStack.app", filepath.Join(home, "Applications", "OrbStack.app")}
}

// OrbStackDownloadURL is OrbStack's official "latest stable" download
// for a Go architecture; it redirects to the versioned .dmg on
// cdn-updates.orbstack.dev (https://orbstack.dev/download).
func OrbStackDownloadURL(goarch string) string {
	arch := "arm64"
	if goarch == "amd64" {
		arch = "amd64"
	}
	return "https://orbstack.dev/download/stable/latest/" + arch
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
