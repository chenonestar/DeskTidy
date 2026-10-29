//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	shlwapi  = windows.NewLazySystemDLL("shlwapi.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pFindWindowW                   = user32.NewProc("FindWindowW")
	pFindWindowExW                 = user32.NewProc("FindWindowExW")
	pEnumWindows                   = user32.NewProc("EnumWindows")
	pGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	pShowWindowAsync               = user32.NewProc("ShowWindowAsync")
	pIsWindowVisible               = user32.NewProc("IsWindowVisible")
	pIsWindow                      = user32.NewProc("IsWindow")
	pIsChild                       = user32.NewProc("IsChild")
	pSetParent                     = user32.NewProc("SetParent")
	pGetParent                     = user32.NewProc("GetParent")
	pGetAncestor                   = user32.NewProc("GetAncestor")
	pGetWindow                     = user32.NewProc("GetWindow")
	pSetWindowLongPtrW             = user32.NewProc("SetWindowLongPtrW")
	pGetWindowLongPtrW             = user32.NewProc("GetWindowLongPtrW")
	pCallWindowProcW               = user32.NewProc("CallWindowProcW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pSetWindowPos                  = user32.NewProc("SetWindowPos")
	pGetWindowRect                 = user32.NewProc("GetWindowRect")
	pClientToScreen                = user32.NewProc("ClientToScreen")
	pScreenToClient                = user32.NewProc("ScreenToClient")
	pWindowFromPoint               = user32.NewProc("WindowFromPoint")
	pGetClassNameW                 = user32.NewProc("GetClassNameW")
	pGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	pSetWinEventHook               = user32.NewProc("SetWinEventHook")
	pUnhookWinEvent                = user32.NewProc("UnhookWinEvent")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pPostMessageW                  = user32.NewProc("PostMessageW")
	pPostThreadMessageW            = user32.NewProc("PostThreadMessageW")
	pRegisterWindowMessageW        = user32.NewProc("RegisterWindowMessageW")
	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pSendInput                     = user32.NewProc("SendInput")
	pSetCursorPos                  = user32.NewProc("SetCursorPos")
	pGetCursorPos                  = user32.NewProc("GetCursorPos")
	pEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	pGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	pGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetDC                         = user32.NewProc("GetDC")
	pReleaseDC                     = user32.NewProc("ReleaseDC")

	pGetObjectW   = gdi32.NewProc("GetObjectW")
	pGetDIBits    = gdi32.NewProc("GetDIBits")
	pDeleteObject = gdi32.NewProc("DeleteObject")

	pSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	pSHChangeNotify              = shell32.NewProc("SHChangeNotify")
	pStrRetToStrW                = shlwapi.NewProc("StrRetToStrW")

	pCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	pCoUninitialize   = ole32.NewProc("CoUninitialize")
	pCoCreateInstance = ole32.NewProc("CoCreateInstance")
	pCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")

	pK32GetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

const (
	swHide = 0
	swShow = 5

	gwlWndProc    = -4
	gwlHwndParent = -8
	gwlStyle      = -16
	gwlExStyle    = -20

	wsChild        = 0x40000000
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsClipSiblings = 0x04000000
	wsCaption      = 0x00C00000
	wsThickFrame   = 0x00040000
	wsExToolWindow = 0x00000080

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	wmDestroy           = 0x0002
	wmCommand           = 0x0111
	wmWindowPosChanging = 0x0046
	wmQuit              = 0x0012

	gaRoot = 2

	eventSystemForeground = 0x0003
	eventObjectShow       = 0x8002
	winEventOutOfContext  = 0x0000
	objidWindow           = 0

	inputMouse      = 0
	inputKeyboard   = 1
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	keyEventKeyUp   = 0x0002
	keyEventUnicode = 0x0004
	vkLWin          = 0x5B

	// 桌面 SHELLDLL_DefView 的命令：刷新、切换“显示桌面图标”
	cmdDesktopRefresh     = 0x7103
	cmdToggleDesktopIcons = 0x7402
)

var hwndBottom = uintptr(1)

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type windowPos struct {
	Hwnd            uintptr
	HwndInsertAfter uintptr
	X, Y, Cx, Cy    int32
	Flags           uint32
}

func utf16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

func findWindow(class string) uintptr {
	h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(utf16(class))), 0)
	return h
}

func findWindowEx(parent, after uintptr, class, title string) uintptr {
	var t uintptr
	if title != "" {
		t = uintptr(unsafe.Pointer(utf16(title)))
	}
	h, _, _ := pFindWindowExW.Call(parent, after, uintptr(unsafe.Pointer(utf16(class))), t)
	return h
}

