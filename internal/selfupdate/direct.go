package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// pickAsset chooses the release file for goos/goarch, matching the names
// .github/workflows/release.yml publishes: mullion.exe (and
// mullion-<tag>-windows-amd64.zip) for Windows — arm64 Windows runs the
// amd64 build — and mullion-<tag>-darwin-<arch>.tar.gz for macOS.
func pickAsset(assets []Asset, goos, goarch string) (Asset, bool) {
	var want []func(string) bool
	switch goos {
	case "windows":
		want = []func(string) bool{
			func(n string) bool { return n == "mullion.exe" },
			func(n string) bool {
				return strings.HasPrefix(n, "mullion-") && strings.HasSuffix(n, "-windows-amd64.zip")
			},
		}
	case "darwin":
		if goarch != "arm64" && goarch != "amd64" {
			return Asset{}, false
		}
		suffix := "-darwin-" + goarch + ".tar.gz"
		want = []func(string) bool{
			func(n string) bool { return strings.HasPrefix(n, "mullion-") && strings.HasSuffix(n, suffix) },
		}
	}
	for _, match := range want {
		for _, a := range assets {
			if match(a.Name) {
				return a, true
			}
		}
	}
	return Asset{}, false
}

// checksumFileNames are checksum manifests a release may carry. The
// current workflow publishes none — GitHub's per-asset digest covers it
// — but a manifest is honored if one is ever added.
var checksumFileNames = []string{"checksums.txt", "SHA256SUMS", "sha256sums.txt"}

// expectedSHA256 finds the published sha256 (hex) for asset: GitHub's
// digest field first, then a checksum manifest or an "<asset>.sha256"
// file in the release. "" means nothing was published.
func expectedSHA256(ctx context.Context, rel Release, asset Asset) (string, error) {
	if d, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok && len(d) == 64 {
		return strings.ToLower(d), nil
	}
	for _, a := range rel.Assets {
		isManifest := false
		for _, n := range checksumFileNames {
			if strings.EqualFold(a.Name, n) {
				isManifest = true
			}
		}
		single := strings.EqualFold(a.Name, asset.Name+".sha256")
		if !isManifest && !single {
			continue
		}
		body, err := fetchSmall(ctx, a.URL)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", a.Name, err)
		}
		if sum := findChecksum(body, asset.Name, single); sum != "" {
			return sum, nil
		}
	}
	return "", nil
}

// findChecksum reads a sha256sum-style manifest ("<hex>  <name>" lines,
// optionally "*<name>"); single files may hold just the hex.
func findChecksum(body, name string, single bool) string {
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || len(f[0]) != 64 {
			continue
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			continue
		}
		if (len(f) == 1 && single) || (len(f) >= 2 && strings.TrimPrefix(f[1], "*") == name) {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

func fetchSmall(ctx context.Context, url string) (string, error) {
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
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(data), err
}

// download fetches url into dest, reporting progress about every 200ms,
// and returns the file's sha256.
func download(ctx context.Context, url, dest string, progress func(done, total int64)) (string, error) {
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
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	pw := &progressWriter{total: resp.ContentLength, report: progress}
	_, err = io.Copy(io.MultiWriter(f, h, pw), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	progress(pw.done, pw.total)
	return hex.EncodeToString(h.Sum(nil)), nil
}

type progressWriter struct {
	done, total int64
	last        time.Time
	report      func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if time.Since(w.last) > 200*time.Millisecond {
		w.last = time.Now()
		w.report(w.done, w.total)
	}
	return len(p), nil
}

// extractBinary writes the mullion executable contained in archive (a
// .zip, a .tar.gz, or the bare executable) to dest with mode 0755.
func extractBinary(archive, assetName, dest string) error {
	want := exeName("mullion")
	switch {
	case strings.HasSuffix(assetName, ".zip"):
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if strings.EqualFold(filepath.Base(f.Name), "mullion.exe") && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				return writeExe(rc, dest)
			}
		}
		return fmt.Errorf("%s does not contain mullion.exe", assetName)
	case strings.HasSuffix(assetName, ".tar.gz"):
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return fmt.Errorf("%s does not contain %s", assetName, want)
			}
			if err != nil {
				return err
			}
			if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "mullion" {
				return writeExe(tr, dest)
			}
		}
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeExe(f, dest)
}

