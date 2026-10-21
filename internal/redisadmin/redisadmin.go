// Package redisadmin installs phpRedisAdmin (github.com/erikdubbelboer/
// phpRedisAdmin, CC-BY-3.0) — a PHP web UI for Redis — into
// ~/.mullion/phpredisadmin, served by Mullion's PHP as
// https://redis.<tld>, the way Adminer and phpMyAdmin are.
//
// phpRedisAdmin talks to Redis through Predis (pure PHP, MIT), its one
// real composer dependency, so no PHP extension is needed. The release
// tarballs don't ship vendor/, and Composer isn't needed for it either:
// Mullion downloads the Predis release phpRedisAdmin's composer.json
// pins and writes a vendor/autoload.php that loads it (its other
// requirement, paragonie/random_compat, only polyfills random_bytes()
// for PHP 5).
//
// It does not import app: app links the site (app.EnsureRedisAdmin).
package redisadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pm/internal/archive"
	"pm/internal/pmdir"
)

// SiteName is the site phpRedisAdmin is linked as (redis.<tld>).
const SiteName = "redis"

// Seams for tests: GitHub's API and the tarball host.
var (
	releasesAPI = "https://api.github.com/repos/erikdubbelboer/phpRedisAdmin/releases/latest"
	codeload    = "https://codeload.github.com"
)

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// Dir is where phpRedisAdmin lives.
func Dir(paths pmdir.Paths) string { return paths.RedisAdminDir() }

// Installed reports whether phpRedisAdmin and its Predis are present.
func Installed(paths pmdir.Paths) bool {
	for _, f := range []string{"index.php", "vendor/autoload.php"} {
		if _, err := os.Stat(filepath.Join(Dir(paths), f)); err != nil {
			return false
		}
	}
	return true
}

// Version reads the installed version ("" when unknown).
func Version(paths pmdir.Paths) string {
	b, err := os.ReadFile(filepath.Join(Dir(paths), "VERSION.mullion"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Remove deletes Mullion's phpRedisAdmin. Unlinking is the caller's.
func Remove(paths pmdir.Paths) error { return os.RemoveAll(Dir(paths)) }

// URL is phpRedisAdmin's address; db >= 0 opens that database.
func URL(tld string, db int) string {
	u := "https://" + SiteName + "." + tld + "/"
	if db >= 0 {
		u += "?s=0&d=" + strconv.Itoa(db)
	}
	return u
}

var tagRe = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?$`)

func latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("looking up the latest phpRedisAdmin release: HTTP %s", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("reading the phpRedisAdmin release: %w", err)
	}
	if !tagRe.MatchString(rel.Tag) {
		return "", fmt.Errorf("unexpected phpRedisAdmin release tag %q", rel.Tag)
	}
	return rel.Tag, nil
}

// fetchTree downloads a GitHub tag's source tarball and unpacks it,
// returning the single top-level folder it contains.
func fetchTree(ctx context.Context, repo, tag, work string) (string, error) {
	url := codeload + "/" + repo + "/tar.gz/refs/tags/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s %s: HTTP %s", repo, tag, resp.Status)
	}
	name := strings.ReplaceAll(repo, "/", "-")
	tgz := filepath.Join(work, name+".tar.gz")
	f, err := os.Create(tgz)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", repo, err)
	}
	out := filepath.Join(work, name)
	if err := archive.ExtractTarGz(tgz, out); err != nil {
		return "", fmt.Errorf("unpacking %s: %w", repo, err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(out, e.Name()), nil
		}
	}
	return "", fmt.Errorf("%s %s: empty archive", repo, tag)
}

// PredisTag reads the Predis version phpRedisAdmin's composer.json
// requires ("v2.3.0", "^2.3" -> "v2.3.0" when exact, else the floor
// version "v2.3.0").
func PredisTag(composerJSON []byte) (string, error) {
	var c struct {
		Require map[string]string `json:"require"`
	}
	if err := json.Unmarshal(composerJSON, &c); err != nil {
		return "", fmt.Errorf("reading phpRedisAdmin's composer.json: %w", err)
	}
	spec := c.Require["predis/predis"]
	m := regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`).FindStringSubmatch(spec)
	if m == nil {
		return "", fmt.Errorf("phpRedisAdmin's composer.json has no usable predis/predis version (%q)", spec)
	}
	patch := m[3]
	if patch == "" {
		patch = "0"
	}
	return "v" + m[1] + "." + m[2] + "." + patch, nil
}

// Install downloads the latest phpRedisAdmin and the Predis it needs
// (a no-op when installed), writing its config for the given Redis
// port. Returns the installed version ("" when it already was).
func Install(ctx context.Context, paths pmdir.Paths, port int, password string) (string, error) {
	if Installed(paths) {
		return "", WriteConfig(paths, port, password)
	}
	tag, err := latestTag(ctx)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(paths.TmpDir(), "phpredisadmin-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	tree, err := fetchTree(ctx, "erikdubbelboer/phpRedisAdmin", tag, work)
	if err != nil {
		return "", err
	}
	composer, err := os.ReadFile(filepath.Join(tree, "composer.json"))
	if err != nil {
		return "", fmt.Errorf("phpRedisAdmin %s has no composer.json", tag)
	}
	ptag, err := PredisTag(composer)
	if err != nil {
		return "", err
	}
	predis, err := fetchTree(ctx, "predis/predis", ptag, work)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(predis, "autoload.php")); err != nil {
		return "", fmt.Errorf("predis %s has no autoload.php", ptag)
	}
	vendor := filepath.Join(tree, "vendor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(predis, filepath.Join(vendor, "predis")); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(vendor, "autoload.php"), []byte(autoloadPHP), 0o644); err != nil {
		return "", err
	}
	// Development and packaging leftovers stay out of a served folder.
	for _, junk := range []string{"tests", "Dockerfile", "docker-compose.yml", "composer.lock", ".github", "vendor/predis/tests", "vendor/predis/examples", "vendor/predis/bin"} {
		os.RemoveAll(filepath.Join(tree, junk))
	}
	version := strings.TrimPrefix(tag, "v")
	if err := os.WriteFile(filepath.Join(tree, "VERSION.mullion"), []byte(version+" (predis "+strings.TrimPrefix(ptag, "v")+")\n"), 0o644); err != nil {
		return "", err
	}
	dir := Dir(paths)
	os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tree, dir); err != nil {
		return "", err
	}
	return version, WriteConfig(paths, port, password)
}

