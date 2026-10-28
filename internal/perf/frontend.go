package perf

// The frontend part of the readiness audit: what an existing production
// build ships (Mullion doesn't build for the check — "Build now" does,
// on request) and oversized images.

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Build-size thresholds (gzip).
const (
	JSBudgetGzip    = 500 << 10 // all the JavaScript of the build
	ChunkBudgetGzip = 250 << 10 // one file
	ImageMax        = 300 << 10
)

// Asset is one file of a build.
type Asset struct {
	Path string `json:"path"` // relative to the project
	Size int64  `json:"size"`
	Gzip int64  `json:"gzip"`
	Kind string `json:"kind"` // js, css, map
	// Unminified: looks like source (many short, indented lines).
	Unminified bool `json:"unminified,omitempty"`
}

// BuildReport is what a build ships.
type BuildReport struct {
	Dir       string  `json:"dir"` // relative; "" = no build output found
	JSSize    int64   `json:"jsSize"`
	JSGzip    int64   `json:"jsGzip"`
	CSSSize   int64   `json:"cssSize"`
	CSSGzip   int64   `json:"cssGzip"`
	Maps      int     `json:"maps"`
	Files     int     `json:"files"`
	Largest   []Asset `json:"largest"`
	Images    []Asset `json:"images"` // over ImageMax, anywhere in public/ and resources/
	Truncated bool    `json:"truncated,omitempty"`
}

// buildDirs are where builds land, in the order looked at.
var buildDirs = []string{filepath.Join("public", "build"), "dist", "build", "out", filepath.Join(".output", "public"), filepath.Join(".next", "static")}

// FindBuildDir is the project's build output ("" = none).
func FindBuildDir(dir string) string {
	for _, d := range buildDirs {
		if fi, err := os.Stat(filepath.Join(dir, d)); err == nil && fi.IsDir() {
			if d == "build" || d == "out" || d == "dist" {
				// A PHP project's build/ is often something else.
				if !hasWebFiles(filepath.Join(dir, d)) {
					continue
				}
			}
			return d
		}
	}
	// Laravel Mix: public/js + public/css with a mix-manifest.json.
	if _, err := os.Stat(filepath.Join(dir, "public", "mix-manifest.json")); err == nil {
		return "public"
	}
	return ""
}

func hasWebFiles(dir string) bool {
	found := false
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || n > 2000 {
			return filepath.SkipAll
		}
		n++
		if !d.IsDir() {
			switch strings.ToLower(filepath.Ext(p)) {
			case ".js", ".mjs", ".css", ".html":
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

const maxBuildFiles = 5000

// AnalyzeBuild measures the build in buildDir (relative to dir).
func AnalyzeBuild(dir, buildDir string) BuildReport {
	r := BuildReport{Dir: buildDir, Largest: []Asset{}, Images: []Asset{}}
	if buildDir != "" {
		root := filepath.Join(dir, buildDir)
		var assets []Asset
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || (buildDir == "public" && p != root && d.Name() == "build") {
					return filepath.SkipDir
				}
				return nil
			}
			if r.Files >= maxBuildFiles {
				r.Truncated = true
				return filepath.SkipAll
			}
			ext := strings.ToLower(filepath.Ext(p))
			kind := ""
			switch ext {
			case ".js", ".mjs":
				kind = "js"
			case ".css":
				kind = "css"
			case ".map":
				r.Maps++
				r.Files++
				return nil
			default:
				return nil
			}
			if buildDir == "public" && !strings.Contains(filepath.ToSlash(p), "/public/js/") && !strings.Contains(filepath.ToSlash(p), "/public/css/") {
				return nil // Mix: only its own output folders
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			r.Files++
			rel, _ := filepath.Rel(dir, p)
			a := Asset{Path: filepath.ToSlash(rel), Size: int64(len(data)), Gzip: gzipSize(data), Kind: kind, Unminified: looksUnminified(data)}
			if kind == "js" {
				r.JSSize += a.Size
				r.JSGzip += a.Gzip
			} else {
				r.CSSSize += a.Size
				r.CSSGzip += a.Gzip
			}
			assets = append(assets, a)
			return nil
		})
		sort.Slice(assets, func(i, j int) bool { return assets[i].Gzip > assets[j].Gzip })
		if len(assets) > 10 {
			// Keep the unminified ones past the top ten too: they are findings.
			top := append([]Asset(nil), assets[:10]...)
			for _, a := range assets[10:] {
				if a.Unminified && len(top) < 30 {
					top = append(top, a)
				}
			}
			assets = top
		}
		r.Largest = assets
	}
	r.Images = bigImages(dir)
	return r
}

func gzipSize(data []byte) int64 {
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, gzip.DefaultCompression)
	_, _ = w.Write(data)
	_ = w.Close()
	return int64(b.Len())
}