func writeExe(r io.Reader, dest string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// oldExeName is where Windows parks the running executable during a
// swap: mullion.exe → mullion.old.exe.
func oldExeName(target string) string {
	base := filepath.Base(target)
	ext := filepath.Ext(base)
	return filepath.Join(filepath.Dir(target), strings.TrimSuffix(base, ext)+".old"+ext)
}

// swapExe replaces target with newFile (which must be in the same
// directory, so each step is a rename on one volume).
//
// On Windows a running executable can't be overwritten or deleted, but
// it can be renamed: target moves aside to mullion.old.exe (the running
// processes keep working from it) and newFile takes its name.
// CleanupOld deletes the parked copy on a later start, once nothing
// runs from it. A stale .old that is still locked gets a unique name
// instead of blocking the swap. Unix just renames over target — running
// processes keep the old, now unlinked, inode.
func swapExe(target, newFile, goos string) error {
	if goos != "windows" {
		if err := os.Chmod(newFile, 0o755); err != nil {
			return err
		}
		return os.Rename(newFile, target)
	}
	old := oldExeName(target)
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		ext := filepath.Ext(old)
		old = strings.TrimSuffix(old, ext) + fmt.Sprintf("-%d", time.Now().UnixNano()) + ext
	}
	if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("moving the running mullion.exe aside: %w", err)
	}
	if err := os.Rename(newFile, target); err != nil {
		_ = os.Rename(old, target) // put the working copy back
		return err
	}
	return nil
}

// CleanupOld deletes executables parked by a Windows swap (mullion.old.exe,
// mullion.old-<n>.exe) next to the running one and in extraDirs. Files
// still in use fail to delete and are simply tried again next start.
func CleanupOld(extraDirs ...string) {
	dirs := extraDirs
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		cleanupOldIn(d)
	}
}

func cleanupOldIn(dir string) {
	for _, pat := range []string{"mullion.old.exe", "mullion.old-*.exe"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// directTarget is the executable a direct upgrade replaces: the running
// one, symlinks resolved.
var directTarget = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

func upgradeDirect(ctx context.Context, rel Release, rep Reporter) (string, error) {
	asset, ok := pickAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", manualErr(Direct, "Mullion %s has no download for %s/%s", rel.Version(), runtime.GOOS, runtime.GOARCH)
	}
	target, err := directTarget()
	if err != nil {
		return "", manualErr(Direct, "finding the running mullion: %v", err)
	}
	dir := filepath.Dir(target)

	// Download next to the target, so the final step is a rename on the
	// same volume.
	tmpDir, err := os.MkdirTemp(dir, ".mullion-upgrade-*")
	if err != nil {
		return "", manualErr(Direct, "cannot write to %s (%v)", dir, err)
	}
	defer os.RemoveAll(tmpDir)

	archive := filepath.Join(tmpDir, asset.Name)
	rep.status("Downloading " + asset.Name + "…")
	var lastPct int64 = -1
	sum, err := download(ctx, asset.URL, archive, func(done, total int64) {
		if total > 0 {
			pct := done * 100 / total
			if rep.Status != nil {
				rep.Status(fmt.Sprintf("Downloading %s… %d%% (%.1f / %.1f MB)", asset.Name, pct, float64(done)/1e6, float64(total)/1e6))
			}
			if pct/25 != lastPct/25 || pct == 100 && lastPct != 100 {
				rep.line(fmt.Sprintf("    %3d%%  %.1f / %.1f MB", pct, float64(done)/1e6, float64(total)/1e6))
			}
			lastPct = pct
		}
	})
	if err != nil {
		return "", manualErr(Direct, "downloading %s: %v", asset.Name, err)
	}
	if asset.Size > 0 {
		if info, err := os.Stat(archive); err != nil || info.Size() != asset.Size {
			return "", manualErr(Direct, "the download of %s is incomplete", asset.Name)
		}
	}
	want, err := expectedSHA256(ctx, rel, asset)
	if err != nil {
		return "", manualErr(Direct, "%v", err)
	}
	switch {
	case want == "":
		rep.line("note: the release publishes no checksum for " + asset.Name + " — verified its size only")
	case want != sum:
		return "", manualErr(Direct, "%s failed its checksum (expected %s, got %s) — not installed", asset.Name, want, sum)
	default:
		rep.line("sha256 verified: " + sum)
	}

	newFile := filepath.Join(dir, ".mullion-new-"+filepath.Base(tmpDir)+filepath.Ext(target))
	if err := extractBinary(archive, asset.Name, newFile); err != nil {
		os.Remove(newFile)
		return "", manualErr(Direct, "unpacking %s: %v", asset.Name, err)
	}
	// Never swap in a binary that doesn't run.
	if got, err := binaryVersion(ctx, newFile); err != nil || Newer(rel.Version(), got) {
		os.Remove(newFile)
		if err == nil {
			err = fmt.Errorf("it reports v%s", got)
		}
		return "", manualErr(Direct, "the downloaded mullion does not work (%v) — nothing was changed", err)
	}
	rep.status("Replacing " + target + "…")
	if err := swapExe(target, newFile, runtime.GOOS); err != nil {
		os.Remove(newFile)
		return "", manualErr(Direct, "replacing %s: %v", target, err)
	}
	return target, nil
}