// autoloadPHP stands in for Composer's vendor/autoload.php.
const autoloadPHP = `<?php
// Generated by mullion: phpRedisAdmin's dependency (Predis) without Composer.
require __DIR__ . '/predis/autoload.php';
`

// ConfigPHP renders includes/config.inc.php for Mullion's Redis.
// allow_no_auth: the tool is only reachable on this machine (Caddy
// serves redis.<tld> for loopback names, and Redis itself only listens
// on 127.0.0.1), like Mullion's phpMyAdmin and Adminer.
func ConfigPHP(port int, password string) string {
	auth := ""
	if password != "" {
		auth = "\n      'auth'      => " + phpString(password) + ","
	}
	return `<?php
// Generated by mullion — rewritten when Redis's port changes; edits are lost.
$config = array(
  'servers' => array(
    array(
      'name'      => 'Mullion Redis',
      'host'      => '127.0.0.1',
      'port'      => ` + strconv.Itoa(port) + `,
      'scheme'    => 'tcp',
      'filter'    => '*',
      'databases' => 16,
      'flush'     => true,` + auth + `
    ),
  ),
  'seperator' => ':',
  'showEmptyNamespaceAsKey' => false,
  'hideEmptyDBs' => false,
  'allow_no_auth' => true,
  'cookie_auth' => false,
  'maxkeylen' => 100,
  'count_elements_page' => 100,
  'keys' => false,
  'scansize' => 1000,
  'scanmax' => 0,
);
`
}

// phpString single-quotes s for PHP source.
func phpString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// WriteConfig (re)writes includes/config.inc.php — after an install and
// whenever Redis moves to another port.
func WriteConfig(paths pmdir.Paths, port int, password string) error {
	dir := filepath.Join(Dir(paths), "includes")
	if _, err := os.Stat(dir); err != nil {
		return nil // not installed
	}
	return os.WriteFile(filepath.Join(dir, "config.inc.php"), []byte(ConfigPHP(port, password)), 0o644)
}
