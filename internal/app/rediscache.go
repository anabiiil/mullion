package app

// The project page's Cache tab: one project's view of Mullion's Redis —
// only the keys under its prefix (read from its .env, see
// redis.ProjectSettings), a bounded preview of any of them, deleting
// them, and the "Use Redis in this project" .env edit.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/redis"
)

// ProjectCacheInfo is the Cache tab's header.
type ProjectCacheInfo struct {
	Site     string             `json:"site"`
	Settings redis.ProjectRedis `json:"settings"`
	// Installed / Running describe Mullion's Redis.
	Installed bool `json:"installed"`
	Running   bool `json:"running"`
	Port      int  `json:"port"`
	// Keys and CacheKeys count the project's keys in its default and
	// cache databases (capped at keyCountCap: More is then true).
	Keys      int  `json:"keys"`
	CacheKeys int  `json:"cacheKeys"`
	More      bool `json:"more"`
	// Plan is what "Use Redis in this project" would change (empty when
	// the .env already says so).
	Plan []redis.EnvChange `json:"plan"`
	Diff string            `json:"diff"`
}

const keyCountCap = 20000

func (a *App) siteDir(site string) (string, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return "", fmt.Errorf("no site named %q", site)
	}
	return s.Path, nil
}

// ProjectRedisSettings reads a site's Redis settings from its .env.
func (a *App) ProjectRedisSettings(site string) (redis.ProjectRedis, error) {
	dir, err := a.siteDir(site)
	if err != nil {
		return redis.ProjectRedis{}, err
	}
	return redis.ProjectSettings(dir, redis.Port), nil
}

// ProjectCache gathers the Cache tab's header: settings, key counts,
// and the .env plan.
func (a *App) ProjectCache(site string) (ProjectCacheInfo, error) {
	dir, err := a.siteDir(site)
	if err != nil {
		return ProjectCacheInfo{}, err
	}
	info := ProjectCacheInfo{
		Site:      site,
		Settings:  redis.ProjectSettings(dir, redis.Port),
		Installed: a.State.Config.Redis != "",
		Port:      redis.Port,
	}
	plan, _, err := a.ProjectEnvPlan(site, nil)
	if err == nil {
		info.Plan = plan
		info.Diff = redis.DiffEnv(plan)
	}
	if info.Plan == nil {
		info.Plan = []redis.EnvChange{}
	}
	if !info.Installed {
		return info, nil
	}
	c, err := redis.Connect(2 * time.Second)
	if err != nil {
		return info, nil
	}
	defer c.Close()
	info.Running = true
	count := func(db int) int {
		if c.Select(db) != nil {
			return 0
		}
		keys, more, err := c.ScanAll(projectPattern(info.Settings.Prefix, ""), keyCountCap)
		if err != nil {
			return 0
		}
		if more {
			info.More = true
		}
		return len(keys)
	}
	info.Keys = count(info.Settings.DB)
	if info.Settings.CacheDB != info.Settings.DB {
		info.CacheKeys = count(info.Settings.CacheDB)
	}
	return info, nil
}

// projectPattern is the SCAN MATCH pattern for a project's keys,
// optionally narrowed to those containing query.
func projectPattern(prefix, query string) string {
	p := redis.GlobEscape(prefix) + "*"
	if query != "" {
		p += redis.GlobEscape(query) + "*"
	}
	return p
}

// KeyInfo is one row of the key list.
type KeyInfo struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	TTL    int64  `json:"ttl"` // ms; -1 none
	Memory int64  `json:"memory"`
}

// KeyPage is one page of a project's keys.
type KeyPage struct {
	DB     int       `json:"db"`
	Keys   []KeyInfo `json:"keys"`
	Cursor string    `json:"cursor"` // "0" = no more
	Prefix string    `json:"prefix"`
}

