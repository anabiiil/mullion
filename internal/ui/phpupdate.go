package ui

// PHP patch updates on the PHP page: which installed versions have a
// newer patch (cached 6 h by phpver.Updates), what updating one would
// switch, and the update itself as a streaming job.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"pm/internal/app"
	"pm/internal/phpver"
)

func registerPHPUpdate(api apiRegistrar) {
	api("/api/php/updates", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Force bool }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		ups, err := a.PHPUpdates(ctx, in.Force)
		out := map[string]any{"updates": ups, "error": ""}
		if ups == nil {
			out["updates"] = map[string]phpver.Update{}
		}
		if err != nil {
			out["error"] = err.Error()
		}
		if t := phpver.UpdatesCheckedAt(a.Paths); !t.IsZero() {
			out["checkedAt"] = t.UnixMilli()
		}
		return out, nil
	})
	api("/api/php/update/plan", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Versions []string }
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		ups, err := a.PHPUpdates(r.Context(), false)
		for _, v := range in.Versions {
			if _, ok := ups[v]; !ok {
				// The cached listing may predate a new release (the
				// install picker saw it): look again, for real.
				ups, err = a.PHPUpdates(r.Context(), true)
				break
			}
		}
		if err != nil && len(ups) == 0 {
			return nil, err
		}
		plans := []app.PHPUpdatePlan{}
		for _, v := range in.Versions {
			u, ok := ups[v]
			if !ok {
				return nil, fmt.Errorf("PHP %s has no newer patch release", v)
			}
			p := a.PlanPHPUpdate(v, u.To)
			p.Security = u.Security
			plans = append(plans, p)
		}
		return plans, nil
	})
	api("/api/php/update", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Versions []string
			KeepOld  bool
		}
		if err := decodeJSON(r, &in); err != nil {
			return nil, err
		}
		if len(in.Versions) == 0 {
			return nil, errors.New("no PHP version to update")
		}
		versions := append([]string(nil), in.Versions...)
		sort.Slice(versions, func(i, j int) bool { return phpver.Compare(versions[i], versions[j]) < 0 })
		if !jobs.installMu.TryLock() {
			return nil, errors.New("another install is already in progress — wait for it to finish and try again")
		}
		id := jobs.startStream(func(setStatus func(string), appendLog func([]byte)) (any, error) {
			defer jobs.installMu.Unlock()
			a2, err := app.New()
			if err != nil {
				return nil, err
			}
			a2.Progress = setStatus
			opts := app.PHPUpdateOptions{
				KeepOld: in.KeepOld,
				Log: func(s string) {
					setStatus(s)
					appendLog([]byte("==> " + s + "\n"))
				},
			}
			var results []app.PHPUpdateResult
			for _, v := range versions {
				res, err := a2.UpdatePHP(context.Background(), v, opts)
				if errors.Is(err, app.ErrPHPUpToDate) {
					appendLog([]byte("PHP " + v + " is already the newest patch.\n"))
					continue
				}
				if err != nil {
					appendLog([]byte("error: " + err.Error() + "\n"))
					return results, err
				}
				for _, w := range res.Warnings {
					appendLog([]byte("warning: " + w + "\n"))
				}
				appendLog([]byte(fmt.Sprintf("PHP %s is now %s.\n", res.From, res.To)))
				results = append(results, res)
			}
			return results, nil
		})
		return map[string]string{"job": id}, nil
	})
}
