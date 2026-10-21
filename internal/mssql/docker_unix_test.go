//go:build !windows

package mssql

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

// fakeDocker is a shell script standing in for the docker CLI: it logs
// every invocation (and whether the password reached its environment)
// and keeps a tiny container state in files.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$1" in
  info) echo 27.0.0 ;;
  container)
    if [ ! -f "$FAKE_STATE/exists" ]; then echo "Error: No such container" >&2; exit 1; fi
    case "$*" in
      *State.Running*) cat "$FAKE_STATE/running" ;;
      *) echo cid ;;
    esac ;;
  run) touch "$FAKE_STATE/exists"; echo true > "$FAKE_STATE/running"; echo "pw=$MSSQL_SA_PASSWORD" >> "$FAKE_LOG"; echo cid ;;
  start) echo true > "$FAKE_STATE/running" ;;
  stop) echo false > "$FAKE_STATE/running" ;;
  rm) rm -f "$FAKE_STATE/exists" ;;
  port) echo "127.0.0.1:1433" ;;
  exec) cat > "$FAKE_STATE/stdin"; echo "sqlpw=$SQLCMDPASSWORD" >> "$FAKE_LOG"; echo 1 ;;
  *) ;;
esac
`

// setupFakeDocker points the package at the fake and at a sandbox home
// with a listening docker socket (so daemonSocketUp is true).
func setupFakeDocker(t *testing.T) (logFile string, paths pmdir.Paths) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	os.MkdirAll(state, 0o755)
	logFile = filepath.Join(dir, "log")
	t.Setenv("FAKE_LOG", logFile)
	t.Setenv("FAKE_STATE", state)
	t.Setenv("DOCKER_HOST", "")

	// A short home: unix socket paths are limited to ~104 bytes.
	home, err := os.MkdirTemp("", "msq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	sockDir := filepath.Join(home, ".orbstack", "run")
	os.MkdirAll(sockDir, 0o755)
	ln, err := net.Listen("unix", filepath.Join(sockDir, "docker.sock"))
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	oldOverride, oldHome, oldPw, oldPort := dockerOverride, homeDir, Password, Port
	dockerOverride = bin
	homeDir = func() string { return home }
	t.Cleanup(func() { dockerOverride, homeDir, Password, Port = oldOverride, oldHome, oldPw, oldPort })
	return logFile, pmdir.Paths{Home: filepath.Join(dir, "mullion")}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestContainerLifecycleWithFakeDocker(t *testing.T) {
	logFile, paths := setupFakeDocker(t)
	Port = 14330

	if err := Install(context.Background(), paths, "Secret_123x", nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	log := readLog(t, logFile)
	for _, want := range []string{
		"info --format",
		"pull --platform linux/amd64 " + Image,
		"volume create " + VolumeName,
		"run -d --name " + ContainerName,
		"-p 127.0.0.1:14330:1433",
		"pw=Secret_123x",    // the password reached docker through its environment
		"sqlpw=Secret_123x", // ...and sqlcmd's
	} {
		if !strings.Contains(log, want) {
			t.Errorf("docker log misses %q:\n%s", want, log)
		}
	}
	if strings.Contains(strings.Split(log, "pw=")[0], "Secret_123x") {
		t.Errorf("password appeared in docker arguments:\n%s", log)
	}

	if err := Stop(paths); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(readLog(t, logFile), "stop -t 30 "+ContainerName) {
		t.Error("Stop did not stop the container")
	}
	if err := Ensure(paths); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !strings.Contains(readLog(t, logFile), "start "+ContainerName) {
		t.Error("Ensure did not start the stopped container")
	}

	// A port change recreates the container (the volume stays).
	Port = 14331
	if err := ApplyPort(paths); err != nil {
		t.Fatalf("ApplyPort: %v", err)
	}
	log = readLog(t, logFile)
	if !strings.Contains(log, "rm -f "+ContainerName) || !strings.Contains(log, "-p 127.0.0.1:14331:1433") {
		t.Errorf("ApplyPort did not recreate on the new port:\n%s", log)
	}
	if strings.Contains(log, "volume rm") {
		t.Error("ApplyPort must keep the data volume")
	}

	dbs, err := UserDatabases(paths)
	if err != nil || len(dbs) != 1 || dbs[0] != "1" {
		t.Errorf("UserDatabases via fake sqlcmd = %v, %v", dbs, err)
	}
	stdin, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_STATE"), "stdin"))
	if !strings.Contains(string(stdin), "sys.databases") {
		t.Errorf("SQL must be fed on stdin, got %q", stdin)
	}

	if err := Uninstall(context.Background(), paths, false); err != nil {
		t.Fatalf("Uninstall keep data: %v", err)
	}
	if strings.Contains(readLog(t, logFile), "volume rm") {
		t.Error("keep-data uninstall removed the volume")
	}
	if err := Uninstall(context.Background(), paths, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !strings.Contains(readLog(t, logFile), "volume rm -f "+VolumeName) {
		t.Error("delete-data uninstall kept the volume")
	}
}

func TestNoDockerMeansOrbStackNeeded(t *testing.T) {
	old, oldHome := dockerOverride, homeDir
	t.Cleanup(func() { dockerOverride, homeDir = old, oldHome })
	dockerOverride = ""
	empty := t.TempDir()
	homeDir = func() string { return empty }
	t.Setenv("PATH", empty)
	if dockerPath() != "" && !fileExists("/usr/local/bin/docker") && !fileExists("/opt/homebrew/bin/docker") {
		t.Fatal("unexpected docker found")
	}
	if dockerPath() != "" {
		t.Skip("a docker CLI is installed system-wide on this machine")
	}
	if err := Install(context.Background(), pmdir.Paths{Home: empty}, "Secret_123x", nil); err != ErrNoDocker {
		t.Errorf("Install without docker = %v, want ErrNoDocker", err)
	}
	if err := Stop(pmdir.Paths{Home: empty}); err != nil {
		t.Errorf("Stop without docker = %v, want nil", err)
	}
}

func TestStopFastIsBounded(t *testing.T) {
	logFile, paths := setupFakeDocker(t)
	os.WriteFile(filepath.Join(os.Getenv("FAKE_STATE"), "exists"), nil, 0o644)
	os.WriteFile(filepath.Join(os.Getenv("FAKE_STATE"), "running"), []byte("true\n"), 0o644)
	if err := StopFast(paths); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readLog(t, logFile), "stop -t 10 "+ContainerName) {
		t.Errorf("StopFast should use a short grace:\n%s", readLog(t, logFile))
	}
	// Stopped already: no stop command.
	os.Remove(logFile)
	if err := StopFast(paths); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readLog(t, logFile), "stop -t") {
		t.Error("StopFast stopped a stopped container")
	}
}
