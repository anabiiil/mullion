//go:build windows

package workers

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// th32csSnapProcess flags CreateToolhelp32Snapshot to enumerate every
// process (TH32CS_SNAPPROCESS).
const th32csSnapProcess = 0x00000002

// processMemoryCounters mirrors enough of PROCESS_MEMORY_COUNTERS
// (psapi.h) for GetProcessMemoryInfo to fill in.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	psapiDLL                 = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = psapiDLL.NewProc("GetProcessMemoryInfo")
)

// workingSetSize is GetProcessMemoryInfo's WorkingSetSize for h (our
// stand-in for RSS on Windows).
func workingSetSize(h syscall.Handle) (uintptr, bool) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	return c.WorkingSetSize, r != 0
}

// cpuSnapshot is a pid's cumulative CPU time at a point in time, kept
// so the next sample can turn it into a percentage.
type cpuSnapshot struct {
	at    time.Time
	ns100 uint64 // kernel+user time, 100ns units
}

var (
	cpuMu   sync.Mutex
	cpuPrev = map[int]cpuSnapshot{}
)

func filetimeTo100ns(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// cpuPercent is pid's CPU% of one core since its previous sample (0 on
// the first sample we have for it).
func cpuPercent(pid int, h syscall.Handle, now time.Time) float64 {
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	cur := filetimeTo100ns(kernel) + filetimeTo100ns(user)

	cpuMu.Lock()
	prev, ok := cpuPrev[pid]
	cpuPrev[pid] = cpuSnapshot{at: now, ns100: cur}
	cpuMu.Unlock()

	if !ok || !now.After(prev.at) || cur < prev.ns100 {
		return 0
	}
	deltaCPUns := float64(cur-prev.ns100) * 100 // 100ns ticks -> ns
	deltaWallns := float64(now.Sub(prev.at).Nanoseconds())
	if deltaWallns <= 0 {
		return 0
	}
	return deltaCPUns / deltaWallns * 100
}

// pruneDeadPids drops cached CPU-time snapshots for pids no longer seen
// in the latest snapshot, so a long-dead worker doesn't leak memory.
func pruneDeadPids(live map[int]bool) {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	for pid := range cpuPrev {
		if !live[pid] {
			delete(cpuPrev, pid)
		}
	}
}

// SampleTrees measures each root pid's whole process tree in one pass:
// a single Toolhelp32 snapshot gives every pid's parent, then each
// root's descendants are walked by parent pid (Windows worker children
// are cmd.exe /C's process tree, found this way since there's no pgid).
// Missing pids are absent from the result.
func SampleTrees(pids []int) map[int]Sample {
	result := map[int]Sample{}
	if len(pids) == 0 {
		return result
	}
	snap, err := syscall.CreateToolhelp32Snapshot(th32csSnapProcess, 0)
	if err != nil {
		return result
	}
	defer syscall.CloseHandle(snap)

	children := map[int][]int{}
	exists := map[int]bool{}
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for perr := syscall.Process32First(snap, &entry); perr == nil; perr = syscall.Process32Next(snap, &entry) {
		pid := int(entry.ProcessID)
		ppid := int(entry.ParentProcessID)
		exists[pid] = true
		children[ppid] = append(children[ppid], pid)
	}

	now := time.Now()
	live := map[int]bool{}
	for _, root := range pids {
		if !exists[root] {
			continue
		}
		tree := []int{root}
		for queue := []int{root}; len(queue) > 0; {
			p := queue[0]
			queue = queue[1:]
			for _, c := range children[p] {
				tree = append(tree, c)
				queue = append(queue, c)
			}
		}

		var rss uintptr
		var cpu float64
		var n int
		for _, pid := range tree {
			h, oerr := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
			if oerr != nil {
				continue
			}
			if ws, ok := workingSetSize(h); ok {
				rss += ws
			}
			cpu += cpuPercent(pid, h, now)
			syscall.CloseHandle(h)
			n++
			live[pid] = true
		}
		result[root] = Sample{T: now.Unix(), RSS: int64(rss), CPU: cpu, Procs: n}
	}
	pruneDeadPids(live)
	return result
}
