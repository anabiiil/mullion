// Package detect guesses what a linked site's project is — language,
// framework, and package manager — from a handful of well-known files
// at its root, for the control panel's Sites page. Detection is fast on
// purpose: only stat/read a few small files, never a directory walk.
package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Info describes what Project found at a directory.
type Info struct {
	Language string `json:"language"`
	// Framework is the human-readable name ("Laravel", "Next.js", ...),
	// empty when none was recognized.
	Framework string `json:"framework,omitempty"`
	// FrameworkVersion is a major version like "11", best-effort.
	FrameworkVersion string `json:"frameworkVersion,omitempty"`
	// Icon is a short, stable key the UI maps to an icon: laravel,
	// symfony, wordpress, drupal, codeigniter, yii, cakephp, slim, php,
	// nuxt, next, vue, react, svelte, sveltekit, astro, angular, vite,
	// remix, gatsby, solid, qwik, express, nest, node, static, unknown.
	Icon string `json:"icon"`
	// PackageManager is npm, pnpm, yarn, or bun — from the lockfile
	// present, empty when none is.
	PackageManager string `json:"packageManager,omitempty"`
}

type cacheEntry struct {
	composerMTime int64
	packageMTime  int64
	info          Info
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// Project detects the project rooted at dir. Results are cached for the
// process lifetime, invalidated when composer.json or package.json's
// mtime changes.
func Project(dir string) Info {
	composerMTime := statMTime(filepath.Join(dir, "composer.json"))
	packageMTime := statMTime(filepath.Join(dir, "package.json"))

	cacheMu.Lock()
	if e, ok := cache[dir]; ok && e.composerMTime == composerMTime && e.packageMTime == packageMTime {
		cacheMu.Unlock()
		return e.info
	}
	cacheMu.Unlock()

	info := detect(dir)

	cacheMu.Lock()
	cache[dir] = cacheEntry{composerMTime: composerMTime, packageMTime: packageMTime, info: info}
	cacheMu.Unlock()
	return info
}

func statMTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func detect(dir string) Info {
	if info, ok := detectPHP(dir); ok {
		return info
	}
	if info, ok := detectNode(dir); ok {
		return info
	}
	if fileExists(filepath.Join(dir, "index.html")) {
		return Info{Language: "HTML", Icon: "static"}
	}
	return Info{Language: "Unknown", Icon: "unknown"}
}

type composerJSON struct {
	Require map[string]string `json:"require"`
}

type composerLock struct {
	Packages []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"packages"`
}

// phpFrameworks is checked in order against composer.json's require
// keys; the first match wins.
var phpFrameworks = []struct {
	pkg, name, icon string
}{
	{"laravel/framework", "Laravel", "laravel"},
	{"symfony/framework-bundle", "Symfony", "symfony"},
	{"drupal/core", "Drupal", "drupal"},
	{"codeigniter4/framework", "CodeIgniter", "codeigniter"},
	{"yiisoft/yii2", "Yii", "yii"},
	{"cakephp/cakephp", "CakePHP", "cakephp"},
	{"slim/slim", "Slim", "slim"},
}

var leadingDigits = regexp.MustCompile(`\d+`)

// majorFromConstraint extracts a leading major version number from a
// composer/npm version constraint or a plain semver string, e.g.
// "^11.31|^10.10" -> "11", "v11.31.0" -> "11".
func majorFromConstraint(s string) string {
	return leadingDigits.FindString(s)
}

func isWordPress(dir string) bool {
	return fileExists(filepath.Join(dir, "wp-config.php")) || dirExists(filepath.Join(dir, "wp-content"))
}

func detectPHP(dir string) (Info, bool) {
	composerPath := filepath.Join(dir, "composer.json")
	data, err := os.ReadFile(composerPath)
	hasComposer := err == nil

	var require map[string]string
	if hasComposer {
		var doc composerJSON
		if jsonErr := json.Unmarshal(data, &doc); jsonErr == nil {
			require = doc.Require
		}
	}

	for _, fw := range phpFrameworks {
		constraint, ok := require[fw.pkg]
		if !ok {
			continue
		}
		version := majorFromConstraint(constraint)
		if fw.pkg == "laravel/framework" {
			if lockVersion := lockedVersion(dir, fw.pkg); lockVersion != "" {
				version = lockVersion
			}
		}
		return Info{Language: "PHP", Framework: fw.name, FrameworkVersion: version, Icon: fw.icon}, true
	}

	if isWordPress(dir) {
		return Info{Language: "PHP", Framework: "WordPress", Icon: "wordpress"}, true
	}

	if fileExists(filepath.Join(dir, "artisan")) {
		version := lockedVersion(dir, "laravel/framework")
		return Info{Language: "PHP", Framework: "Laravel", FrameworkVersion: version, Icon: "laravel"}, true
	}

	if hasComposer {
		return Info{Language: "PHP", Icon: "php"}, true
	}
	return Info{}, false
}

// lockedVersion reads composer.lock (when present) for the installed
// version of pkg, cleaned to a major version.
func lockedVersion(dir, pkg string) string {
	data, err := os.ReadFile(filepath.Join(dir, "composer.lock"))
	if err != nil {
		return ""
	}
	var lock composerLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return ""
	}
	for _, p := range lock.Packages {
		if p.Name == pkg {
			return majorFromConstraint(p.Version)
		}
	}
	return ""
}
