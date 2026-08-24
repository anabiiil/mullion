//go:build !windows

package nodever

import "path/filepath"

// NodeBin is the node executable inside a version directory.
func NodeBin(versionDir string) string { return filepath.Join(versionDir, "bin", "node") }

// BinDir is the directory to put on the PATH for a version.
func BinDir(versionDir string) string { return filepath.Join(versionDir, "bin") }

// Tool resolves a bundled tool (npm, npx, corepack) inside a version dir.
func Tool(versionDir, name string) string {
	return filepath.Join(versionDir, "bin", name)
}
