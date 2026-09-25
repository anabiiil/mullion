//go:build windows

// Package tray puts Mullion's icon in the Windows notification area
// (next to the clock) with a right-click menu for the common actions.
package tray

import (
	"os"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Handlers are the actions the tray menu triggers.
type Handlers struct {
	OpenDashboard func()
	StartServices func()
	StopServices  func()
	OpenPMA       func()
}

const (
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmApp         = 0x8000
	wmTrayMsg     = wmApp + 1
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	nifMessage    = 0x1
	nifIcon       = 0x2
	nifTip        = 0x4
	nimAdd        = 0x0
	nimDelete     = 0x2
	mfString      = 0x0
	mfSeparator   = 0x800
	tpmReturnCmd  = 0x100
	tpmNonotify   = 0x80

	cmdDashboard = 1
	cmdStart     = 2
	cmdStop      = 3
	cmdPMA       = 4
	cmdQuit      = 5
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	user32           = syscall.NewLazyDLL("user32.dll")
	shell32          = syscall.NewLazyDLL("shell32.dll")
	getModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	createMutex      = kernel32.NewProc("CreateMutexW")
	registerClassEx  = user32.NewProc("RegisterClassExW")
	createWindowEx   = user32.NewProc("CreateWindowExW")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	getMessage       = user32.NewProc("GetMessageW")
	translateMessage = user32.NewProc("TranslateMessage")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
	postQuitMessage  = user32.NewProc("PostQuitMessage")
	loadIcon         = user32.NewProc("LoadIconW")
	createPopupMenu  = user32.NewProc("CreatePopupMenu")
	appendMenu       = user32.NewProc("AppendMenuW")
	destroyMenu      = user32.NewProc("DestroyMenu")
	trackPopupMenu   = user32.NewProc("TrackPopupMenu")
	setForeground    = user32.NewProc("SetForegroundWindow")
	getCursorPos     = user32.NewProc("GetCursorPos")
	shellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	extractIconEx    = shell32.NewProc("ExtractIconExW")
)

type wndClassEx struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background syscall.Handle
	MenuName, ClassName                *uint16
	IconSm                             syscall.Handle
}

type notifyIconData struct {
	Size                       uint32
	Wnd                        syscall.Handle
	ID, Flags, CallbackMessage uint32
	Icon                       syscall.Handle
	Tip                        [128]uint16
	State, StateMask           uint32
	Info                       [256]uint16
	TimeoutVersion             uint32
	InfoTitle                  [64]uint16
	InfoFlags                  uint32
	GuidItem                   [16]byte
	BalloonIcon                syscall.Handle
}

type point struct{ X, Y int32 }

type msg struct {
	Wnd     syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// ErrAlreadyRunning means another tray instance owns the icon.
var ErrAlreadyRunning = fmt.Errorf("the Mullion tray is already running")

var handlers Handlers

// Run shows the tray icon and blocks in the message loop until the
// user picks Quit. One instance only, enforced with a named mutex.
func Run(h Handlers) error {
	runtime.LockOSThread()
	handlers = h

	// The last-error must come from the Call itself — the runtime makes
	// other syscalls in between that would reset GetLastError.
	name, _ := syscall.UTF16PtrFromString("Local\\MullionTray")
	handle, _, lastErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return fmt.Errorf("tray mutex: %v", lastErr)
	}
	if errno, ok := lastErr.(syscall.Errno); ok && errno == syscall.ERROR_ALREADY_EXISTS {
		fmt.Println("The Mullion tray icon is already running.")
		return ErrAlreadyRunning
	}

	hInst, _, _ := getModuleHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString("MullionTrayWnd")

	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(wndProc),
		Instance:  syscall.Handle(hInst),
		ClassName: className,
	}
	if atom, _, err := registerClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("registering the tray window class: %v", err)
	}

	title, _ := syscall.UTF16PtrFromString("Mullion")
	hwnd, _, err := createWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("creating the tray window: %v", err)
	}

	icon := ownIcon(hInst)

	nid := notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             syscall.Handle(hwnd),
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayMsg,
		Icon:            syscall.Handle(icon),
	}
	tip := syscall.StringToUTF16("Mullion — local dev server")
	copy(nid.Tip[:], tip)
	if ok, _, err := shellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ok == 0 {
		return fmt.Errorf("adding the tray icon: %v", err)
	}
	defer shellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

	var m msg
	for {
		r, _, gmErr := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) < 0 {
			return fmt.Errorf("tray message loop failed: %v", gmErr)
		}
		if int32(r) == 0 {
			return nil // WM_QUIT
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// ownIcon pulls the small icon straight out of this exe file — resource
// IDs vary with the tooling, so extracting by file is the reliable way.
func ownIcon(hInst uintptr) uintptr {
	if exe, err := os.Executable(); err == nil {
		if p, err := syscall.UTF16PtrFromString(exe); err == nil {
			var small syscall.Handle
			extractIconEx.Call(uintptr(unsafe.Pointer(p)), 0, 0,
				uintptr(unsafe.Pointer(&small)), 1)
			if small != 0 {
				return uintptr(small)
			}
		}
	}
	// Fall back to whatever icon resource the module exposes.
	if icon, _, _ := loadIcon.Call(hInst, 1); icon != 0 {
		return icon
	}
	const idiApplication = 32512
	icon, _, _ := loadIcon.Call(0, idiApplication)
	return icon
}

func wndProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTrayMsg:
		switch uint32(lParam) {
		case wmLButtonDbl:
			if handlers.OpenDashboard != nil {
				go handlers.OpenDashboard()
			}
		case wmRButtonUp:
			showMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch wParam & 0xffff {
		case cmdDashboard:
			if handlers.OpenDashboard != nil {
				go handlers.OpenDashboard()
			}
		case cmdStart:
			if handlers.StartServices != nil {
				go handlers.StartServices()
			}
		case cmdStop:
			if handlers.StopServices != nil {
				go handlers.StopServices()
			}
		case cmdPMA:
			if handlers.OpenPMA != nil {
				go handlers.OpenPMA()
			}
		case cmdQuit:
			postQuitMessage.Call(0)
		}
		return 0
	case wmClose, wmDestroy:
		postQuitMessage.Call(0)
		return 0
	}
	r, _, _ := defWindowProc.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}

func showMenu(hwnd syscall.Handle) {
	menu, _, _ := createPopupMenu.Call()
	if menu == 0 {
		return
	}
	defer destroyMenu.Call(menu)

	add := func(id uintptr, text string) {
		t, _ := syscall.UTF16PtrFromString(text)
		appendMenu.Call(menu, mfString, id, uintptr(unsafe.Pointer(t)))
	}
	add(cmdDashboard, "Open Dashboard")
	appendMenu.Call(menu, mfSeparator, 0, 0)
	add(cmdStart, "Start services")
	add(cmdStop, "Stop services")
	appendMenu.Call(menu, mfSeparator, 0, 0)
	add(cmdPMA, "phpMyAdmin")
	appendMenu.Call(menu, mfSeparator, 0, 0)
	add(cmdQuit, "Quit tray")

	var pt point
	getCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Required by TrackPopupMenu, or the menu refuses to dismiss.
	setForeground.Call(uintptr(hwnd))
	cmd, _, _ := trackPopupMenu.Call(menu, tpmReturnCmd|tpmNonotify,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	if cmd != 0 {
		wndProc(hwnd, wmCommand, cmd, 0)
	}
}
