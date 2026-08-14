package phpver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"pm/internal/proc"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
)

const peclBase = "https://downloads.php.net/~windows/pecl/releases/"

// abi describes the binary flavor of an installed PHP build — a PECL
// DLL must match it exactly or PHP refuses to load it.
type abi struct {
	Branch string // "8.4"
	TS     bool   // thread-safe build?
	VS     string // compiler toolset, e.g. "vs17"
}

// InstallPeclExtension downloads a prebuilt PECL extension (e.g. redis,
// xdebug, imagick) that the stock PHP build does not ship, drops its
// DLL into the version's ext directory, and enables it.
func InstallPeclExtension(ctx context.Context, paths pmdir.Paths, version, name string) error {
	name = normalizeExtName(name)
	target, err := detectABI(paths, version)
	if err != nil {
		return err
	}

	extVer, zipURL, err := findPeclZip(ctx, name, target)
	if err != nil {
		return err
	}
	fmt.Printf("Found %s %s for PHP %s (%s)\n", name, extVer, target.Branch, target.VS)

	zipPath := filepath.Join(paths.TmpDir(), "pecl-"+name+".zip")
	if err := download.ToFile(ctx, zipURL, zipPath); err != nil {
		return err
	}
	defer os.Remove(zipPath)

	staging := filepath.Join(paths.TmpDir(), "pecl-extract")
	os.RemoveAll(staging)
	if err := archive.ExtractZip(zipPath, staging); err != nil {
		return fmt.Errorf("extracting %s: %w", zipPath, err)
	}
	defer os.RemoveAll(staging)

	// The extension DLL goes into ext\. Everything else the archive
	// ships (imagick bundles the whole ImageMagick runtime — CORE_RL_*
	// and friends) must sit next to php.exe or the extension fails to
	// load with a misleading "module could not be found".
	extDll := "php_" + name + ".dll"
	found := false
	deps := 0
	werr := filepath.WalkDir(staging, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".dll") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), extDll) {
			found = true
			return os.WriteFile(filepath.Join(paths.PhpVersionDir(version), "ext", extDll), data, 0o755)
		}
		deps++
		return os.WriteFile(filepath.Join(paths.PhpVersionDir(version), d.Name()), data, 0o755)
	})
	if werr != nil {
		return werr
	}
	if !found {
		return fmt.Errorf("the downloaded archive has no %s in it", extDll)
	}
	if deps > 0 {
		fmt.Printf("Placed %d support DLL(s) next to php.exe.\n", deps)
	}

	return SetExtension(paths, version, name, true)
}

// detectABI asks the installed php.exe about its build flavor.
func detectABI(paths pmdir.Paths, version string) (abi, error) {
	php := filepath.Join(paths.PhpVersionDir(version), "php.exe")
	out, err := proc.Quiet(php, "-i").Output()
	if err != nil {
		return abi{}, fmt.Errorf("running php -i for PHP %s: %w", version, err)
	}
	// "PHP Extension Build => API20240924,NTS,VS17"
	m := regexp.MustCompile(`PHP Extension Build => API\d+,(NTS|TS),(V[SC]\d+)`).FindSubmatch(out)
	if m == nil {
		return abi{}, fmt.Errorf("could not detect the PHP build flavor from php -i")
	}
	sel, err := ParseSelector(version)
	if err != nil {
		return abi{}, err
	}
	return abi{
		Branch: fmt.Sprintf("%d.%d", sel.Major, sel.Minor),
		TS:     string(m[1]) == "TS",
		VS:     strings.ToLower(string(m[2])),
	}, nil
}

// findPeclZip walks the extension's release listing, newest version
// first, and returns the first zip matching the target ABI.
func findPeclZip(ctx context.Context, name string, target abi) (extVer, url string, err error) {
	listing, err := fetchPecl(ctx, peclBase+name+"/")
	if err != nil {
		return "", "", fmt.Errorf("extension %q not found on the PECL Windows builds server", name)
	}
	verRe := regexp.MustCompile(`href="([0-9][^"/]*)/"`)
	var versions []string
	for _, m := range verRe.FindAllStringSubmatch(listing, -1) {
		versions = append(versions, m[1])
	}
	if len(versions) == 0 {
		return "", "", fmt.Errorf("no releases listed for extension %q", name)
	}
	sort.Slice(versions, func(i, j int) bool { return peclVerLess(versions[j], versions[i]) })

	ts := "nts"
	if target.TS {
		ts = "ts"
	}
	fileRe := regexp.MustCompile(fmt.Sprintf(
		`href="(php_%s-[^"]*-%s-%s-%s-x64\.zip)"`,
		regexp.QuoteMeta(name), regexp.QuoteMeta(target.Branch), ts, regexp.QuoteMeta(target.VS)))

	for _, v := range versions {
		dir, err := fetchPecl(ctx, peclBase+name+"/"+v+"/")
		if err != nil {
			continue
		}
		if m := fileRe.FindStringSubmatch(dir); m != nil {
			return v, peclBase + name + "/" + v + "/" + m[1], nil
		}
	}
	return "", "", fmt.Errorf("no %s build of %q matches PHP %s %s %s x64 — the extension may not support this PHP version yet",
		ts, name, target.Branch, target.VS, ts)
}

func fetchPecl(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(body), err
}

// peclVerLess orders dotted versions with possible suffixes (1.2.3RC1).
func peclVerLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, nb := 0, 0
		fmt.Sscanf(pa[i], "%d", &na)
		fmt.Sscanf(pb[i], "%d", &nb)
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}
