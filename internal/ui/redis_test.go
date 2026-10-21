package ui

import (
	"testing"

	"pm/internal/redis"
)

func TestMonitorFilter(t *testing.T) {
	f := monitorFilter{dbs: map[int]bool{0: true, 1: true}, prefix: "shop_database_"}
	cases := []struct {
		ev   redis.MonitorEvent
		want bool
	}{
		{redis.MonitorEvent{DB: 1, Addr: "127.0.0.1:1", Args: []string{"GET", "shop_database_shop_cache_x"}}, true},
		{redis.MonitorEvent{DB: 0, Addr: "127.0.0.1:1", Args: []string{"RPUSH", "shop_database_queues:default", "{}"}}, true},
		{redis.MonitorEvent{DB: 0, Addr: "127.0.0.1:1", Args: []string{"GET", "other_database_x"}}, false},
		{redis.MonitorEvent{DB: 5, Addr: "127.0.0.1:1", Args: []string{"GET", "shop_database_x"}}, false},
		{redis.MonitorEvent{DB: 0, Addr: "127.0.0.1:1", Args: []string{"SELECT", "shop_database_"}}, false},
		{redis.MonitorEvent{DB: 1, Addr: "127.0.0.1:1", Args: []string{"FLUSHDB"}}, true},
		{redis.MonitorEvent{DB: 1, Addr: "lua", Args: []string{"del", "shop_database_k"}}, true},
		{redis.MonitorEvent{DB: 1, Addr: "127.0.0.1:1"}, false},
	}
	for i, c := range cases {
		if got := f.match(c.ev); got != c.want {
			t.Errorf("case %d (%v): match = %v, want %v", i, c.ev.Args, got, c.want)
		}
	}
	all := monitorFilter{dbs: map[int]bool{}}
	if !all.match(redis.MonitorEvent{DB: 9, Addr: "x", Args: []string{"PING"}}) {
		t.Error("an unfiltered monitor shows everything")
	}
}

func TestPanelText(t *testing.T) {
	if panelText("héllo") != "héllo" {
		t.Error("valid UTF-8 must pass through")
	}
	if got := panelText("a\xffb\x00"); got != `a\xffb`+"\x00" {
		t.Errorf("panelText = %q", got)
	}
	p := panelPreview(redis.Preview{Key: "k\xfe", Items: []string{"\xc3"}, Fields: []redis.Field{{Name: "\xff", Value: "v"}}})
	if p.Key != `k\xfe` || p.Items[0] != `\xc3` || p.Fields[0].Name != `\xff` {
		t.Errorf("panelPreview = %+v", p)
	}
}
