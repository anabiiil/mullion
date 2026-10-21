package cmd

import (
	"strings"
	"testing"

	"pm/internal/mssql"
)

func TestMSSQLStatusLine(t *testing.T) {
	up := mssqlStatusLine("2025", 1433, true, false, "darwin", mssql.RuntimeStatus{})
	if !strings.Contains(up, "port 1433") || !strings.Contains(up, "running") {
		t.Errorf("running line: %q", up)
	}
	cases := []struct {
		rt   mssql.RuntimeStatus
		want string
	}{
		{mssql.RuntimeStatus{}, "OrbStack is not installed"},
		{mssql.RuntimeStatus{OrbStack: true, Docker: true}, "OrbStack is not running"},
		{mssql.RuntimeStatus{OrbStack: true, Docker: true, Daemon: true}, "OrbStack is up"},
	}
	for _, c := range cases {
		line := mssqlStatusLine("2025", 14330, false, false, "darwin", c.rt)
		if !strings.Contains(line, "stopped") || !strings.Contains(line, "port 14330") || !strings.Contains(line, c.want) {
			t.Errorf("%+v: %q, want %q", c.rt, line, c.want)
		}
	}
	win := mssqlStatusLine("2025", 1433, false, true, "windows", mssql.RuntimeStatus{})
	if strings.Contains(win, "OrbStack") || !strings.Contains(win, "by you") {
		t.Errorf("windows line: %q", win)
	}
}

func TestMSSQLEngineHelpers(t *testing.T) {
	if engineLabel("mssql") != "SQL Server" {
		t.Error(engineLabel("mssql"))
	}
	if h := mssqlLeftoverHint(); !strings.Contains(h, mssql.ContainerName) && !strings.Contains(h, mssql.InstanceName) {
		t.Error(h)
	}
}