func className(h uintptr) string {
	if h == 0 {
		return ""
	}
	buf := make([]uint16, 256)
	pGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func windowPid(h uintptr) uint32 {
	var pid uint32
	pGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func isWindow(h uintptr) bool {
	r, _, _ := pIsWindow.Call(h)
	return h != 0 && r != 0
}

func isVisible(h uintptr) bool {
	r, _, _ := pIsWindowVisible.Call(h)
	return r != 0
}

func isChild(parent, h uintptr) bool {
	r, _, _ := pIsChild.Call(parent, h)
	return r != 0
}

func showAsync(h uintptr, cmd int) {
	pShowWindowAsync.Call(h, uintptr(cmd))
}

func getParent(h uintptr) uintptr {
	r, _, _ := pGetParent.Call(h)
	return r
}

func getLongPtr(h uintptr, idx int) uintptr {
	r, _, _ := pGetWindowLongPtrW.Call(h, uintptr(idx))
	return r
}

func setLongPtr(h uintptr, idx int, v uintptr) uintptr {
	r, _, _ := pSetWindowLongPtrW.Call(h, uintptr(idx), v)
	return r
}

func setWindowPos(h, after uintptr, x, y, cx, cy int32, flags uint32) {
	pSetWindowPos.Call(h, after, uintptr(x), uintptr(y), uintptr(cx), uintptr(cy), uintptr(flags))
}

func windowRect(h uintptr) rect {
	var r rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r)))
	return r
}

func clientToScreen(h uintptr, p point) point {
	pClientToScreen.Call(h, uintptr(unsafe.Pointer(&p)))
	return p
}

func screenToClient(h uintptr, p point) point {
	pScreenToClient.Call(h, uintptr(unsafe.Pointer(&p)))
	return p
}

func windowFromPoint(p point) uintptr {
	// POINT 按值传递，x64 上打包进一个寄存器
	r, _, _ := pWindowFromPoint.Call(uintptr(uint32(p.X)) | uintptr(uint32(p.Y))<<32)
	return r
}

func foregroundWindow() uintptr {
	r, _, _ := pGetForegroundWindow.Call()
	return r
}

func postMessage(h uintptr, m uint32, w, l uintptr) {
	pPostMessageW.Call(h, uintptr(m), w, l)
}

func dpiForWindow(h uintptr) uint32 {
	if pGetDpiForWindow.Find() != nil {
		return 96
	}
	r, _, _ := pGetDpiForWindow.Call(h)
	return uint32(r)
}

func cursorPos() point {
	var p point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return p
}

// ---- SendInput ----

type mouseInput struct {
	Type      uint32
	_         uint32
	Dx, Dy    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
}

type keybdInput struct {
	Type      uint32
	_         uint32
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
	_         [8]byte // 与 MOUSEINPUT 等长
}

func sendMouse(flags uint32) {
	in := mouseInput{Type: inputMouse, Flags: flags}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(vk uint16, scan uint16, flags uint32) {
	in := keybdInput{Type: inputKeyboard, Vk: vk, Scan: scan, Flags: flags}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func clickAt(p point) {
	pSetCursorPos.Call(uintptr(p.X), uintptr(p.Y))
	sendMouse(mouseLeftDown)
	sendMouse(mouseLeftUp)
}

func typeText(s string) {
	for _, c := range windows.StringToUTF16(s) {
		if c == 0 {
			continue
		}
		sendKey(0, c, keyEventUnicode)
		sendKey(0, c, keyEventUnicode|keyEventKeyUp)
	}
}

func pressWinD() {
	sendKey(vkLWin, 0, 0)
	sendKey('D', 0, 0)
	sendKey('D', 0, keyEventKeyUp)
	sendKey(vkLWin, 0, keyEventKeyUp)
}

// ---- 显示器 ----

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

type MonitorDesc struct {
	Left, Top, Right, Bottom int32
	Primary                  bool
}

var (
	monitorList []MonitorDesc
	monitorCb   = syscall.NewCallback(func(hmon, hdc, lprc, data uintptr) uintptr {
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi)))
		monitorList = append(monitorList, MonitorDesc{mi.Monitor.Left, mi.Monitor.Top, mi.Monitor.Right, mi.Monitor.Bottom, mi.Flags&1 != 0})
		return 1
	})
	enumMu sync.Mutex
)

func monitors() []MonitorDesc {
	enumMu.Lock()
	defer enumMu.Unlock()
	monitorList = nil
	pEnumDisplayMonitors.Call(0, 0, monitorCb, 0)
	return monitorList
}

// ---- COM ----

const (
	coinitApartmentThreaded = 0x2
	clsctxAll               = 0x17
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// vcall 调用 COM 对象虚表中第 idx 个方法
func vcall(obj uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

func release(obj uintptr) {
	if obj != 0 {
		vcall(obj, 2)
	}
}

func queryInterface(obj uintptr, iid *windows.GUID) (uintptr, uint32) {
	var out uintptr
	hr := vcall(obj, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	return out, uint32(hr)
}

func coTaskMemFree(p uintptr) {
	if p != 0 {
		pCoTaskMemFree.Call(p)
	}
}
