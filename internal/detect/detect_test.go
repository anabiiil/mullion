package detect

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write creates a file with content at dir/name, making parent dirs as
// needed.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProject(t *testing.T) {
	cases := []struct {
		name  string
		setup func(dir string)
		want  Info
	}{
		{
			name: "laravel via composer require",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"php": "^8.2", "laravel/framework": "^11.31"}}`)
			},
			want: Info{Language: "PHP", Framework: "Laravel", FrameworkVersion: "11", Icon: "laravel"},
		},
		{
			name: "laravel version refined by composer.lock",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"laravel/framework": "^11.0"}}`)
				write(t, dir, "composer.lock", `{"packages": [{"name": "laravel/framework", "version": "v11.31.0"}]}`)
			},
			want: Info{Language: "PHP", Framework: "Laravel", FrameworkVersion: "11", Icon: "laravel"},
		},
		{
			name: "laravel fallback via artisan with no composer match",
			setup: func(dir string) {
				write(t, dir, "artisan", "#!/usr/bin/env php")
			},
			want: Info{Language: "PHP", Framework: "Laravel", Icon: "laravel"},
		},
		{
			name: "symfony",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"symfony/framework-bundle": "^7.1"}}`)
			},
			want: Info{Language: "PHP", Framework: "Symfony", FrameworkVersion: "7", Icon: "symfony"},
		},
		{
			name: "drupal",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"drupal/core": "^10.2"}}`)
			},
			want: Info{Language: "PHP", Framework: "Drupal", FrameworkVersion: "10", Icon: "drupal"},
		},
		{
			name: "codeigniter",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"codeigniter4/framework": "^4.5"}}`)
			},
			want: Info{Language: "PHP", Framework: "CodeIgniter", FrameworkVersion: "4", Icon: "codeigniter"},
		},
		{
			name: "yii",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"yiisoft/yii2": "~2.0.45"}}`)
			},
			want: Info{Language: "PHP", Framework: "Yii", FrameworkVersion: "2", Icon: "yii"},
		},
		{
			name: "cakephp",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"cakephp/cakephp": "^5.0"}}`)
			},
			want: Info{Language: "PHP", Framework: "CakePHP", FrameworkVersion: "5", Icon: "cakephp"},
		},
		{
			name: "slim",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"slim/slim": "^4.12"}}`)
			},
			want: Info{Language: "PHP", Framework: "Slim", FrameworkVersion: "4", Icon: "slim"},
		},
		{
			name: "wordpress via wp-config",
			setup: func(dir string) {
				write(t, dir, "wp-config.php", "<?php")
			},
			want: Info{Language: "PHP", Framework: "WordPress", Icon: "wordpress"},
		},
		{
			name: "wordpress via wp-content dir",
			setup: func(dir string) {
				write(t, dir, "wp-content/plugins/.keep", "")
			},
			want: Info{Language: "PHP", Framework: "WordPress", Icon: "wordpress"},
		},
		{
			name: "plain composer php project",
			setup: func(dir string) {
				write(t, dir, "composer.json", `{"require": {"php": "^8.2"}}`)
			},
			want: Info{Language: "PHP", Icon: "php"},
		},
		{
			name: "nuxt over vue",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"nuxt": "^3.13.0", "vue": "^3.5.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Nuxt", FrameworkVersion: "3", Icon: "nuxt"},
		},
		{
			name: "next over react",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"next": "^14.2.0", "react": "^18.3.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Next.js", FrameworkVersion: "14", Icon: "next"},
		},
		{
			name: "sveltekit over svelte",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"devDependencies": {"@sveltejs/kit": "^2.5.0", "svelte": "^4.2.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "SvelteKit", FrameworkVersion: "2", Icon: "sveltekit"},
		},
		{
			name: "plain vue",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Vue", FrameworkVersion: "3", Icon: "vue"},
		},
		{
			name: "plain react",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"react": "^18.3.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "React", FrameworkVersion: "18", Icon: "react"},
		},
		{
			name: "remix",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"@remix-run/react": "^2.9.0", "react": "^18.3.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Remix", FrameworkVersion: "2", Icon: "remix"},
		},
		{
			name: "angular",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"@angular/core": "^18.2.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Angular", FrameworkVersion: "18", Icon: "angular"},
		},
		{
			name: "express plain node api",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"express": "^4.19.0"}}`)
			},
			want: Info{Language: "JavaScript", Framework: "Express", FrameworkVersion: "4", Icon: "express"},
		},
		{
			name: "typescript via tsconfig",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"react": "^18.3.0"}}`)
				write(t, dir, "tsconfig.json", `{}`)
			},
			want: Info{Language: "TypeScript", Framework: "React", FrameworkVersion: "18", Icon: "react"},
		},
		{
			name: "typescript via dependency",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}, "devDependencies": {"typescript": "^5.5.0"}}`)
			},
			want: Info{Language: "TypeScript", Framework: "Vue", FrameworkVersion: "3", Icon: "vue"},
		},
		{
			name: "plain node, no framework",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"lodash": "^4.17.0"}}`)
			},
			want: Info{Language: "JavaScript", Icon: "node"},
		},
		{
			name: "package manager from pnpm lockfile",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
				write(t, dir, "pnpm-lock.yaml", "lockfileVersion: '9.0'")
			},
			want: Info{Language: "JavaScript", Framework: "Vue", FrameworkVersion: "3", Icon: "vue", PackageManager: "pnpm"},
		},
		{
			name: "package manager from yarn lockfile",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
				write(t, dir, "yarn.lock", "# yarn lockfile v1")
			},
			want: Info{Language: "JavaScript", Framework: "Vue", FrameworkVersion: "3", Icon: "vue", PackageManager: "yarn"},
		},
		{
			name: "package manager from npm lockfile",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
				write(t, dir, "package-lock.json", `{}`)
			},
			want: Info{Language: "JavaScript", Framework: "Vue", FrameworkVersion: "3", Icon: "vue", PackageManager: "npm"},
		},
		{
			name: "framework version from installed node_modules over range",
			setup: func(dir string) {
				write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
				write(t, dir, "node_modules/vue/package.json", `{"version": "3.5.13"}`)
			},
			want: Info{Language: "JavaScript", Framework: "Vue", FrameworkVersion: "3.5.13", Icon: "vue"},
		},
		{
			name: "static html only",
			setup: func(dir string) {
				write(t, dir, "index.html", "<html></html>")
			},
			want: Info{Language: "HTML", Icon: "static"},
		},
		{
			name:  "unknown empty dir",
			setup: func(dir string) {},
			want:  Info{Language: "Unknown", Icon: "unknown"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(dir)
			got := Project(dir)
			if got != c.want {
				t.Errorf("Project() = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestProjectCacheInvalidatesOnChange(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies": {"vue": "^3.5.0"}}`)
	got := Project(dir)
	if got.Framework != "Vue" {
		t.Fatalf("Project() = %+v, want Vue", got)
	}

	// Rewrite as a React project; mtime must change for the cache to
	// notice, so nudge it forward explicitly.
	write(t, dir, "package.json", `{"dependencies": {"react": "^18.3.0"}}`)
	newTime := mustStat(t, filepath.Join(dir, "package.json")).ModTime().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "package.json"), newTime, newTime); err != nil {
		t.Fatal(err)
	}

	got = Project(dir)
	if got.Framework != "React" {
		t.Fatalf("Project() after rewrite = %+v, want React (cache should invalidate on mtime change)", got)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
