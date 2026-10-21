package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
	"pm/internal/redis"
	"pm/internal/redisadmin"
)

func redisTestApp(t *testing.T) *App {
	t.Helper()
	paths := pmdir.Paths{Home: t.TempDir(), Backups: filepath.Join(t.TempDir(), "backups")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	old := redis.Port
	t.Cleanup(func() { redis.Port = old })
	redis.Port = 26379
	return &App{Paths: paths, State: state, skipApply: true}
}

func addLaravelSite(t *testing.T, a *App, name, env string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "artisan"), []byte("<?php"), 0o644)
	if env != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
	}
	a.State.AddSite(config.Site{Name: name, Path: dir})
	return dir
}

func TestUseRedisInProject(t *testing.T) {
	a := redisTestApp(t)
	dir := addLaravelSite(t, a, "shop", "APP_NAME=Shop\nCACHE_STORE=database\nREDIS_HOST=127.0.0.1\n")
	plan, _, err := a.ProjectEnvPlan("shop", nil)
	if err != nil || len(plan) != 6 {
		t.Fatalf("plan = %+v %v", plan, err)
	}
	// Only the chosen keys.
	backup, err := a.UseRedisInProject("shop", []string{"CACHE_STORE", "REDIS_PORT"})
	if err != nil {
		t.Fatal(err)
	}
	if backup != filepath.Join(dir, ".env.mullion-backup") {
		t.Errorf("backup = %q", backup)
	}
	old, _ := os.ReadFile(backup)
	if string(old) != "APP_NAME=Shop\nCACHE_STORE=database\nREDIS_HOST=127.0.0.1\n" {
		t.Errorf("backup content = %q", old)
	}
	if st, _ := os.Stat(filepath.Join(dir, ".env")); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf(".env mode changed to %v", st.Mode().Perm())
	}
	vals := redis.ParseEnv(string(must2(os.ReadFile(filepath.Join(dir, ".env")))))
	if vals["CACHE_STORE"] != "redis" || vals["REDIS_PORT"] != "26379" || vals["SESSION_DRIVER"] != "" {
		t.Errorf(".env after = %v", vals)
	}
	info, err := a.ProjectCache("shop")
	if err != nil || info.Installed || !info.Settings.PointsHere || len(info.Plan) != 4 || info.Settings.Prefix != "shop_database_" {
		t.Errorf("cache info = %+v %v", info, err)
	}
	if _, err := a.ProjectCache("nope"); err == nil {
		t.Error("unknown site should fail")
	}
	// Nothing left to do: no write, no backup.
	if _, err := a.UseRedisInProject("shop", nil); err != nil {
		t.Fatal(err)
	}
	if b, err := a.UseRedisInProject("shop", nil); err != nil || b != "" {
		t.Errorf("second apply = %q %v", b, err)
	}
}

func TestProjectCacheGuards(t *testing.T) {
	a := redisTestApp(t)
	addLaravelSite(t, a, "bare", "REDIS_PREFIX=\nCACHE_PREFIX=\n")
	if _, err := a.FlushProjectCache("bare"); err == nil || !strings.Contains(err.Error(), "no prefix") {
		t.Errorf("flush without a prefix = %v", err)
	}
	addLaravelSite(t, a, "shop", "APP_NAME=Shop\n")
	if err := a.DeleteProjectKey("shop", 0, "other_app_key"); err != errOutsideProject {
		t.Errorf("deleting another project's key = %v", err)
	}
	if _, err := a.ProjectKeyPreview("shop", 0, "other"); err != errOutsideProject {
		t.Errorf("preview of another project's key = %v", err)
	}
	if _, err := a.ProjectKeys("shop", 0, "", "", 10); err == nil {
		t.Error("listing keys without Redis installed should fail")
	}
	if got := projectPattern("a*b_", "x?"); got != `a\*b_*x\?*` {
		t.Errorf("projectPattern = %q", got)
	}
}

func TestRedisAdminSiteGuard(t *testing.T) {
	a := redisTestApp(t)
	a.State.Config.Redis = "8.10.2"
	a.State.Config.GlobalPHP = "8.4.1"
	// A user's own site called "redis" is never taken over or removed.
	userDir := t.TempDir()
	a.State.AddSite(config.Site{Name: redisadmin.SiteName, Path: userDir})
	if _, err := a.EnsureRedisAdmin(context.Background(), -1); err == nil || !strings.Contains(err.Error(), "already named") {
		t.Errorf("EnsureRedisAdmin over a user site = %v", err)
	}
	if err := a.UninstallAdminTool("phpredisadmin"); err != nil {
		t.Fatal(err)
	}
	if s := a.State.FindSite(redisadmin.SiteName); s == nil || s.Path != userDir {
		t.Error("the user's site named redis was removed")
	}
	if a.RedisAdminURL(0) != "" {
		t.Error("no admin URL when redis.<tld> isn't ours")
	}
	var found bool
	for _, tool := range a.AdminTools() {
		if tool.Name == "phpredisadmin" {
			found = true
			if tool.Installed || !tool.EngineInstalled {
				t.Errorf("tool = %+v", tool)
			}
		}
	}
	if !found {
		t.Error("AdminTools lacks phpRedisAdmin")
	}
}

