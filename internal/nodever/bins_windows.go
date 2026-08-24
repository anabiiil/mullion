//go:build windows

package nodever

import "path/filepath"

// NodeBin is the node executable inside a version directory (Windows
// zips put everything at the top level, no bin/).
func NodeBin(versionDir string) string { return filepath.Join(versionDir, "node.exe") }

// BinDir is the directory to put on the PATH for a version.
func BinDir(versionDir string) string { return versionDir }

// Tool resolves a bundled tool (npm, npx, corepack) inside a version dir.
func Tool(versionDir, name string) string {
	if name == "node" {
		return NodeBin(versionDir)
	}
	return filepath.Join(versionDir, name+".cmd")
}
