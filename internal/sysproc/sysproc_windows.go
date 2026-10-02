//go:build windows

package sysproc

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"pm/internal/proc"
)

// portOwner resolves which process listens on the port. It reads the TCP
// table straight from iphlpapi: the Ports page asks for a dozen ports, and
// a PowerShell round trip per port made it take several seconds.
func PortOwner(port int) (int, string) {
	for _, r := range tcpRows() {
		if r.state == mibTCPStateListen && r.localPort == port {
			name := processName(r.pid)
			if name == "" {
				name = "unknown"
			}
			return int(r.pid), name
		}
	}
	return 0, ""
}

const (
	mibTCPStateListen = 2
	mibTCPStateEstab  = 5
)

var getExtendedTCPTable = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

type tcpRow struct {
	state, pid uint32
	localPort  int
}

// tcpRows lists every IPv4 and IPv6 TCP endpoint with its owning pid.
func tcpRows() []tcpRow {
	const (
		afInet, afInet6       = 2, 23
		tcpTableOwnerPidAll   = 5
		errInsufficientBuffer = 122
	)
	var rows []tcpRow
	for _, af := range []uintptr{afInet, afInet6} {
		size := uint32(16 << 10)
		var buf []byte
		for {
			buf = make([]byte, size)
			r, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])),
				uintptr(unsafe.Pointer(&size)), 0, af, tcpTableOwnerPidAll, 0)
			if r == errInsufficientBuffer {
				continue // size now holds what the table needs
			}
			if r != 0 {
				buf = nil
			}
			break
		}
		if len(buf) < 4 {
			continue
		}
		n := *(*uint32)(unsafe.Pointer(&buf[0]))
		// MIB_TCPROW_OWNER_PID is 6 DWORDs; MIB_TCP6ROW_OWNER_PID is two
		// 16-byte addresses plus 6 DWORDs. Ports are in network byte order.
		rowSize, stateAt, portAt, pidAt := 24, 0, 8, 20
		if af == afInet6 {
			rowSize, stateAt, portAt, pidAt = 56, 48, 20, 52
		}
		dword := func(off int) uint32 { return *(*uint32)(unsafe.Pointer(&buf[off])) }
		for i := 0; i < int(n) && 4+(i+1)*rowSize <= len(buf); i++ {
			base := 4 + i*rowSize
			p := dword(base + portAt)
			rows = append(rows, tcpRow{
				state:     dword(base + stateAt),
				pid:       dword(base + pidAt),
				localPort: int(p&0xff)<<8 | int(p>>8&0xff),
			})
		}
	}
	return rows
}

// processName is the image name (e.g. caddy.exe) of a running process.
func processName(pid uint32) string {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if e.ProcessID == pid {
			return syscall.UTF16ToString(e.ExeFile[:])
		}
	}
	return ""
}

// killWithParent stops the process, and — for supervisor architectures
// like mysqld's monitor — also its parent when it's the same executable.
func KillWithParent(pid int) {
	script := fmt.Sprintf(`
$p = Get-Process -Id %d -ErrorAction SilentlyContinue
if ($p) {
  $parentId = (Get-CimInstance Win32_Process -Filter "ProcessId=%d").ParentProcessId
  Stop-Process -Id %d -Force -ErrorAction SilentlyContinue
  $par = Get-Process -Id $parentId -ErrorAction SilentlyContinue
  if ($par -and $par.Name -eq $p.Name) { Stop-Process -Id $parentId -Force -ErrorAction SilentlyContinue }
}`, pid, pid, pid)
	_ = proc.Quiet("powershell", "-NoProfile", "-Command", script).Run()
}

var queryFullProcessImageName = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

// processesUnder lists PIDs of running processes whose executable lives
// under dir (optionally restricted to one image name). This is how the
// uninstall distinguishes Mullion's servers from Laragon's.
//
// It asks for PROCESS_QUERY_LIMITED_INFORMATION only: that right is
// granted across integrity levels, so the servers an elevated `mullion
// setup` started still count as Mullion's from a normal prompt or the
// panel. (Win32_Process.ExecutablePath comes back empty for those, which
// made Mullion treat its own MySQL and Caddy as foreign.)
func ProcessesUnder(dir, image string) []int {
	prefix := strings.ToLower(strings.TrimRight(filepath.Clean(dir), `\`) + `\`)
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)

	var pids []int
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if e.ProcessID == 0 {
			continue
		}
		if image != "" && !strings.EqualFold(syscall.UTF16ToString(e.ExeFile[:]), image) {
			continue
		}
		if p := imagePath(e.ProcessID); p != "" && strings.HasPrefix(strings.ToLower(p), prefix) {
			pids = append(pids, int(e.ProcessID))
		}
	}
	return pids
}

// imagePath is the full executable path of a process, or "" when it
// cannot be read (protected system processes).
func imagePath(pid uint32) string {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	if r, _, _ := queryFullProcessImageName.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

// killProcess force-stops one process by pid.
func KillProcess(pid int) {
	_ = proc.Quiet("taskkill", "/F", "/PID", fmt.Sprint(pid)).Run()
}

// openURL opens a web page in the default browser.
func OpenURL(url string) {
	_ = proc.Quiet("cmd", "/c", "start", "", url).Start()
}

// scheduleHomeRemoval starts a detached helper that deletes the install
// directory once every Mullion process has exited — this very process
// still runs from bin, so it cannot delete it itself.
func ScheduleHomeRemoval(home string) error {
	script := fmt.Sprintf(`
$dir = '%s'
for ($i = 0; $i -lt 120; $i++) {
  $running = Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -like ($dir + '\*') }
  if (-not $running) { break }
  Start-Sleep -Seconds 1
}
Start-Sleep -Seconds 1
Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue`,
		strings.ReplaceAll(home, "'", "''"))
	cmd := proc.Quiet("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", script)
	proc.DetachHiddenConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("scheduling removal of %s: %w", home, err)
	}
	_ = cmd.Process.Release()
	return nil
}

// conflictExample names the usual suspect in the port-conflict warning.
const ConflictExample = " (e.g. quit Laragon)"

// printStackHint has nothing extra to add on Windows — killing the
// process is usually enough there.
func PrintStackHint(conflicts []Conflict) {}

// stopConflict stops a conflicting listener; on Windows killing the
// process (and its same-name parent) is what works.
func StopConflict(c Conflict) { KillWithParent(c.PID) }

// PortActive reports whether any ESTABLISHED TCP connection exists to
// the local port (an open browser tab keeps its websocket alive).
func PortActive(port int) bool {
	for _, r := range tcpRows() {
		if r.state == mibTCPStateEstab && r.localPort == port {
			return true
		}
	}
	return false
}
