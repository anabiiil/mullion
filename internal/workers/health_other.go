//go:build !windows

package workers

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psRow is one parsed line of `ps -A -o pid=,ppid=,pgid=,rss=,pcpu=`.
type psRow struct {
	pid, ppid, pgid int
	rssKB           int64
	pcpu            float64
}

// parsePS is the pure parsing half of SampleTrees, split out so it can
// be tested against a fixture string without shelling out.
func parsePS(out string) []psRow {
	var rows []psRow
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		rss, err4 := strconv.ParseInt(fields[3], 10, 64)
		cpu, err5 := strconv.ParseFloat(fields[4], 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			continue
		}
		rows = append(rows, psRow{pid: pid, ppid: ppid, pgid: pgid, rssKB: rss, pcpu: cpu})
	}
	return rows
}

// SampleTrees measures each root pid's whole process tree in one `ps`
// exec: a process is in a root's tree when its pgid matches the root's
// pgid (workers are session leaders, so their children share it) or it
// descends from the root via the ppid chain (covers a child that made
// its own session/group). Missing pids are absent from the result.
func SampleTrees(pids []int) map[int]Sample {
	result := map[int]Sample{}
	if len(pids) == 0 {
		return result
	}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,rss=,pcpu=").Output()
	if err != nil {
		return result
	}
	rows := parsePS(string(out))
	byPid := make(map[int]psRow, len(rows))
	for _, r := range rows {
		byPid[r.pid] = r
	}
	now := time.Now().Unix()

	for _, root := range pids {
		rootRow, ok := byPid[root]
		if !ok {
			continue
		}
		included := map[int]bool{root: true}
		for _, r := range rows {
			if r.pgid == rootRow.pgid {
				included[r.pid] = true
			}
		}
		// Fixed point over ppid edges, in case a descendant started its
		// own session/group and so didn't share the root's pgid.
		for changed := true; changed; {
			changed = false
			for _, r := range rows {
				if included[r.ppid] && !included[r.pid] {
					included[r.pid] = true
					changed = true
				}
			}
		}

		var rssKB int64
		var cpu float64
		var n int
		for pid := range included {
			r, ok := byPid[pid]
			if !ok {
				continue
			}
			rssKB += r.rssKB
			cpu += r.pcpu
			n++
		}
		result[root] = Sample{T: now, RSS: rssKB * 1024, CPU: cpu, Procs: n}
	}
	return result
}
