package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// nodeFrameworks is checked in order against package.json's merged
// dependencies; the first match wins. Meta-frameworks come before the
// base framework they build on (Nuxt before Vue, Next before React,
// SvelteKit before Svelte, ...).
var nodeFrameworks = []struct {
	pkg, name, icon string
}{
	{"nuxt", "Nuxt", "nuxt"},
	{"next", "Next.js", "next"},
	{"@sveltejs/kit", "SvelteKit", "sveltekit"},
	{"astro", "Astro", "astro"},
	{"gatsby", "Gatsby", "gatsby"},
	{"@angular/core", "Angular", "angular"},
	{"@builder.io/qwik", "Qwik", "qwik"},
	{"@nestjs/core", "NestJS", "nest"},
	{"solid-js", "Solid", "solid"},
	{"svelte", "Svelte", "svelte"},
	{"vue", "Vue", "vue"},
	{"react", "React", "react"},
	{"express", "Express", "express"},
	{"vite", "Vite", "vite"},
}

// remixPrefix matches any of the @remix-run/* packages (react, node,
// dev, serve, ...); Remix is checked ahead of Gatsby, alongside the
// other meta-frameworks.
const remixPrefix = "@remix-run/"

func detectNode(dir string) (Info, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return Info{}, false
	}
	var pkg packageJSON
	_ = json.Unmarshal(data, &pkg) // best-effort; malformed -> no deps found

	deps := map[string]string{}
	for k, v := range pkg.Dependencies {
		deps[k] = v
	}
	for k, v := range pkg.DevDependencies {
		deps[k] = v
	}

	language := "JavaScript"
	if _, ok := deps["typescript"]; ok || fileExists(filepath.Join(dir, "tsconfig.json")) {
		language = "TypeScript"
	}

	info := Info{Language: language, Icon: "node", PackageManager: packageManager(dir)}

	// Remix is checked right after Astro: a meta-framework, more
	// specific than the plain React it also depends on.
	if remixPkg, ok := remixDep(deps); ok {
		info.Framework = "Remix"
		info.Icon = "remix"
		info.FrameworkVersion = frameworkVersion(dir, remixPkg, deps[remixPkg])
		return info, true
	}

	for _, fw := range nodeFrameworks {
		if constraint, ok := deps[fw.pkg]; ok {
			info.Framework = fw.name
			info.Icon = fw.icon
			info.FrameworkVersion = frameworkVersion(dir, fw.pkg, constraint)
			return info, true
		}
	}

	return info, true
}

func remixDep(deps map[string]string) (string, bool) {
	for name := range deps {
		if strings.HasPrefix(name, remixPrefix) {
			return name, true
		}
	}
	return "", false
}

// frameworkVersion prefers the exact installed version from
// node_modules/<pkg>/package.json, falling back to the major version
// parsed out of the declared range.
func frameworkVersion(dir, pkg, rangeStr string) string {
	data, err := os.ReadFile(filepath.Join(dir, "node_modules", pkg, "package.json"))
	if err == nil {
		var installed struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &installed) == nil && installed.Version != "" {
			return installed.Version
		}
	}
	return majorFromConstraint(rangeStr)
}

// lockfiles maps a lockfile name to the package manager that writes it.
var lockfiles = []struct {
	file, manager string
}{
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "yarn"},
	{"bun.lockb", "bun"},
	{"bun.lock", "bun"},
	{"package-lock.json", "npm"},
}

func packageManager(dir string) string {
	for _, lf := range lockfiles {
		if fileExists(filepath.Join(dir, lf.file)) {
			return lf.manager
		}
	}
	return ""
}
