//go:build !windows && !darwin

package mysql

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

var mysqlArch = map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]

// versionRe finds downloadable releases on dev.mysql.com pages —
// generic Linux builds are named mysql-<v>-linux-glibc<X>-<arch>.tar.xz;
// the older minimal tarballs use .tar.gz, which is what we can extract.
var versionRe = regexp.MustCompile(`mysql-(\d+)\.(\d+)\.(\d+)-linux-glibc[\d.]+-` + mysqlArch + `\.tar\.gz`)

// downloadPageQuery selects Linux Generic on dev.mysql.com's pages.
const downloadPageQuery = "?tpl=platform&os=2"

func flavorSupported(version string) error {
	if IsMaria(version) {
		return fmt.Errorf("MariaDB installs are not supported on this platform yet")
	}
	return nil
}

func downloadURLs(version string) []string {
	series := version[:strings.LastIndex(version, ".")]
	var urls []string
	for _, glibc := range []string{"2.28", "2.17", "2.12"} {
		name := fmt.Sprintf("mysql-%s-linux-glibc%s-%s.tar.gz", version, glibc, mysqlArch)
		urls = append(urls,
			fmt.Sprintf("https://dev.mysql.com/get/Downloads/MySQL-%s/%s", series, name),
			fmt.Sprintf("https://downloads.mysql.com/archives/get/p/23/file/%s", name))
	}
	return urls
}
