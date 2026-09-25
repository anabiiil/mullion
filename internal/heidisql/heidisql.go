// Package heidisql installs the portable HeidiSQL desktop client into
// ~/.mullion/heidisql, preconfigured with a session for the local
// MySQL server.
package heidisql

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
)

var versionRe = regexp.MustCompile(`HeidiSQL_([\d.]+)_64_Portable\.zip`)

func Dir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "heidisql") }
func Exe(paths pmdir.Paths) string { return filepath.Join(Dir(paths), "heidisql.exe") }

// Installed reports whether HeidiSQL is present.
func Installed(paths pmdir.Paths) bool {
	_, err := os.Stat(Exe(paths))
	return err == nil
}

// Install downloads the latest portable HeidiSQL. It is a no-op when
// already installed.
func Install(ctx context.Context, paths pmdir.Paths) error {
	if Installed(paths) {
		return writeSession(paths)
	}

	version, err := latestVersion(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/HeidiSQL/HeidiSQL/releases/download/v%s/HeidiSQL_%s_64_Portable.zip",
		version, version)
	zipPath := filepath.Join(paths.TmpDir(), "heidisql.zip")
	fmt.Printf("Downloading HeidiSQL %s...\n", version)
	if err := download.ToFile(ctx, url, zipPath); err != nil {
		return err
	}
	defer os.Remove(zipPath)

	// The portable zip has heidisql.exe at its root.
	if err := archive.ExtractZip(zipPath, Dir(paths)); err != nil {
		os.RemoveAll(Dir(paths))
		return fmt.Errorf("extracting %s: %w", zipPath, err)
	}
	if !Installed(paths) {
		return fmt.Errorf("unexpected archive layout: no heidisql.exe in %s", Dir(paths))
	}
	return writeSession(paths)
}

// Launch opens HeidiSQL. No detach attributes: proc.Detach sets
// HideWindow, which is right for background servers but makes a GUI app
// start with its window INVISIBLE. GUI processes survive the CLI
// exiting on their own — they are not attached to our console.
func Launch(paths pmdir.Paths) error {
	if !Installed(paths) {
		return fmt.Errorf("HeidiSQL is not installed (run: mullion heidisql)")
	}
	cmd := exec.Command(Exe(paths))
	cmd.Dir = Dir(paths)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// latestVersion scrapes heidisql.com's download page (the binaries
// themselves are served from GitHub releases).
func latestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.heidisql.com/download.php", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET heidisql download page: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	m := versionRe.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("could not find a portable HeidiSQL release on the download page")
	}
	return string(m[1]), nil
}

// writeSession seeds the portable settings with a "Mullion" session for
// the local server, so the first launch connects with one click. Only
// written when no settings exist yet — HeidiSQL owns the file afterwards.
//
// Line format is <key>\t<datatype>\t<value> where the datatype is the
// ordinal of HeidiSQL's TAppSettingDataType: 0=Int, 1=Bool, 2=String.
// Wrong ordinals crash HeidiSQL when it loads the session, so keep this
// to the minimum set of keys.
func writeSession(paths pmdir.Paths) error {
	settings := filepath.Join(Dir(paths), "portable_settings.txt")
	if _, err := os.Stat(settings); err == nil {
		return nil
	}
	content := "Servers\\Mullion\\Host\t2\t127.0.0.1\r\n" +
		"Servers\\Mullion\\User\t2\troot\r\n" +
		"Servers\\Mullion\\Port\t0\t3306\r\n"
	return os.WriteFile(settings, []byte(content), 0o644)
}
