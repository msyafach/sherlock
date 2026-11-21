package main

// Background mode: a notification-area (tray) icon, minimize/close hiding the window to the tray,
// and the global Alt+Space hotkey. Everything here talks to Win32 directly through user32/shell32.

import (
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	pRegisterHotKey         = user32.NewProc("RegisterHotKey")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pCallWindowProcW        = user32.NewProc("CallWindowProcW")
	pSetWindowLongPtrW      = user32.NewProc("SetWindowLongPtrW")
	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	pGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pShowWindow             = user32.NewProc("ShowWindow")
	pIsIconic               = user32.NewProc("IsIconic")
	pIsWindowVisible        = user32.NewProc("IsWindowVisible")
	pLoadImageW             = user32.NewProc("LoadImageW")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	pGetModuleHandleW       = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	pSetMenuDefaultItem     = user32.NewProc("SetMenuDefaultItem")
)

const (
	modAlt      = 0x0001
	modNoRepeat = 0x4000
	vkSpace     = 0x20

	wmNull        = 0x0000
	wmClose       = 0x0010
	wmSysCommand  = 0x0112
	wmHotkey      = 0x0312
	wmLButtonUp   = 0x0202
	wmLButtonDbl  = 0x0203
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000
	wmTray        = wmApp + 1
	scMinimize    = 0xF020
	swHide        = 0
	swShow        = 5
	swRestore     = 9
	gwlpWndProc   = ^uintptr(3) // -4
	nimAdd        = 0
	nimDelete     = 2
	nifMessage    = 0x1
	nifIcon       = 0x2
	nifTip        = 0x4
	imageIcon     = 1
	smCxSmIcon    = 49
	smCySmIcon    = 50
	mfString      = 0x0
	mfSeparator   = 0x800
	tpmReturnCmd  = 0x100
	tpmRightAlign = 0x8
	tpmBottom     = 0x20
	cmdOpen       = 1
	cmdQuit       = 2
)

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type notifyIconData struct {
	size            uint32
	hwnd            uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guidItem        windows.GUID
	balloonIcon     uintptr
}

// Tray keeps the main window reachable while it is hidden.
type Tray struct {
	main     uintptr // the Sherlock window
	hwnd     uintptr // hidden window that receives tray and hotkey messages
	nid      notifyIconData
	quitting atomic.Bool
	oldProc  uintptr
	onShow   func() // runs after the window is brought up (focus the search box)
	onQuit   func() // ends the app
	created  uint32 // "TaskbarCreated": Explorer restarted, re-add the icon
	HotkeyOK bool
}

var tray *Tray

// startTray hooks minimize/close of the main window so they hide it, adds the tray icon and
// registers Alt+Space. It must be called on the main window's thread.
func startTray(main uintptr, onShow, onQuit func()) *Tray {
	t := &Tray{main: main, onShow: onShow, onQuit: onQuit}
	tray = t
	t.oldProc, _, _ = pSetWindowLongPtrW.Call(main, gwlpWndProc, windows.NewCallback(mainProc))

	ready := make(chan struct{})
	go t.loop(ready)
	<-ready
	return t
}

// mainProc wraps the webview window procedure: minimize and close hide to the tray instead.
func mainProc(hwnd, msg, wp, lp uintptr) uintptr {
	t := tray
	switch {
	case msg == wmClose && !t.quitting.Load():
		pShowWindow.Call(hwnd, swHide)
		return 0
	case msg == wmSysCommand && wp&0xFFF0 == scMinimize:
		pShowWindow.Call(hwnd, swHide)
		return 0
	}
	r, _, _ := pCallWindowProcW.Call(t.oldProc, hwnd, msg, wp, lp)
	return r
}

func (t *Tray) loop(ready chan struct{}) {
	runtime.LockOSThread() // the tray window, hotkey and their messages belong to this thread
	inst, _, _ := pGetModuleHandleW.Call(0)
	cls, _ := windows.UTF16PtrFromString("SherlockTray")
	wc := wndClassEx{wndProc: windows.NewCallback(trayProc), instance: inst, className: cls}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	t.hwnd, _, _ = pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)),
		0, 0, 0, 0, 0, 0, 0, inst, 0)
	tc, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tc)))
	t.created = uint32(r)

	cx, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := pGetSystemMetrics.Call(smCySmIcon)
	icon, _, _ := pLoadImageW.Call(inst, 1, imageIcon, cx, cy, 0) // icon resource 1 (winres/app.rc)
	t.nid = notifyIconData{hwnd: t.hwnd, id: 1, flags: nifMessage | nifIcon | nifTip, callbackMessage: wmTray, icon: icon}
	t.nid.size = uint32(unsafe.Sizeof(t.nid))
	copy(t.nid.tip[:], windows.StringToUTF16("Sherlock — Alt+Space untuk membuka"))
	pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid)))

	ok, _, _ := pRegisterHotKey.Call(t.hwnd, 1, modAlt|modNoRepeat, vkSpace)
	t.HotkeyOK = ok != 0
	close(ready)

	var m winMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func trayProc(hwnd, msg, wp, lp uintptr) uintptr {
	t := tray
	switch {
	case t == nil:
	case msg == wmHotkey:
		t.Toggle()
		return 0
	case msg == wmTray:
		switch lp & 0xFFFF {
		case wmLButtonUp, wmLButtonDbl:
			t.Show()
		case wmRButtonUp:
			t.menu()
		}
		return 0
	case t.created != 0 && uint32(msg) == t.created:
		pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid)))
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (t *Tray) menu() {
	m, _, _ := pCreatePopupMenu.Call()
	open, _ := windows.UTF16PtrFromString("Buka Sherlock")
	quit, _ := windows.UTF16PtrFromString("Keluar")
	pAppendMenuW.Call(m, mfString, cmdOpen, uintptr(unsafe.Pointer(open)))
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	pAppendMenuW.Call(m, mfString, cmdQuit, uintptr(unsafe.Pointer(quit)))
	pSetMenuDefaultItem.Call(m, cmdOpen, 0)
	var pt struct{ x, y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(t.hwnd) // so the menu closes when clicking elsewhere
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmReturnCmd|tpmRightAlign|tpmBottom, uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	pPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	pDestroyMenu.Call(m)
	switch cmd {
	case cmdOpen:
		t.Show()
	case cmdQuit:
		t.Quit()
	}
}

// Show brings the main window to the front and focuses the search box.
func (t *Tray) Show() {
	if iconic, _, _ := pIsIconic.Call(t.main); iconic != 0 {
		pShowWindow.Call(t.main, swRestore)
	} else {
		pShowWindow.Call(t.main, swShow)
	}
	pSetForegroundWindow.Call(t.main)
	t.onShow()
}

// Toggle hides the window when it is in front, otherwise shows it.
func (t *Tray) Toggle() {
	fg, _, _ := pGetForegroundWindow.Call()
	vis, _, _ := pIsWindowVisible.Call(t.main)
	if fg == t.main && vis != 0 {
		pShowWindow.Call(t.main, swHide)
		return
	}
	t.Show()
}

// Quit really closes the app.
func (t *Tray) Quit() {
	t.quitting.Store(true)
	t.onQuit()
}

// Remove deletes the tray icon (call on exit).
func (t *Tray) Remove() {
	pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
}
