//go:build windows

package pty

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ConPTY lives in kernel32 on Windows 10 1809+; there is no x/sys here,
// so the few calls we need are bound lazily. Start fails cleanly on
// older systems where the procs are missing.
var (
	kernel32                          = syscall.NewLazyDLL("kernel32.dll")
	procCreatePseudoConsole           = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole           = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole            = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcThreadAttrList  = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute     = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttributeList = kernel32.NewProc("DeleteProcThreadAttributeList")
)

const (
	extendedStartupInfoPresent       = 0x00080000
	createUnicodeEnvironment         = 0x00000400
	procThreadAttributePseudoConsole = 0x00020016
	startfUseStdHandles              = 0x00000100
)

type startupInfoEx struct {
	syscall.StartupInfo
	attributeList uintptr
}

// PTY is a program attached to a Windows pseudo console (ConPTY).
type PTY struct {
	hpc     uintptr
	in      *os.File // we write keystrokes here
	out     *os.File // the console's rendered VT output
	process syscall.Handle
	pid     int
	attrs   []byte // proc thread attribute list, kept alive with the process

	closeOnce sync.Once
	waitOnce  sync.Once
	waitCode  int
	waitErr   error
	done      chan struct{}
}

// DefaultShell is PowerShell 7 (pwsh) when installed, else Windows
// PowerShell, without the copyright banner.
func DefaultShell() (string, []string) {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p, []string{"-NoLogo"}
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p, []string{"-NoLogo"}
	}
	return "cmd.exe", nil
}

func coord(cols, rows uint16) uintptr { return uintptr(cols) | uintptr(rows)<<16 }

// Start runs cmd with args in dir under a new pseudo console.
func Start(cmd string, args []string, dir string, env []string, cols, rows uint16) (*PTY, error) {
	if err := procCreatePseudoConsole.Find(); err != nil {
		return nil, errors.New("pty: this Windows version has no ConPTY (needs Windows 10 1809 or later)")
	}
	cols, rows = clampSize(cols, rows)

	var inRead, inWrite, outRead, outWrite syscall.Handle
	if err := syscall.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := syscall.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		syscall.CloseHandle(inRead)
		syscall.CloseHandle(inWrite)
		return nil, err
	}

	var hpc uintptr
	r, _, _ := procCreatePseudoConsole.Call(coord(cols, rows), uintptr(inRead), uintptr(outWrite), 0, uintptr(unsafe.Pointer(&hpc)))
	// The console duplicated its ends; ours must go or reads never see EOF.
	syscall.CloseHandle(inRead)
	syscall.CloseHandle(outWrite)
	if r != 0 { // HRESULT S_OK == 0
		syscall.CloseHandle(inWrite)
		syscall.CloseHandle(outRead)
		return nil, syscall.Errno(r)
	}
	p := &PTY{
		hpc:  hpc,
		in:   os.NewFile(uintptr(inWrite), "conpty-in"),
		out:  os.NewFile(uintptr(outRead), "conpty-out"),
		done: make(chan struct{}),
	}
	if err := p.spawn(cmd, args, dir, env); err != nil {
		procClosePseudoConsole.Call(hpc)
		p.in.Close()
		p.out.Close()
		return nil, err
	}
	return p, nil
}

func (p *PTY) spawn(cmd string, args []string, dir string, env []string) error {
	var size uintptr
	procInitializeProcThreadAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return errors.New("pty: InitializeProcThreadAttributeList sizing failed")
	}
	p.attrs = make([]byte, size)
	list := uintptr(unsafe.Pointer(&p.attrs[0]))
	if r, _, err := procInitializeProcThreadAttrList.Call(list, 1, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		return err
	}
	// The attribute's value is the HPCON itself (not a pointer to it).
	if r, _, err := procUpdateProcThreadAttribute.Call(list, 0, procThreadAttributePseudoConsole,
		p.hpc, unsafe.Sizeof(p.hpc), 0, 0); r == 0 {
		procDeleteProcThreadAttributeList.Call(list)
		return err
	}

	var si startupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	// Null std handles: without this a child of a panel whose own stdio
	// is redirected would write there instead of into the pseudo console.
	si.Flags = startfUseStdHandles
	si.attributeList = list

	if path, err := exec.LookPath(cmd); err == nil {
		cmd = path
	}
	line := syscall.EscapeArg(cmd)
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	cmdLine, err := syscall.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = syscall.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	if env == nil {
		env = os.Environ()
	}
	env = setEnv(env, "TERM", "xterm-256color")
	env = setEnv(env, "COLORTERM", "truecolor")
	block := envBlock(env)

	var pi syscall.ProcessInformation
	err = syscall.CreateProcess(nil, cmdLine, nil, nil, false,
		extendedStartupInfoPresent|createUnicodeEnvironment,
		&block[0], dirPtr, (*syscall.StartupInfo)(unsafe.Pointer(&si)), &pi)
	procDeleteProcThreadAttributeList.Call(list)
	if err != nil {
		return err
	}
	syscall.CloseHandle(pi.Thread)
	p.process = pi.Process
	p.pid = int(pi.ProcessId)
	return nil
}

// Pid of the program running in the pseudo console.
func (p *PTY) Pid() int { return p.pid }

func (p *PTY) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if err != nil && errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
		err = os.ErrClosed
	}
	return n, err
}

func (p *PTY) Write(b []byte) (int, error) { return p.in.Write(b) }

// Resize changes the pseudo console's size.
func (p *PTY) Resize(cols, rows uint16) error {
	cols, rows = clampSize(cols, rows)
	if r, _, _ := procResizePseudoConsole.Call(p.hpc, coord(cols, rows)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// Close tears the pseudo console down, which ends the shell (and every
// console program attached to it). ClosePseudoConsole can block until
// its output is drained on older Windows builds, so it runs while the
// output pipe is still being read, and is given a bounded time.
func (p *PTY) Close() error {
	p.closeOnce.Do(func() {
		go p.Wait()
		p.in.Close()
		closed := make(chan struct{})
		go func() {
			procClosePseudoConsole.Call(p.hpc)
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			syscall.TerminateProcess(p.process, 1)
		}
		p.out.Close()
	})
	return nil
}

// Wait blocks until the program exits and returns its exit code.
func (p *PTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		_, err := syscall.WaitForSingleObject(p.process, syscall.INFINITE)
		var code uint32
		if err == nil {
			err = syscall.GetExitCodeProcess(p.process, &code)
		}
		p.waitCode, p.waitErr = int(code), err
		syscall.CloseHandle(p.process)
		close(p.done)
	})
	<-p.done
	return p.waitCode, p.waitErr
}

// Cwd can't be read from another process on Windows without heavy
// machinery (PEB reads); callers track the directory themselves.
func Cwd(pid int) (string, error) { return "", ErrUnsupported }

func setEnv(env []string, key, val string) []string {
	prefix := strings.ToUpper(key) + "="
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+val)
}

// envBlock builds a CREATE_UNICODE_ENVIRONMENT block: sorted
// "k=v\0" entries followed by a final \0.
func envBlock(env []string) []uint16 {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return strings.ToUpper(sorted[i]) < strings.ToUpper(sorted[j]) })
	var b []uint16
	for _, kv := range sorted {
		if strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}
