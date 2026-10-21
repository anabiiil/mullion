package app

// Redis: install/start/stop, the phpRedisAdmin site, backups, doctor,
// and the project page's Cache tab. The server itself lives in
// internal/redis, the web UI in internal/redisadmin.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/redis"
	"pm/internal/redisadmin"
)

var errRedisNotInstalled = redis.ErrNotInstalled

// InstallRedis installs the latest Redis (building it from source on
// macOS — a no-op when that version is already there), makes it the
// active one, and starts it. Run again later it is the upgrade: the old
// version is stopped and removed once the new one is up; the data is
// kept. Returns the version now active.
func (a *App) InstallRedis(ctx context.Context, progress redis.Progress) (string, error) {
	v, err := redis.Install(ctx, a.Paths, progress)
	if err != nil {
		return "", err
	}
	c := &a.State.Config
	prev := c.Redis
	if prev != "" && prev != v {
		if err := redis.Stop(a.Paths); err != nil {
			return "", err
		}
	}
	c.Redis = v
	c.RedisStopped = false
	if err := a.State.Save(); err != nil {
		return "", err
	}
	if !a.skipApply {
		if progress != nil {
			progress("Starting Redis…")
		}
		if err := redis.Start(a.Paths, v); err != nil {
			return v, err
		}
	}
	if prev != "" && prev != v {
		_ = redis.RemoveVersion(a.Paths, prev)
	}
	return v, nil
}

// RedisUpdate reports the newest published Redis when it is newer than
// the installed one ("" otherwise, or offline).
func (a *App) RedisUpdate(ctx context.Context) string {
	cur := a.State.Config.Redis
	if cur == "" {
		return ""
	}
	latest, err := redis.LatestVersion(ctx)
	if err != nil || !redis.Newer(latest, cur) {
		return ""
	}
	return latest
}

