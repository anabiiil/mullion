package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pm/internal/app"
	"pm/internal/proc"
	"pm/internal/selfupdate"
)

// upgradedPref is the panel preference the upgrade job leaves for the
// NEW panel: it holds the version just installed, and the page that
// finds it equal to its own version toasts "Mullion is now vX" and
// clears it.
const upgradedPref = "update.justUpgraded"

// registerUpgrade adds the "About & updates" endpoints:
//
//	/api/upgrade/status {force} → selfupdate.Info (latest best-effort, ≤8s)
//	/api/upgrade/start          → streaming job: upgrade, then hand off
//
// The hand-off: this panel process is running the OLD binary, and
// replacing it is the whole point. Once the new binary is on disk the
// job starts `<new> upgrade --finish --panel-pid <this pid>
// --reopen-panel` as a detached process and reports success; the
// helper waits two seconds (so the page reads the finished job), then
// restarts the agent, stops this panel and opens the new one — on
// macOS by quitting and reopening Mullion.app (whose window-host child
// is this process), on Windows by starting `mullion ui`, which replaces
// the old app-mode browser window.
func registerUpgrade(api apiRegistrar) {
	api("/api/upgrade/status", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Force bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		info := selfupdate.Check(ctx, in.Force)
		return struct {
			selfupdate.Info
			Relaunch string `json:"relaunch"`
		}{info, relaunchMode()}, nil
	})
	api("/api/upgrade/start", func(a *app.App, r *http.Request) (any, error) {
		if !jobs.installMu.TryLock() {
			return nil, errors.New("another install is already in progress — wait for it to finish and try again")
		}
		id := jobs.startStream(func(setStatus func(string), appendLog func([]byte)) (any, error) {
			defer jobs.installMu.Unlock()
			res, err := selfupdate.Upgrade(context.Background(), selfupdate.Reporter{
				Line:   func(s string) { appendLog([]byte(s + "\n")) },
				Status: setStatus,
			})
			if err != nil {
				return nil, err
			}
			setStatus("Restarting Mullion on v" + res.To + "…")
			appendLog([]byte("==> Restarting Mullion on v" + res.To + "…\n"))
			setUpgradedPref(res.To)
			if err := startFinisher(a, res.Exe, res.From); err != nil {
				return nil, fmt.Errorf("v%s is installed, but restarting Mullion failed: %v — quit and reopen Mullion", res.To, err)
			}
			return map[string]string{"from": res.From, "to": res.To, "channel": string(res.Channel), "relaunch": relaunchMode()}, nil
		})
		return map[string]string{"job": id}, nil
	})
}

// setUpgradedPref stores the just-installed version for the next panel.
func setUpgradedPref(v string) {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	a, err := app.New()
	if err != nil {
		return
	}
	if a.State.Config.UI == nil {
		a.State.Config.UI = map[string]string{}
	}
	a.State.Config.UI[upgradedPref] = v
	_ = a.State.Save()
}

// startFinisher launches the new binary's `upgrade --finish` detached,
// logging to logs/upgrade.log. It must outlive this process.
func startFinisher(a *app.App, exe, from string) error {
	logFile, err := os.OpenFile(filepath.Join(a.Paths.LogsDir(), "upgrade.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n--- %s upgrade from v%s ---\n", time.Now().Format(time.RFC3339), from)
	c := proc.Quiet(exe, "upgrade", "--finish", "--from", from,
		"--panel-pid", strconv.Itoa(os.Getpid()), "--reopen-panel")
	c.Stdout, c.Stderr = logFile, logFile
	c.Env = append(os.Environ(), "MULLION_NO_REDIRECT=1")
	proc.Detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// relaunchMode tells the page how it comes back after an upgrade:
// "app" (Mullion.app quits and reopens), "window" (Windows: a new
// app-mode window replaces this one) or "tab" (a new browser tab).
func relaunchMode() string {
	switch runtime.GOOS {
	case "windows":
		return "window"
	case "darwin":
		out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(os.Getppid())).Output()
		if err == nil && strings.HasSuffix(strings.TrimSpace(string(out)), ".app/Contents/MacOS/Mullion") {
			return "app"
		}
	}
	return "tab"
}
