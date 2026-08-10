package phpver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
)

// Extensions enabled by default in every installed version's php.ini.
var defaultExtensions = []string{
	"curl", "fileinfo", "gd", "intl", "mbstring", "exif", "mysqli",
	"openssl", "pdo_mysql", "pdo_pgsql", "pdo_sqlite", "sqlite3",
	"sodium", "zip",
}

// Install downloads and unpacks a release into ~/.mullion/php/<version> and
// writes a working php.ini. It is a no-op if the version is already installed.
func Install(ctx context.Context, paths pmdir.Paths, rel Release) (string, error) {
	destDir := paths.PhpVersionDir(rel.Version)
	if _, err := os.Stat(filepath.Join(destDir, PhpCgiName)); err == nil {
		return destDir, nil
	}

	zipPath := filepath.Join(paths.TmpDir(), filepath.Base(rel.ZipURL))
	fmt.Printf("Downloading PHP %s...\n", rel.Version)
	if err := download.ToFile(ctx, rel.ZipURL, zipPath); err != nil {
		return "", err
	}
	defer os.Remove(zipPath)

	if err := archive.ExtractZip(zipPath, destDir); err != nil {
		os.RemoveAll(destDir)
		return "", fmt.Errorf("extracting %s: %w", zipPath, err)
	}
	if err := writeIni(destDir); err != nil {
		return "", fmt.Errorf("writing php.ini: %w", err)
	}
	return destDir, nil
}

// Installed lists the full versions present under ~/.mullion/php, newest first.
func Installed(paths pmdir.Paths) ([]string, error) {
	entries, err := os.ReadDir(paths.PhpDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "current" {
			continue
		}
		if _, err := ParseSelector(e.Name()); err == nil {
			out = append(out, e.Name())
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// FindInstalled returns the newest installed version matching the selector.
func FindInstalled(paths pmdir.Paths, sel Selector) (string, error) {
	versions, err := Installed(paths)
	if err != nil {
		return "", err
	}
	best := ""
	for _, v := range versions {
		if sel.Matches(v) && (best == "" || Compare(v, best) > 0) {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("PHP %s is not installed (run: mullion php install %s)", sel, sel)
	}
	return best, nil
}

// writeIni starts from php.ini-development and appends our overrides, so
// users can still edit the file afterwards.
func writeIni(phpDir string) error {
	iniPath := filepath.Join(phpDir, "php.ini")
	if _, err := os.Stat(iniPath); err == nil {
		return nil
	}
	base, err := os.ReadFile(filepath.Join(phpDir, "php.ini-development"))
	if err != nil {
		return err
	}

	// Only enable extensions whose DLL the build actually ships — e.g.
	// PHP 8.5 dropped php_opcache.dll (opcache is built in there), and
	// loading a missing DLL makes every php invocation print a warning.
	hasDLL := func(name string) bool {
		_, err := os.Stat(filepath.Join(phpDir, "ext", "php_"+name+".dll"))
		return err == nil
	}

	var b strings.Builder
	b.Write(base)
	b.WriteString("\n\n; ---- managed by mullion ----\n")
	b.WriteString("extension_dir = \"ext\"\n")
	for _, ext := range defaultExtensions {
		if hasDLL(ext) {
			b.WriteString("extension=" + ext + "\n")
		}
	}
	if hasDLL("opcache") {
		b.WriteString("zend_extension=opcache\n")
	}
	b.WriteString("opcache.enable=1\n")
	b.WriteString("cgi.fix_pathinfo=1\n")
	b.WriteString("cgi.force_redirect=0\n")
	b.WriteString("memory_limit=512M\n")
	b.WriteString("upload_max_filesize=100M\n")
	b.WriteString("post_max_size=100M\n")
	b.WriteString("max_execution_time=120\n")

	// Point curl/openssl at a CA bundle if the build ships one.
	if _, err := os.Stat(filepath.Join(phpDir, "extras", "ssl", "cacert.pem")); err == nil {
		caPath := filepath.Join(phpDir, "extras", "ssl", "cacert.pem")
		b.WriteString("curl.cainfo=\"" + caPath + "\"\n")
		b.WriteString("openssl.cafile=\"" + caPath + "\"\n")
	}
	return os.WriteFile(iniPath, []byte(b.String()), 0o644)
}
