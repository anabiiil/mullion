package frankenphp

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// A site's server is checked, stopped and started by whichever Mullion
// process applies the state (UI jobs, the CLI, the agent, the idle
// watcher), so Ensure and Stop take a per-site lock that works across
// processes: a file created with O_EXCL holding its owner's pid and a
// random token.
const (
	// lockStale is when a lock left by a crashed holder is broken. Ensure
	// holds it for about a minute at worst (stop grace, port wait, ready
	// timeout).
	lockStale = 2 * time.Minute
	// lockWait is how long a caller waits for the lock before giving up.
	lockWait = lockStale + 30*time.Second
)

func lockPath(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.PidsDir(), "franken-"+site+".lock")
}

// siteLock is a held per-site lock.
type siteLock struct {
	path, token string
}

// lockSite takes the site's lock, breaking one whose owner died or that
// is older than lockStale.
func lockSite(paths pmdir.Paths, site string) (*siteLock, error) {
	path := lockPath(paths, site)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	token := fmt.Sprintf("%d %x", os.Getpid(), rand.Uint64())
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.WriteString(token)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return &siteLock{path: path, token: token}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		breakStaleLock(path)
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s's FrankenPHP server is busy (another Mullion process holds %s)", site, path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// breakStaleLock removes the lock file when its holder is gone or it is
// older than lockStale. The lock is renamed aside first, so of several
// processes that find it stale only one takes it; should that one have
// caught a fresh lock taken in between, it puts the lock back.
func breakStaleLock(path string) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	held := readString(path)
	pid, _ := strconv.Atoi(strings.Fields(held + " 0")[0])
	if time.Since(st.ModTime()) < lockStale && (held == "" || pid <= 0 || processAlive(pid)) {
		// Empty: being written this instant by its new owner.
		return
	}
	aside := fmt.Sprintf("%s.stale-%x", path, rand.Uint64())
	if os.Rename(path, aside) != nil {
		return // someone else broke it
	}
	if readString(aside) != held {
		_ = os.Link(aside, path) // fails if yet another lock exists: fine
	}
	_ = os.Remove(aside)
}

// unlock releases the lock if it is still ours.
func (l *siteLock) unlock() {
	if l != nil && readString(l.path) == l.token {
		_ = os.Remove(l.path)
	}
}
