package frankenphp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"pm/internal/archive"
	"pm/internal/pmdir"
)

// Install downloads rel into <FrankenDir>/<tag>, checking the asset's
// published SHA-256 while it streams. Everything happens in
// <FrankenDir>/.<tag>.tmp and is renamed into place only once verified,
// so an interrupted or corrupt download never looks installed.
//
// The Windows zip has frankenphp.exe at its root next to the PHP DLLs
// and ext/ folder, so it is extracted as is. The macOS asset is the bare
// binary.
func Install(ctx context.Context, paths pmdir.Paths, rel Release, progress func(string)) (err error) {
	if !validTag(rel.Tag) {
		return fmt.Errorf("unexpected FrankenPHP release tag %q", rel.Tag)
	}
	want, ok := strings.CutPrefix(rel.Asset.Digest, "sha256:")
	if !ok || want == "" {
		return fmt.Errorf("FrankenPHP %s has no published SHA-256, so the download cannot be verified", rel.Tag)
	}
	if progress == nil {
		progress = func(string) {}
	}

	tmp := filepath.Join(paths.FrankenDir(), "."+rel.Tag+".tmp")
	final := filepath.Join(paths.FrankenDir(), rel.Tag)
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()

	if rel.Asset.Size > 0 {
		progress(fmt.Sprintf("Downloading FrankenPHP %s (%s)…", rel.Tag, humanBytes(rel.Asset.Size)))
	} else {
		progress(fmt.Sprintf("Downloading FrankenPHP %s…", rel.Tag))
	}

	isZip := strings.HasSuffix(strings.ToLower(rel.Asset.Name), ".zip")
	dest := filepath.Join(tmp, pmdir.ExeName("frankenphp"))
	if isZip {
		dest = filepath.Join(tmp, "download.zip")
	}
	if err := download(ctx, rel.Asset.URL, dest, want); err != nil {
		return err
	}
	if isZip {
		progress("Unpacking FrankenPHP " + rel.Tag + "…")
		if err := archive.ExtractZip(dest, tmp); err != nil {
			return fmt.Errorf("unpacking FrankenPHP %s: %w", rel.Tag, err)
		}
		if err := os.Remove(dest); err != nil {
			return err
		}
	}
	bin := filepath.Join(tmp, pmdir.ExeName("frankenphp"))
	if st, serr := os.Stat(bin); serr != nil || st.IsDir() {
		return fmt.Errorf("the FrankenPHP %s download does not contain %s", rel.Tag, pmdir.ExeName("frankenphp"))
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return err
	}

	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// download streams url into dest and checks its SHA-256 (hex).
func download(ctx context.Context, url, dest, wantSum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading FrankenPHP: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading FrankenPHP: HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading FrankenPHP: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSum) {
		return fmt.Errorf("checksum mismatch for the FrankenPHP download (got %s, want %s) — the download is corrupt or was tampered with", got, wantSum)
	}
	return nil
}

// Installed lists the installed release tags, newest first.
func Installed(paths pmdir.Paths) []string {
	entries, err := os.ReadDir(paths.FrankenDir())
	if err != nil {
		return nil
	}
	var tags []string
	for _, e := range entries {
		if !e.IsDir() || !validTag(e.Name()) {
			continue
		}
		if st, err := os.Stat(Binary(paths, e.Name())); err != nil || st.IsDir() {
			continue
		}
		tags = append(tags, e.Name())
	}
	sort.Slice(tags, func(i, j int) bool {
		a, _ := parseTag(tags[i])
		b, _ := parseTag(tags[j])
		for k := range a {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return false
	})
	return tags
}

// Binary is the path of the frankenphp executable of an installed tag.
func Binary(paths pmdir.Paths, tag string) string {
	return filepath.Join(paths.FrankenDir(), tag, pmdir.ExeName("frankenphp"))
}

// phpVersionRe finds "PHP 8.6.0" in `frankenphp -v`'s line:
//
//	FrankenPHP v1.14.0 PHP 8.6.0 Caddy v2.11.9 h1:…
//
// The leading \b keeps "FrankenPHP" from matching.
var phpVersionRe = regexp.MustCompile(`\bPHP (\d+\.\d+\.\d+)`)

// PHPVersion asks a FrankenPHP binary which PHP it embeds. PHPRC and
// PHP_INI_SCAN_DIR are dropped: the binary honors them, and Mullion's
// own (set for the shared PHP) would make it load extensions built for
// another PHP.
func PHPVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-v")
	cmd.Env = withoutPHPIni(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s -v: %w", bin, err)
	}
	m := phpVersionRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no PHP version in the output of %s -v: %q", bin, strings.TrimSpace(string(out)))
	}
	return string(m[1]), nil
}

func withoutPHPIni(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PHPRC") || strings.EqualFold(k, "PHP_INI_SCAN_DIR") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// humanBytes matches the MB/GB formatting used elsewhere in Mullion's
// progress messages (internal/app.HumanBytes; not imported to keep this
// package free of that dependency).
func humanBytes(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}
