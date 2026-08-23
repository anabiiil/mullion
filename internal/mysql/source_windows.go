//go:build windows

package mysql

import (
	"fmt"
	"regexp"
	"strings"
)

// versionRe finds downloadable releases on dev.mysql.com pages.
var versionRe = regexp.MustCompile(`mysql-(\d+)\.(\d+)\.(\d+)-winx64\.zip`)

// downloadPageQuery selects the platform on dev.mysql.com's pages
// (the default page already shows Windows builds).
const downloadPageQuery = ""

// flavorSupported reports whether this platform has binaries for the
// requested flavor (Windows has both MySQL and MariaDB).
func flavorSupported(version string) error { return nil }

// downloadURLs returns the download candidates for a version: the
// current release CDN first, then the archive (where superseded
// releases move). MariaDB's archive hosts every release at a stable path.
func downloadURLs(version string) []string {
	if IsMaria(version) {
		v := strings.TrimPrefix(version, mariaPrefix)
		return []string{
			fmt.Sprintf("https://archive.mariadb.org/mariadb-%s/winx64-packages/mariadb-%s-winx64.zip", v, v),
		}
	}
	series := version[:strings.LastIndex(version, ".")]
	name := fmt.Sprintf("mysql-%s-winx64.zip", version)
	return []string{
		fmt.Sprintf("https://dev.mysql.com/get/Downloads/MySQL-%s/%s", series, name),
		fmt.Sprintf("https://downloads.mysql.com/archives/get/p/23/file/%s", name),
	}
}