// ProjectKeys returns a page of about count of the project's keys in
// database db containing query (a SCAN batch is never split, so a page
// can run a little over), continuing a SCAN from cursor ("" or "0" =
// start).
// It never uses KEYS; one call runs at most a bounded number of SCAN
// steps, so a huge keyspace can't stall the panel.
func (a *App) ProjectKeys(site string, db int, query, cursor string, count int) (KeyPage, error) {
	set, err := a.ProjectRedisSettings(site)
	if err != nil {
		return KeyPage{}, err
	}
	if db < 0 || db >= redis.Databases {
		return KeyPage{}, fmt.Errorf("database %d does not exist", db)
	}
	if count <= 0 || count > 500 {
		count = 100
	}
	c, err := a.redisConn()
	if err != nil {
		return KeyPage{}, err
	}
	defer c.Close()
	if err := c.Select(db); err != nil {
		return KeyPage{}, err
	}
	var cur uint64
	if cursor != "" {
		if cur, err = strconv.ParseUint(cursor, 10, 64); err != nil {
			return KeyPage{}, fmt.Errorf("bad cursor %q", cursor)
		}
	}
	pattern := projectPattern(set.Prefix, query)
	seen := map[string]bool{}
	var keys []string
	for steps := 0; steps < 200 && len(keys) < count; steps++ {
		next, batch, err := c.Scan(cur, pattern, 500, "")
		if err != nil {
			return KeyPage{}, err
		}
		for _, k := range batch {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		cur = next
		if cur == 0 {
			break
		}
	}
	page := KeyPage{DB: db, Cursor: strconv.FormatUint(cur, 10), Prefix: set.Prefix, Keys: make([]KeyInfo, 0, len(keys))}
	cmds := make([][]string, 0, len(keys)*3)
	for _, k := range keys {
		cmds = append(cmds, []string{"TYPE", k}, []string{"PTTL", k}, []string{"MEMORY", "USAGE", k, "SAMPLES", "0"})
	}
	vals, _, err := c.DoMulti(cmds)
	if err != nil {
		return KeyPage{}, err
	}
	for i, k := range keys {
		ki := KeyInfo{Key: k}
		if vals != nil {
			ki.Type, ki.TTL, ki.Memory = vals[i*3].Str, vals[i*3+1].Int, vals[i*3+2].Int
		}
		if ki.Type == "none" {
			continue // expired between SCAN and TYPE
		}
		page.Keys = append(page.Keys, ki)
	}
	return page, nil
}

// errOutsideProject guards per-key actions to the project's own keys.
var errOutsideProject = errors.New("that key isn't under this project's prefix")

func (a *App) projectKeyCheck(site, key string) (redis.ProjectRedis, error) {
	set, err := a.ProjectRedisSettings(site)
	if err != nil {
		return set, err
	}
	if !strings.HasPrefix(key, set.Prefix) {
		return set, errOutsideProject
	}
	return set, nil
}

// ProjectKeyPreview reads a bounded preview of one of the project's
// keys.
func (a *App) ProjectKeyPreview(site string, db int, key string) (redis.Preview, error) {
	if _, err := a.projectKeyCheck(site, key); err != nil {
		return redis.Preview{}, err
	}
	c, err := a.redisConn()
	if err != nil {
		return redis.Preview{}, err
	}
	defer c.Close()
	if err := c.Select(db); err != nil {
		return redis.Preview{}, err
	}
	return c.GetPreview(key, 200, 64<<10)
}

// DeleteProjectKey removes one of the project's keys.
func (a *App) DeleteProjectKey(site string, db int, key string) error {
	if _, err := a.projectKeyCheck(site, key); err != nil {
		return err
	}
	c, err := a.redisConn()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Select(db); err != nil {
		return err
	}
	_, err = c.Unlink(key)
	return err
}

// FlushProjectCache deletes the project's cache entries — keys starting
// with its Redis prefix + cache prefix in its cache database — by SCAN
// + UNLINK, leaving sessions, queues and other projects alone. Returns
// how many keys were deleted.
func (a *App) FlushProjectCache(site string) (int64, error) {
	set, err := a.ProjectRedisSettings(site)
	if err != nil {
		return 0, err
	}
	full := set.Prefix + set.CachePrefix
	if full == "" {
		return 0, errors.New("this project's cache keys have no prefix, so they can't be told apart from other keys — flush its database from the Databases → Redis tab instead")
	}
	c, err := a.redisConn()
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if err := c.Select(set.CacheDB); err != nil {
		return 0, err
	}
	return c.DeleteMatching(redis.GlobEscape(full) + "*")
}

// ProjectEnvPlan is what "Use Redis in this project" would change in
// the site's .env (keys narrows it to those keys; nil = all), and the
// file's current text.
func (a *App) ProjectEnvPlan(site string, keys []string) ([]redis.EnvChange, string, error) {
	dir, err := a.siteDir(site)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	set := redis.ProjectSettings(dir, redis.Port)
	want := redis.LaravelEnv(redis.Port, redis.Password, set.LegacyCacheKey)
	plan := redis.PlanEnv(string(data), want)
	if keys != nil {
		allowed := map[string]bool{}
		for _, k := range keys {
			allowed[k] = true
		}
		var picked []redis.EnvChange
		for _, c := range plan {
			if allowed[c.Key] {
				picked = append(picked, c)
			}
		}
		plan = picked
	}
	return plan, string(data), nil
}

// UseRedisInProject applies the plan to the site's .env: the current
// file is first copied to .env.mullion-backup. Returns the backup path
// ("" when the .env didn't exist).
func (a *App) UseRedisInProject(site string, keys []string) (string, error) {
	plan, text, err := a.ProjectEnvPlan(site, keys)
	if err != nil {
		return "", err
	}
	if len(plan) == 0 {
		return "", nil
	}
	dir, _ := a.siteDir(site)
	envPath := filepath.Join(dir, ".env")
	backup := ""
	mode := os.FileMode(0o644)
	if st, err := os.Stat(envPath); err == nil {
		mode = st.Mode().Perm()
		backup = envPath + ".mullion-backup"
		if err := os.WriteFile(backup, []byte(text), mode); err != nil {
			return "", fmt.Errorf("backing up .env: %w", err)
		}
	}
	if err := os.WriteFile(envPath, []byte(redis.ApplyEnv(text, plan)), mode); err != nil {
		return backup, err
	}
	return backup, nil
}
