//go:build windows

package mongodb

import "fmt"

// indexTarget/indexArch select this platform's builds in MongoDB's
// release index. MongoDB ships x86_64 only; Windows on ARM runs it
// under emulation.
const (
	indexTarget = "windows"
	indexArch   = "x86_64"
)

// mongoshDistro is this platform's "distro" in mongosh's release index.
const mongoshDistro = "win32"

// serverEntries are the only files taken from the server zip: it is
// ~0.8 GB, nearly all of it .pdb debug symbols, mongos and a bundled
// vc_redist — mongod.exe alone is ~30 MB compressed.
var serverEntries = []string{"mongod.exe"}

// runHint is appended when an installed binary fails to execute.
const runHint = " (mongod needs the Microsoft Visual C++ 2015-2022 x64 runtime — `mullion setup` installs it)"

func platformSupported() error { return nil }

// serverFallbackURL is the official archive URL, used when the release
// index can't be reached.
func serverFallbackURL(version string) string {
	return fmt.Sprintf("https://fastdl.mongodb.org/windows/mongodb-windows-x86_64-%s.zip", version)
}

func mongoshURL(version string) string {
	return fmt.Sprintf("https://downloads.mongodb.com/compass/mongosh-%s-win32-x64.zip", version)
}

// prepareBinaries has nothing to do on Windows.
func prepareBinaries(dir string) {}