// looksUnminified: a file of a few KB or more whose lines are short and
// indented — source, not a bundler's output.
func looksUnminified(data []byte) bool {
	if len(data) < 4096 {
		return false
	}
	lines := bytes.Count(data, []byte("\n")) + 1
	if lines < 50 {
		return false
	}
	avg := len(data) / lines
	indented := bytes.Count(data, []byte("\n  ")) + bytes.Count(data, []byte("\n\t"))
	return avg < 80 && indented*4 > lines
}

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true, ".avif": true, ".svg": true}

func bigImages(dir string) []Asset {
	out := []Asset{}
	seen := 0
	for _, sub := range []string{"public", "resources", "src", "static", "assets"} {
		root := filepath.Join(dir, sub)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "vendor", ".git", "storage":
					return filepath.SkipDir
				}
				return nil
			}
			if seen++; seen > 20000 {
				return filepath.SkipAll
			}
			if !imageExt[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() <= ImageMax {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			out = append(out, Asset{Path: filepath.ToSlash(rel), Size: fi.Size(), Kind: "image"})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// CheckFrontend turns a build report into findings. buildCmd is the
// project's build command ("npm run build"; "" = none).
func CheckFrontend(r BuildReport, buildCmd string) []Finding {
	var out []Finding
	if r.Dir == "" {
		if buildCmd != "" {
			out = append(out, Finding{ID: "frontend.no-build", Category: "frontend", Severity: SevInfo,
				Title: "No production build to check",
				Why:   "Mullion checks what a build ships (sizes, source maps, minification) without building it for you. Build once to see the numbers.",
				Fix:   buildCmd, Command: buildCmd})
		}
	} else {
		if r.JSGzip > JSBudgetGzip {
			out = append(out, Finding{ID: "frontend.js-size", Category: "frontend", Severity: SevMedium,
				Title: "The build ships " + kb(r.JSGzip) + " of JavaScript (gzip)",
				Why:   "Over " + kb(JSBudgetGzip) + " compressed, a page's scripts take seconds to download and parse on a mid-range phone. Split routes with dynamic import(), drop unused dependencies, and check what the biggest chunks contain.",
				Fix:   "// lazy-load a heavy page or library\nconst Chart = () => import('./Chart.vue')"})
		}
		var big, unmin []string
		for _, a := range r.Largest {
			if a.Gzip > ChunkBudgetGzip {
				big = append(big, a.Path+"  "+kb(a.Gzip)+" gzip ("+kb(a.Size)+")")
			}
			if a.Unminified {
				unmin = append(unmin, a.Path+"  "+kb(a.Size))
			}
		}
		if len(big) > 0 {
			out = append(out, Finding{ID: "frontend.big-chunks", Category: "frontend", Severity: SevMedium,
				Title: strconv.Itoa(len(big)) + " file(s) over " + kb(ChunkBudgetGzip) + " compressed",
				Why:   "One large chunk blocks the page until all of it has loaded. Split vendor libraries (build.rollupOptions.output.manualChunks in Vite) and lazy-load what the first screen doesn't need.",
				Where: big})
		}
		if len(unmin) > 0 {
			out = append(out, Finding{ID: "frontend.unminified", Category: "frontend", Severity: SevMedium,
				Title: "Unminified files in the build",
				Why:   "These look like source code, not a minified bundle — typically a dev build copied into the build folder, or files copied as is. They are several times larger than needed.",
				Where: unmin, Fix: buildCmd})
		}
		if r.Maps > 0 {
			out = append(out, Finding{ID: "frontend.source-maps", Category: "frontend", Severity: SevLow,
				Title: strconv.Itoa(r.Maps) + " source map(s) in " + r.Dir,
				Why:   "Source maps let anyone read your original frontend code. Ship them only on purpose (e.g. uploaded to an error tracker, not served).",
				Fix:   "// vite.config.js\nbuild: { sourcemap: false }   // or 'hidden' for an error tracker"})
		}
	}
	if len(r.Images) > 0 {
		var where []string
		for _, a := range r.Images {
			where = append(where, a.Path+"  "+kb(a.Size))
		}
		out = append(out, Finding{ID: "frontend.big-images", Category: "frontend", Severity: SevLow,
			Title: strconv.Itoa(len(r.Images)) + " image(s) over " + kb(ImageMax),
			Why:   "Large images are usually the heaviest part of a page. Resize them to the size they are shown at and serve WebP/AVIF.",
			Where: where, Fix: "cwebp -q 80 in.png -o out.webp"})
	}
	return out
}

func kb(n int64) string {
	if n >= 1<<20 {
		return strconv.FormatFloat(float64(n*10/(1<<20))/10, 'f', -1, 64) + " MB"
	}
	return strconv.FormatInt((n+512)>>10, 10) + " KB"
}
