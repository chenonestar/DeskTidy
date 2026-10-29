//go:build windows

package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// 风险 1：面板挂到桌面层的三种做法
const (
	modeNormal = "normal" // 普通窗口（对照组）
	modeChild  = "child"  // SetParent 成为桌面 DefView 宿主窗口的子窗口
	modeOwned  = "owned"  // 顶层窗口，owner 设为桌面宿主窗口，并始终置底
	modeBottom = "bottom" // 普通顶层窗口置底，桌面成为前台时临时提上来
)

type panelState struct {
	mu        sync.Mutex
	hwnd      uintptr
	mode      string
	origStyle uintptr
	origWnd   uintptr // 原窗口过程
	keepDown  atomic.Bool
	raised    atomic.Bool
}

var panel = &panelState{mode: modeNormal}

var panelWndProc = syscall.NewCallback(func(h, m, w, l uintptr) uintptr {
	if uint32(m) == wmWindowPosChanging && panel.keepDown.Load() && !panel.raised.Load() {
		wp := (*windowPos)(unsafe.Pointer(l))
		if wp.Flags&swpNoZOrder == 0 {
			wp.HwndInsertAfter = hwndBottom
		}
	}
	r, _, _ := pCallWindowProcW.Call(panel.origWnd, h, m, w, l)
	return r
})

func (p *panelState) currentMode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *panelState) attach() error {
	p.hwnd = findWindow(panelClass)
	if p.hwnd == 0 {
		return fmt.Errorf("没有找到面板窗口")
	}
	p.origStyle = getLongPtr(p.hwnd, gwlStyle)
	p.origWnd = setLongPtr(p.hwnd, gwlWndProc, panelWndProc)
	onForeground = p.onForeground
	return nil
}

// 当前面板在屏幕上的位置
func (p *panelState) screenRect() rect { return windowRect(p.hwnd) }

func (p *panelState) reset() {
	r := p.screenRect()
	p.keepDown.Store(false)
	p.raised.Store(false)
	if getParent(p.hwnd) != 0 && getLongPtr(p.hwnd, gwlStyle)&wsChild != 0 {
		pSetParent.Call(p.hwnd, 0)
	}
	setLongPtr(p.hwnd, gwlHwndParent, 0)
	setLongPtr(p.hwnd, gwlStyle, p.origStyle)
	setWindowPos(p.hwnd, 0, r.Left, r.Top, r.Right-r.Left, r.Bottom-r.Top, swpNoZOrder|swpFrameChanged|swpShowWindow)
}

func (p *panelState) apply(mode string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hwnd == 0 {
		return fmt.Errorf("面板窗口还没就绪")
	}
	p.reset()
	d := refreshDesktop()
	r := p.screenRect()
	w, h := r.Right-r.Left, r.Bottom-r.Top
	switch mode {
	case modeNormal:
	case modeChild:
		if d.Host == 0 {
			return fmt.Errorf("没有找到桌面宿主窗口")
		}
		style := (p.origStyle &^ (wsPopup | wsCaption | wsThickFrame)) | wsChild | wsClipSiblings | wsVisible
		setLongPtr(p.hwnd, gwlStyle, style)
		pSetParent.Call(p.hwnd, d.Host)
		pt := screenToClient(d.Host, point{r.Left, r.Top})
		setWindowPos(p.hwnd, 0, pt.X, pt.Y, w, h, swpFrameChanged|swpShowWindow) // HWND_TOP：在 DefView 之上
	case modeOwned:
		if d.Host == 0 {
			return fmt.Errorf("没有找到桌面宿主窗口")
		}
		setLongPtr(p.hwnd, gwlHwndParent, d.Host)
		p.keepDown.Store(true)
		setWindowPos(p.hwnd, hwndBottom, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	case modeBottom:
		p.keepDown.Store(true)
		setWindowPos(p.hwnd, hwndBottom, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	default:
		return fmt.Errorf("未知模式 %s", mode)
	}
	p.mode = mode
	updateState(func(s *DemoState) { s.PanelMode = mode })
	logf("面板模式切换为 %s；父窗口=%s(%#x)", mode, className(getParent(p.hwnd)), getParent(p.hwnd))
	return nil
}

// bottom 模式的补救：桌面成为前台（Win+D、点桌面空白处）时把面板临时提到顶，其他程序成为前台时再压回底部
func (p *panelState) onForeground(h uintptr) {
	if p.currentMode() != modeBottom || h == 0 {
		return
	}
	cls := className(h)
	isDesktop := cls == "Progman" || cls == "WorkerW"
	switch {
	case isDesktop && !p.raised.Load():
		p.raised.Store(true)
		setWindowPos(p.hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	case !isDesktop && h != p.hwnd && !isChild(p.hwnd, h) && p.raised.Load():
		p.raised.Store(false)
		setWindowPos(p.hwnd, hwndBottom, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
	}
}

// ---- 风险 1 自动测试 ----

type PanelTestRequest struct {
	ClickX, ClickY float64 // 测试按钮中心（CSS 像素，相对网页左上角）
	InputX, InputY float64 // 输入框中心
	Dpr            float64
}

type PanelTestResult struct {
	Mode              string     `json:"mode"`
	Steps             []TestStep `json:"steps"`
	VisibleAfterWinD  bool       `json:"visibleAfterWinD"`
	ForegroundAfterWD string     `json:"foregroundAfterWinD"`
}

func (p *panelState) toScreen(x, y, dpr float64) point {
	origin := clientToScreen(p.hwnd, point{0, 0})
	return point{origin.X + int32(x*dpr), origin.Y + int32(y*dpr)}
}

// 命中测试：该屏幕点上最上层的窗口是不是面板（或面板里的 WebView2 子窗口）
func (p *panelState) hitTest(pt point) (bool, string) {
	h := windowFromPoint(pt)
	ok := h == p.hwnd || isChild(p.hwnd, h)
	return ok, fmt.Sprintf("%s(%#x)", className(h), h)
}

func (p *panelState) autoTest(req PanelTestRequest) PanelTestResult {
	res := PanelTestResult{Mode: p.currentMode()}
	saved := cursorPos()
	click := p.toScreen(req.ClickX, req.ClickY, req.Dpr)
	input := p.toScreen(req.InputX, req.InputY, req.Dpr)

	ok, who := p.hitTest(click)
	res.Steps = append(res.Steps, info("Win+D 之前", "测试点上的窗口：%s，是面板=%v（被其他窗口盖住属正常）", who, ok))

	pressWinD()
	time.Sleep(1200 * time.Millisecond)
	fg := foregroundWindow()
	res.ForegroundAfterWD = className(fg)
	ok, who = p.hitTest(click)
	res.VisibleAfterWinD = ok
	res.Steps = append(res.Steps, step("Win+D 后面板可见", ok && isVisible(p.hwnd), "测试点上的窗口：%s；前台窗口：%s", who, res.ForegroundAfterWD))

	clickAt(click)
	time.Sleep(400 * time.Millisecond)
	clickAt(input)
	time.Sleep(300 * time.Millisecond)
	typeText("desk")
	time.Sleep(400 * time.Millisecond)
	ok, who = p.hitTest(click)
	res.Steps = append(res.Steps, info("操作后", "测试点上的窗口：%s，是面板=%v；前台窗口：%s", who, ok, className(foregroundWindow())))

	pressWinD() // 把其他窗口还原回来
	time.Sleep(800 * time.Millisecond)
	pSetCursorPos.Call(uintptr(saved.X), uintptr(saved.Y))
	return res
}