// StartRedis starts Redis and clears the stopped flag.
func (a *App) StartRedis() error {
	v := a.State.Config.Redis
	if v == "" {
		return errRedisNotInstalled
	}
	if err := redis.Start(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.RedisStopped = false
	return a.State.Save()
}

// StopRedis stops Redis (saving a snapshot first) and records that the
// user did, so Apply/self-heal/autostart leave it down.
func (a *App) StopRedis() error {
	if a.State.Config.Redis == "" {
		return nil
	}
	if err := redis.Stop(a.Paths); err != nil {
		return err
	}
	a.State.Config.RedisStopped = true
	return a.State.Save()
}

// RestartRedis bounces Redis.
func (a *App) RestartRedis() error {
	if a.State.Config.Redis == "" {
		return errRedisNotInstalled
	}
	if err := redis.Stop(a.Paths); err != nil {
		return err
	}
	return a.StartRedis()
}

// redisConn connects to Mullion's Redis, failing clearly when it isn't
// installed or running.
func (a *App) redisConn() (*redis.Client, error) {
	if a.State.Config.Redis == "" {
		return nil, errRedisNotInstalled
	}
	c, err := redis.Connect(3 * time.Second)
	if err != nil {
		if !redis.Running() {
			return nil, errors.New("Redis is not running — start it first")
		}
		return nil, err
	}
	return c, nil
}

// RedisClient opens a connection to Mullion's Redis (the caller closes
// it).
func (a *App) RedisClient() (*redis.Client, error) { return a.redisConn() }

// FlushRedis empties one database (db >= 0) or all of them (db < 0).
func (a *App) FlushRedis(db int) error {
	c, err := a.redisConn()
	if err != nil {
		return err
	}
	defer c.Close()
	if db < 0 {
		return c.FlushAll()
	}
	if db >= redis.Databases {
		return fmt.Errorf("database %d does not exist (0-%d)", db, redis.Databases-1)
	}
	if err := c.Select(db); err != nil {
		return err
	}
	return c.FlushDB()
}

/* ── phpRedisAdmin (redis.<tld>) ───────────────────────────── */

// redisAdminLinked reports whether redis.<tld> is Mullion's
// phpRedisAdmin (and not a user's own site that happens to be named
// "redis").
func (a *App) redisAdminLinked() bool {
	s := a.State.FindSite(redisadmin.SiteName)
	return s != nil && filepath.Clean(s.Path) == filepath.Clean(redisadmin.Dir(a.Paths))
}

// RedisAdminURL is phpRedisAdmin's address ("" when not linked); db >= 0
// opens that database.
func (a *App) RedisAdminURL(db int) string {
	if !a.redisAdminLinked() {
		return ""
	}
	return redisadmin.URL(a.State.Config.TLD, db)
}

// EnsureRedisAdmin installs phpRedisAdmin (keeping an existing copy),
// links it as the secured site redis.<tld>, converges the machine, and
// returns its URL (opening database db when db >= 0).
func (a *App) EnsureRedisAdmin(ctx context.Context, db int) (string, error) {
	if a.State.Config.Redis == "" {
		return "", errRedisNotInstalled
	}
	if a.State.Config.GlobalPHP == "" {
		return "", fmt.Errorf("phpRedisAdmin needs PHP: run `mullion php install 8.4` and `mullion use 8.4` first")
	}
	if s := a.State.FindSite(redisadmin.SiteName); s != nil && !a.redisAdminLinked() {
		return "", fmt.Errorf("a linked site is already named %q (%s), so phpRedisAdmin can't use %s — rename that site first",
			redisadmin.SiteName, s.Path, redisadmin.SiteName+"."+a.State.Config.TLD)
	}
	if _, err := redisadmin.Install(ctx, a.Paths, redis.Port, redis.Password); err != nil {
		return "", err
	}
	if !a.redisAdminLinked() {
		a.State.AddSite(config.Site{Name: redisadmin.SiteName, Path: redisadmin.Dir(a.Paths), Secure: true})
	}
	if err := a.applyIfEnabled(); err != nil {
		return "", err
	}
	if !a.skipApply {
		_ = caddy.TrustCA(a.Paths)
	}
	return redisadmin.URL(a.State.Config.TLD, db), nil
}

// afterRedisPortChange follows a moved port in phpRedisAdmin's config.
func (a *App) afterRedisPortChange() error {
	return redisadmin.WriteConfig(a.Paths, redis.Port, redis.Password)
}

/* ── backups ───────────────────────────────────────────────── */

// backupRedis snapshots the dataset (SAVE) and copies dump.rdb into a
// fresh <timestamp>-redis backup folder, starting Redis for it if it is
// stopped (and stopping it again afterwards).
func (a *App) backupRedis() (string, error) {
	v := a.State.Config.Redis
	if v == "" {
		return "", errRedisNotInstalled
	}
	dir := newBackupDir(a.Paths, "redis")
	err := withEngineTempRunning(redis.Owned(a.Paths) && redis.Running(),
		func() error { return redis.Start(a.Paths, v) },
		func() error { return redis.Stop(a.Paths) },
		func() error {
			c, err := redis.Connect(5 * time.Second)
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Save(); err != nil {
				return fmt.Errorf("Redis could not save a snapshot: %w", err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			return copyFileTo(redis.DumpFile(a.Paths), filepath.Join(dir, "dump.rdb"))
		})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// restoreRedisBackup replaces the dataset with a backup's dump.rdb:
// Redis is stopped (it would overwrite the file on shutdown otherwise),
// the current dump.rdb is kept as dump.rdb.before-restore, the backup is
// copied in, and Redis is started again (and left as it was).
func (a *App) restoreRedisBackup(dir string) error {
	v := a.State.Config.Redis
	if v == "" {
		return errRedisNotInstalled
	}
	src := filepath.Join(dir, "dump.rdb")
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("%s has no dump.rdb", dir)
	}
	return a.RestoreRedisFile(src)
}

// RestoreRedisFile loads an RDB file (a backup's dump.rdb, or any
// dump.rdb from another Redis of the same or an older version).
func (a *App) RestoreRedisFile(src string) error {
	v := a.State.Config.Redis
	if v == "" {
		return errRedisNotInstalled
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	head := make([]byte, 5)
	_, _ = f.Read(head)
	f.Close()
	if string(head) != "REDIS" {
		return fmt.Errorf("%s is not a Redis RDB snapshot", src)
	}
	wasRunning := redis.Owned(a.Paths)
	if err := redis.Stop(a.Paths); err != nil {
		return err
	}
	dump := redis.DumpFile(a.Paths)
	if err := os.MkdirAll(filepath.Dir(dump), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dump); err == nil {
		if err := copyFileTo(dump, dump+".before-restore"); err != nil {
			return err
		}
	}
	if err := copyFileTo(src, dump); err != nil {
		return err
	}
	if err := redis.Start(a.Paths, v); err != nil {
		return fmt.Errorf("Redis did not start with the restored data: %w (the previous data is in %s)", err, dump+".before-restore")
	}
	if !wasRunning && a.State.Config.RedisStopped {
		return redis.Stop(a.Paths)
	}
	return nil
}

func copyFileTo(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

/* ── doctor / self-heal ────────────────────────────────────── */

func (a *App) redisDoctorChecks() []DoctorCheck {
	c := a.State.Config
	if c.Redis == "" {
		return nil
	}
	name := "redis " + c.Redis
	ours := redis.Owned(a.Paths)
	up := redis.Running()
	var check DoctorCheck
	switch {
	case ours && up:
		check = DoctorCheck{Name: name, Status: DoctorOK, Detail: fmt.Sprintf("127.0.0.1:%d", redis.Port)}
	case !ours && up:
		check = DoctorCheck{Name: name, Status: DoctorFail,
			Detail: fmt.Sprintf("another program holds port %d — stop it, or move Redis (`mullion ports`)", redis.Port)}
	case c.RedisStopped:
		check = DoctorCheck{Name: name, Status: DoctorWarn, Detail: "stopped by you (`mullion redis start`)"}
	default:
		check = DoctorCheck{Name: name, Status: DoctorFail, Detail: "not running — `mullion redis start`"}
		if data, err := os.ReadFile(redis.LogFile(a.Paths)); err == nil && len(data) > 0 {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			var b strings.Builder
			b.WriteString("  redis log tail:\n")
			for _, l := range lines {
				b.WriteString("    " + l + "\n")
			}
			check.Fix = b.String()
		}
	}
	checks := []DoctorCheck{check}
	if s := a.State.FindSite(redisadmin.SiteName); s != nil && !a.redisAdminLinked() {
		checks = append(checks, DoctorCheck{Name: "redis: web admin", Status: DoctorWarn,
			Detail: "a linked site is named \"redis\", so phpRedisAdmin can't be served on " + redisadmin.SiteName + "." + c.TLD})
	}
	return checks
}

// stopRedisForStack stops Redis for a whole-stack stop without setting
// the user's stopped flag.
func (a *App) stopRedisForStack() error {
	if a.State.Config.Redis == "" {
		return nil
	}
	return redis.Stop(a.Paths)
}

// healRedis is Apply's self-heal for Redis. A failure is only reported:
// a port taken by another Redis must not fail every command.
func (a *App) healRedis() {
	c := a.State.Config
	if !shouldSelfHeal(c.Redis, c.RedisStopped) {
		return
	}
	if err := redis.Start(a.Paths, c.Redis); err != nil {
		fmt.Println("warning: Redis:", err)
	}
}

/* ── connection snippet ────────────────────────────────────── */

// RedisEnvSnippet is the Laravel .env block for Mullion's Redis.
// CACHE_STORE is Laravel 11+'s name; Laravel 10 and older read
// CACHE_DRIVER — both are given, each version ignores the other.
func RedisEnvSnippet(port int) string {
	lines := []string{}
	for _, kv := range redis.LaravelEnv(port, redis.Password, false) {
		lines = append(lines, kv[0]+"="+kv[1])
	}
	lines = append(lines, "CACHE_DRIVER=redis   # Laravel 10 and older")
	return strings.Join(lines, "\n") + "\n"
}
