package phpver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// fakeUpdateSources points Updates at a fake build listing and a fake
// php.net security feed, restoring the real ones afterwards.
func fakeUpdateSources(t *testing.T, latest map[string]Release, listErr *error) (*atomic.Int32, *atomic.Int32) {
	t.Helper()
	var listHits, feedHits atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feedHits.Add(1)
		switch r.URL.Query().Get("version") {
		case "8.4":
			fmt.Fprint(w, `{"8.4.27":{"tags":[]},"8.4.26":{"tags":["security"]},"8.4.25":{"tags":[""]},"8.4.23":{"tags":["security"]}}`)
		case "8.3":
			fmt.Fprint(w, `{"8.3.21":{"tags":[]},"8.3.20":{"tags":["security"]}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(feed.Close)
	oldFn, oldURL, oldNow := latestBuildsFn, securityFeedURL, updatesNow
	latestBuildsFn = func(ctx context.Context, branches []string) (map[string]Release, error) {
		listHits.Add(1)
		if listErr != nil && *listErr != nil {
			return nil, *listErr
		}
		out := map[string]Release{}
		for _, b := range branches {
			if r, ok := latest[b]; ok {
				out[b] = r
			}
		}
		return out, nil
	}
	securityFeedURL = func(branch string) string { return feed.URL + "/?json&version=" + branch }
	t.Cleanup(func() { latestBuildsFn, securityFeedURL, updatesNow = oldFn, oldURL, oldNow })
	return &listHits, &feedHits
}

func TestUpdatesPerBranch(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	fakeUpdateSources(t, map[string]Release{
		"8.4": {Version: "8.4.27"},
		"8.3": {Version: "8.3.21"},
	}, nil)

	got, err := Updates(context.Background(), paths, []string{"8.4.23", "8.4.27", "8.3.21", "8.2.1", "8.4.26"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Update{
		"8.4.23": {From: "8.4.23", To: "8.4.27", Security: true},  // 8.4.26 was a security release
		"8.4.26": {From: "8.4.26", To: "8.4.27", Security: false}, // only 8.4.27 since: not security
	}
	if len(got) != len(want) {
		t.Fatalf("Updates = %+v, want %+v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("Updates[%s] = %+v, want %+v", k, got[k], w)
		}
	}
}

func TestUpdatesCacheAndOffline(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	var listErr error
	hits, _ := fakeUpdateSources(t, map[string]Release{"8.4": {Version: "8.4.27"}}, &listErr)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	updatesNow = func() time.Time { return now }
	ctx := context.Background()

	if _, err := Updates(ctx, paths, []string{"8.4.23"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Updates(ctx, paths, []string{"8.4.23"}, false); err != nil || hits.Load() != 1 {
		t.Fatalf("second check within 6h hit the listing (hits=%d, err=%v)", hits.Load(), err)
	}
	if UpdatesCheckedAt(paths) != now {
		t.Errorf("UpdatesCheckedAt = %v, want %v", UpdatesCheckedAt(paths), now)
	}
	// A branch the cache never looked up forces a fetch.
	if _, err := Updates(ctx, paths, []string{"8.4.23", "8.3.1"}, false); err != nil || hits.Load() != 2 {
		t.Fatalf("new branch did not refetch (hits=%d)", hits.Load())
	}
	if _, err := Updates(ctx, paths, []string{"8.4.23"}, true); err != nil || hits.Load() != 3 {
		t.Fatalf("force did not refetch (hits=%d)", hits.Load())
	}
	now = now.Add(7 * time.Hour)
	if _, err := Updates(ctx, paths, []string{"8.4.23"}, false); err != nil || hits.Load() != 4 {
		t.Fatalf("expired cache did not refetch (hits=%d)", hits.Load())
	}

	// Offline: the last answer is still served, with the error.
	listErr = errors.New("no network")
	now = now.Add(7 * time.Hour)
	got, err := Updates(ctx, paths, []string{"8.4.23"}, false)
	if err == nil || got["8.4.23"].To != "8.4.27" {
		t.Fatalf("offline with a cache: got %+v, %v; want the cached update and an error", got, err)
	}
	// Offline with no cache at all: an error, no updates.
	got, err = Updates(ctx, pmdir.Paths{Home: t.TempDir()}, []string{"8.4.23"}, false)
	if err == nil || len(got) != 0 {
		t.Fatalf("offline without cache: got %+v, %v", got, err)
	}
	// Nothing installed: nothing to ask.
	if got, err := Updates(ctx, paths, nil, true); err != nil || len(got) != 0 {
		t.Fatalf("no versions: %+v, %v", got, err)
	}
}

func TestParseSecurityFeedShapes(t *testing.T) {
	many, err := parseSecurityFeed(strings.NewReader(`{"8.4.27":{"tags":["security"]},"8.4.25":{"tags":[""]},"8.4.24":{"tags":["Security"]}}`))
	if err != nil || strings.Join(many, ",") != "8.4.24,8.4.27" {
		t.Fatalf("map shape = %v, %v", many, err)
	}
	one, err := parseSecurityFeed(strings.NewReader(`{"announcement":true,"tags":["security"],"version":"8.4.27"}`))
	if err != nil || strings.Join(one, ",") != "8.4.27" {
		t.Fatalf("single shape = %v, %v", one, err)
	}
	if _, err := parseSecurityFeed(strings.NewReader(`not json`)); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestSecurityBetween(t *testing.T) {
	secs := []string{"8.4.10", "8.4.24"}
	cases := []struct {
		from, to string
		want     bool
	}{
		{"8.4.23", "8.4.27", true},
		{"8.4.24", "8.4.27", false}, // already on the security release
		{"8.4.9", "8.4.10", true},
		{"8.4.11", "8.4.23", false},
	}
	for _, c := range cases {
		if got := securityBetween(secs, c.from, c.to); got != c.want {
			t.Errorf("securityBetween(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestBranch(t *testing.T) {
	for in, want := range map[string]string{"8.4.23": "8.4", "7.4.33": "7.4", "8": "", "x": ""} {
		if got := Branch(in); got != want {
			t.Errorf("Branch(%q) = %q, want %q", in, got, want)
		}
	}
}
