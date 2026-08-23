//go:build darwin

package mysql

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// mysqlArch is the CPU architecture part of Oracle's macOS tarball names.
var mysqlArch = map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]

// versionRe finds downloadable releases on dev.mysql.com pages —
// macOS tarballs are named mysql-<v>-macos<NN>-<arch>.tar.gz.
var versionRe = regexp.MustCompile(`mysql-(\d+)\.(\d+)\.(\d+)-macos\d+-` + mysqlArch + `\.tar\.gz`)

// downloadPageQuery selects macOS on dev.mysql.com's download pages.
const downloadPageQuery = "?tpl=platform&os=33"

// flavorSupported: MariaDB publishes no official macOS binaries.
func flavorSupported(version string) error {
	if IsMaria(version) {
		return fmt.Errorf("MariaDB has no official macOS binaries — use MySQL instead (mullion mysql install)")
	}
	return nil
}

// downloadURLs returns the download candidates for a version. The macOS
// part of the file name (macos15, macos14, ...) depends on which SDK the
// release was built against and is not derivable from the version alone,
// so several candidates are tried; dev.mysql.com/get transparently
// redirects to the archives for superseded releases, and misses fail
// fast with a 404.
func downloadURLs(version string) []string {
	series := version[:strings.LastIndex(version, ".")]
	var urls []string
	for _, nn := range []string{"26", "15", "14", "13", "12", "11"} {
		name := fmt.Sprintf("mysql-%s-macos%s-%s.tar.gz", version, nn, mysqlArch)
		urls = append(urls,
			fmt.Sprintf("https://dev.mysql.com/get/Downloads/MySQL-%s/%s", series, name))
	}
	for _, nn := range []string{"26", "15", "14", "13", "12", "11"} {
		name := fmt.Sprintf("mysql-%s-macos%s-%s.tar.gz", version, nn, mysqlArch)
		urls = append(urls,
			fmt.Sprintf("https://downloads.mysql.com/archives/get/p/23/file/%s", name))
	}
	return urls
}
