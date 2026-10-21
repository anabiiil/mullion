package config

import (
	"testing"
	"time"
)

func TestDevIdle(t *testing.T) {
	m := time.Minute
	cases := []struct {
		name          string
		c             Config
		s             Site
		noTab, withTb time.Duration
		never         bool
	}{
		{"defaults", Config{}, Site{}, 2 * m, 10 * m, false},
		{"global", Config{DevIdleNoTabMin: 5, DevIdleWithTabMin: 30}, Site{}, 5 * m, 30 * m, false},
		{"global partial", Config{DevIdleWithTabMin: 60}, Site{}, 2 * m, 60 * m, false},
		{"global never", Config{DevNeverSleep: true}, Site{}, 2 * m, 10 * m, true},
		{"site never", Config{}, Site{DevIdleNever: true}, 2 * m, 10 * m, true},
		{"site custom beats global never", Config{DevNeverSleep: true, DevIdleNoTabMin: 7}, Site{DevIdleWithTabMin: 45}, 7 * m, 45 * m, false},
		{"site custom", Config{DevIdleNoTabMin: 5, DevIdleWithTabMin: 30}, Site{DevIdleNoTabMin: 1}, 1 * m, 30 * m, false},
		{"capped", Config{DevIdleNoTabMin: 99999}, Site{}, MaxDevIdleMin * m, 10 * m, false},
	}
	for _, c := range cases {
		noTab, withTab, never := c.c.DevIdle(c.s)
		if noTab != c.noTab || withTab != c.withTb || never != c.never {
			t.Errorf("%s: got %v %v %v, want %v %v %v", c.name, noTab, withTab, never, c.noTab, c.withTb, c.never)
		}
	}
}
