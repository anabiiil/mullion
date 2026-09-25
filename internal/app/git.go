package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"pm/internal/gitops"
)

// The Git tab's operations: each resolves a site name to its folder and
// hands off to gitops, which holds all the logic.

// GitDir is the folder a site's git commands run in.
func (a *App) GitDir(site string) (string, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return "", fmt.Errorf("no site named %q", site)
	}
	return s.Path, nil
}

func (a *App) GitStatus(ctx context.Context, site string) (gitops.RepoStatus, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return gitops.RepoStatus{}, err
	}
	return gitops.Status(ctx, dir)
}

func (a *App) GitDiff(ctx context.Context, site, path string, staged bool) (gitops.DiffResult, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return gitops.DiffResult{}, err
	}
	return gitops.Diff(ctx, dir, path, staged)
}

func (a *App) GitShow(ctx context.Context, site, hash string) (gitops.DiffResult, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return gitops.DiffResult{}, err
	}
	return gitops.Show(ctx, dir, hash)
}

func (a *App) GitStage(ctx context.Context, site string, paths []string, all bool) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	if all {
		return gitops.StageAll(ctx, dir)
	}
	return gitops.Stage(ctx, dir, paths)
}

func (a *App) GitUnstage(ctx context.Context, site string, paths []string, all bool) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	if all {
		return gitops.UnstageAll(ctx, dir)
	}
	return gitops.Unstage(ctx, dir, paths)
}

func (a *App) GitDiscard(ctx context.Context, site string, paths []string) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Discard(ctx, dir, paths)
}

func (a *App) GitCommit(ctx context.Context, site, message string, amend, all bool) (string, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return "", err
	}
	return gitops.Commit(ctx, dir, message, amend, all)
}

func (a *App) GitPull(ctx context.Context, site, mode string, onOutput func([]byte)) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Pull(ctx, dir, mode, onOutput)
}

func (a *App) GitPush(ctx context.Context, site string, setUpstream, force bool, onOutput func([]byte)) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Push(ctx, dir, setUpstream, force, onOutput)
}

func (a *App) GitFetch(ctx context.Context, site string, onOutput func([]byte)) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Fetch(ctx, dir, onOutput)
}

func (a *App) GitBranches(ctx context.Context, site string) ([]gitops.Branch, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return nil, err
	}
	return gitops.Branches(ctx, dir)
}

func (a *App) GitCheckout(ctx context.Context, site, branch string, create bool) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Checkout(ctx, dir, branch, create)
}

func (a *App) GitDeleteBranch(ctx context.Context, site, name string, force bool) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.DeleteBranch(ctx, dir, name, force)
}

func (a *App) GitRebase(ctx context.Context, site, onto string, onOutput func([]byte)) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Rebase(ctx, dir, onto, onOutput)
}

// GitSequencer continues / skips / aborts an in-progress rebase, merge
// or cherry-pick: op is one of rebase-continue, rebase-skip,
// rebase-abort, merge-continue, merge-abort, cherry-pick-abort.
func (a *App) GitSequencer(ctx context.Context, site, op string) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	fn := map[string]func(context.Context, string) error{
		"rebase-continue":   gitops.RebaseContinue,
		"rebase-skip":       gitops.RebaseSkip,
		"rebase-abort":      gitops.RebaseAbort,
		"merge-continue":    gitops.MergeContinue,
		"merge-abort":       gitops.MergeAbort,
		"cherry-pick-abort": gitops.CherryPickAbort,
	}[op]
	if fn == nil {
		return fmt.Errorf("unknown operation %q", op)
	}
	return fn(ctx, dir)
}

func (a *App) GitLog(ctx context.Context, site string, limit int) ([]gitops.LogEntry, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return nil, err
	}
	return gitops.Log(ctx, dir, limit)
}

func (a *App) GitStashList(ctx context.Context, site string) ([]gitops.Stash, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return nil, err
	}
	return gitops.StashList(ctx, dir)
}

// GitStash runs a stash action: push (message, includeUntracked), pop,
// apply or drop (index).
func (a *App) GitStash(ctx context.Context, site, action, message string, includeUntracked bool, index int) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	switch action {
	case "push":
		return gitops.StashPush(ctx, dir, message, includeUntracked)
	case "pop":
		return gitops.StashPop(ctx, dir, index)
	case "apply":
		return gitops.StashApply(ctx, dir, index)
	case "drop":
		return gitops.StashDrop(ctx, dir, index)
	}
	return fmt.Errorf("unknown stash action %q", action)
}

func (a *App) GitStashShow(ctx context.Context, site string, index int) (gitops.DiffResult, error) {
	dir, err := a.GitDir(site)
	if err != nil {
		return gitops.DiffResult{}, err
	}
	return gitops.StashShow(ctx, dir, index)
}

func (a *App) GitInit(ctx context.Context, site string) error {
	dir, err := a.GitDir(site)
	if err != nil {
		return err
	}
	return gitops.Init(ctx, dir)
}

/* ── Sites page: one cheap summary per card ─────────────────────── */

// gitSummaryTTL is how long a card's git summary is reused — the Sites
// page polls, and a status per site per poll would add up.
const gitSummaryTTL = 5 * time.Second

// gitSummaryTimeout bounds one card's `git status` (a huge repo or a
// slow network drive must not hold up the whole page).
const gitSummaryTimeout = 1500 * time.Millisecond

type gitSummaryEntry struct {
	path string
	at   time.Time
	sum  *gitops.Summary // nil: not a repository (or git failed)
}

var gitSummaries = struct {
	sync.Mutex
	m map[string]gitSummaryEntry
}{m: map[string]gitSummaryEntry{}}

// GitSummaries returns site name → git summary for every linked site in
// a repository (sites outside one are left out). Statuses run in
// parallel with a short timeout each and are cached for a few seconds.
func (a *App) GitSummaries(ctx context.Context) map[string]*gitops.Summary {
	out := map[string]*gitops.Summary{}
	if !gitops.Available() {
		return out
	}
	now := time.Now()
	type job struct{ name, path string }
	var todo []job
	gitSummaries.Lock()
	for _, s := range a.State.Sites {
		if e, ok := gitSummaries.m[s.Name]; ok && e.path == s.Path && now.Sub(e.at) < gitSummaryTTL {
			if e.sum != nil {
				out[s.Name] = e.sum
			}
			continue
		}
		todo = append(todo, job{s.Name, s.Path})
	}
	gitSummaries.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, j := range todo {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, gitSummaryTimeout)
			defer cancel()
			sum, ok, err := gitops.QuickStatus(c, j.path)
			var p *gitops.Summary
			if ok && err == nil {
				p = &sum
			}
			mu.Lock()
			if p != nil {
				out[j.name] = p
			}
			mu.Unlock()
			// A timeout isn't cached as "not a repo" for long: retry next time.
			if err == nil {
				gitSummaries.Lock()
				gitSummaries.m[j.name] = gitSummaryEntry{path: j.path, at: time.Now(), sum: p}
				gitSummaries.Unlock()
			}
		}(j)
	}
	wg.Wait()
	return out
}

// ForgetGitSummary drops a site's cached summary (after a git action,
// so its card updates on the next poll).
func ForgetGitSummary(site string) {
	gitSummaries.Lock()
	delete(gitSummaries.m, site)
	gitSummaries.Unlock()
}