func TestRedisPortsAndSnippet(t *testing.T) {
	a := redisTestApp(t)
	a.State.Config.Redis = "8.10.2"
	var got *PortInfo
	for _, s := range a.services() {
		if s.key == "redis" {
			got = &s.info
		}
	}
	if got == nil || got.Port != 26379 || !got.Configurable || got.Service != "Redis 8.10.2" {
		t.Errorf("redis port row = %+v", got)
	}
	if by := a.reservedBy(26379, "mysql"); by != "Redis 8.10.2" {
		t.Errorf("reservedBy = %q", by)
	}
	if n, err := normalizeEngine("Redis"); err != nil || n != "redis" {
		t.Errorf("normalizeEngine = %q %v", n, err)
	}
	ApplyPortConfig(config.Config{RedisPort: 6390})
	if redis.Port != 6390 {
		t.Errorf("ApplyPortConfig: port %d", redis.Port)
	}
	ApplyPortConfig(config.Config{})
	if redis.Port != redis.DefaultPort {
		t.Errorf("default port %d", redis.Port)
	}
	snip := RedisEnvSnippet(6380)
	for _, want := range []string{"REDIS_CLIENT=phpredis\n", "REDIS_HOST=127.0.0.1\n", "REDIS_PASSWORD=null\n", "REDIS_PORT=6380\n",
		"CACHE_STORE=redis\n", "SESSION_DRIVER=redis\n", "QUEUE_CONNECTION=redis\n", "CACHE_DRIVER=redis"} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet lacks %q:\n%s", want, snip)
		}
	}
	if engineAdminTool("redis") != "phpredisadmin" || !strings.HasSuffix(engineDataLocation(a.Paths, "redis"), "data") {
		t.Error("engine helpers")
	}
	if e, ok := backupEngineFromName("2026-10-21_101010-redis"); !ok || e != "redis" {
		t.Error("redis backups not recognized")
	}
}

// TestRedisE2E drives the app layer against a REAL redis-server on port
// 26379 (REDIS_E2E_BIN = a folder with redis-server): install-free
// activation, project keys, preview, delete, flush by prefix, backup and
// restore, and uninstall keeping the data.
func TestRedisE2E(t *testing.T) {
	bin := os.Getenv("REDIS_E2E_BIN")
	if bin == "" {
		t.Skip("set REDIS_E2E_BIN to run")
	}
	a := redisTestApp(t)
	exe := redis.ServerExe(a.Paths, "9.9.9")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, must2(os.ReadFile(filepath.Join(bin, "redis-server"))), 0o755)
	a.State.Config.Redis = "9.9.9"
	if err := a.StartRedis(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { redis.Stop(a.Paths) })
	addLaravelSite(t, a, "shop", "APP_NAME=Shop\n")

	c, err := a.RedisClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Do("SET", "shop_database_queues:default", "x")
	c.Do("SET", "other_database_k", "y")
	c.Select(1)
	for _, k := range []string{"a", "b", "c"} {
		c.Do("SET", "shop_database_shop_cache_"+k, k, "EX", "600")
	}
	c.Do("SET", "shop_database_sess", "s")

	info, err := a.ProjectCache("shop")
	if err != nil || !info.Running || info.Keys != 1 || info.CacheKeys != 4 {
		t.Fatalf("ProjectCache = %+v %v", info, err)
	}
	page, err := a.ProjectKeys("shop", 1, "cache", "", 2)
	if err != nil || len(page.Keys) < 2 {
		t.Fatalf("ProjectKeys = %+v %v", page, err)
	}
	t.Logf("page: cursor=%s keys=%+v", page.Cursor, page.Keys)
	for _, k := range page.Keys {
		if k.Type != "string" || k.TTL <= 0 || k.Memory <= 0 {
			t.Errorf("key info %+v", k)
		}
	}
	p, err := a.ProjectKeyPreview("shop", 1, "shop_database_shop_cache_a")
	if err != nil || p.Value != "a" {
		t.Errorf("preview = %+v %v", p, err)
	}
	if err := a.DeleteProjectKey("shop", 1, "shop_database_shop_cache_a"); err != nil {
		t.Fatal(err)
	}
	n, err := a.FlushProjectCache("shop")
	if err != nil || n != 2 {
		t.Errorf("FlushProjectCache = %d %v", n, err)
	}
	if left, _ := c.DBSize(); left != 1 { // the session key stays
		t.Errorf("db1 has %d keys after the flush, want 1", left)
	}

	dir, err := a.BackupEngine(context.Background(), "redis")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backup: %s", dir)
	if _, err := os.Stat(filepath.Join(dir, "dump.rdb")); err != nil {
		t.Fatal(err)
	}
	if err := a.FlushRedis(-1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := a.RestoreBackup(context.Background(), dir, ""); err != nil {
		t.Fatal(err)
	}
	c2, err := a.RedisClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if n, _ := c2.DBSize(); n != 2 {
		t.Errorf("db0 has %d keys after restore, want 2", n)
	}
	if err := a.StopRedis(); err != nil || !a.State.Config.RedisStopped {
		t.Fatalf("StopRedis: %v", err)
	}
	// A backup of a stopped server starts it briefly and leaves it stopped.
	if _, err := a.BackupEngine(context.Background(), "redis"); err != nil {
		t.Fatal(err)
	}
	if redis.Owned(a.Paths) {
		t.Error("backup left a stopped Redis running")
	}
	if _, err := a.UninstallEngine(context.Background(), "redis", false, false); err != nil {
		t.Fatal(err)
	}
	if a.State.Config.Redis != "" {
		t.Error("config still names Redis")
	}
	if _, err := os.Stat(redis.DumpFile(a.Paths)); err != nil {
		t.Error("uninstall without deleteData must keep dump.rdb")
	}
	if len(redis.Installed(a.Paths)) != 0 {
		t.Error("binaries still there")
	}
}

func must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
