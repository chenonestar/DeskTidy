//go:build windows

package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ---- 定位桌面窗口 ----

type DesktopWindows struct {
	Progman     uintptr
	Host        uintptr // SHELLDLL_DefView 的父窗口：Progman 或某个 WorkerW
	DefView     uintptr
	ListView    uintptr
	ExplorerPid uint32
}

func (d DesktopWindows) String() string {
	return fmt.Sprintf("Progman=%#x 宿主=%s(%#x) DefView=%#x ListView=%#x ExplorerPID=%d",
		d.Progman, className(d.Host), d.Host, d.DefView, d.ListView, d.ExplorerPid)
}

var (
	enumWorkerHost    uintptr
	enumWorkerDefView uintptr
	enumWorkerCb      = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if className(h) == "WorkerW" {
			if dv := findWindowEx(h, 0, "SHELLDLL_DefView", ""); dv != 0 {
				enumWorkerHost, enumWorkerDefView = h, dv
				return 0
			}
		}
		return 1
	})
)

func findDesktop() DesktopWindows {
	var d DesktopWindows
	d.Progman = findWindow("Progman")
	d.Host = d.Progman
	d.DefView = findWindowEx(d.Progman, 0, "SHELLDLL_DefView", "")
	if d.DefView == 0 {
		// 播放过壁纸动画等情况下，DefView 会被移到一个顶层 WorkerW 里
		enumMu.Lock()
		enumWorkerHost, enumWorkerDefView = 0, 0
		pEnumWindows.Call(enumWorkerCb, 0)
		d.Host, d.DefView = enumWorkerHost, enumWorkerDefView
		enumMu.Unlock()
	}
	if d.DefView != 0 {
		d.ListView = findWindowEx(d.DefView, 0, "SysListView32", "")
	}
	if d.Progman != 0 {
		d.ExplorerPid = windowPid(d.Progman)
	}
	return d
}

// 当前桌面窗口句柄，供钩子线程读取
var (
	curListView atomic.Uintptr
	curDefView  atomic.Uintptr
	curHost     atomic.Uintptr
)

func refreshDesktop() DesktopWindows {
	d := findDesktop()
	curListView.Store(d.ListView)
	curDefView.Store(d.DefView)
	curHost.Store(d.Host)
	return d
}

// ---- 注册表 HideIcons ----

const advancedKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`

func hideIconsSetting() uint32 {
	k, err := registry.OpenKey(registry.CURRENT_USER, advancedKey, registry.QUERY_VALUE)
	if err != nil {
		return 0
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("HideIcons")
	if err != nil {
		return 0
	}
	return uint32(v)
}

// ---- 临时隐藏 / 显示 ----

var wantHidden atomic.Bool

func hideIcons() error {
	d := refreshDesktop()
	if d.ListView == 0 {
		return fmt.Errorf("没有找到桌面图标列表窗口")
	}
	// 先记录再修改
	updateState(func(s *DemoState) { s.WantHidden = true; s.ExplorerPid = d.ExplorerPid })
	wantHidden.Store(true)
	showAsync(d.ListView, swHide)
	return waitVisible(d.ListView, false, 2*time.Second)
}

func showIcons() error {
	wantHidden.Store(false)
	d := refreshDesktop()
	if d.ListView != 0 && hideIconsSetting() == 0 {
		showAsync(d.ListView, swShow)
		if err := waitVisible(d.ListView, true, 2*time.Second); err != nil {
			return err
		}
	}
	updateState(func(s *DemoState) { s.WantHidden = false })
	return nil
}

func waitVisible(h uintptr, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isVisible(h) == want {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("等待 %v 后可见状态仍不是 %v", timeout, want)
}

// restoreDesktop 把桌面恢复成原样；主程序退出、守护进程和 --restore 共用
func restoreDesktop(s DemoState, why string) {
	logf("恢复原桌面（%s）", why)
	wantHidden.Store(false)
	d := refreshDesktop()
	if s.FallbackToggled && d.DefView != 0 && hideIconsSetting() != s.OrigHideIcons {
		postMessage(d.DefView, wmCommand, cmdToggleDesktopIcons, 0)
		time.Sleep(time.Second)
		logf("已把“显示桌面图标”设置切回原值 %d（当前 %d）", s.OrigHideIcons, hideIconsSetting())
		d = refreshDesktop()
	}
	if hideIconsSetting() == 0 && d.ListView != 0 && !isVisible(d.ListView) {
		showAsync(d.ListView, swShow)
		if err := waitVisible(d.ListView, true, 3*time.Second); err != nil {
			logf("恢复系统图标失败：%v", err)
		} else {
			logf("系统图标已恢复显示")
		}
	}
	if s.PositionsShuffle != "" {
		if r, err := restorePositions(s.PositionsShuffle); err != nil {
			logf("恢复图标位置失败：%v", err)
		} else {
			logf("图标位置已按快照恢复：%d 个，最大偏差 %d px", r.Applied, r.MaxDelta)
		}
	}
	updateState(func(st *DemoState) {
		st.WantHidden, st.FallbackToggled, st.PositionsShuffle = false, false, ""
	})
}

// ---- 钩子线程：监听图标列表被重新显示、前台窗口变化、Explorer 重启 ----

type ExplorerRestart struct {
	At             string `json:"at"`
	ListViewMs     int64  `json:"listViewMs"`     // 新桌面窗口出现耗时
	RehiddenMs     int64  `json:"rehiddenMs"`     // 再次隐藏完成耗时（-1 表示不需要或失败）
	PanelAlive     bool   `json:"panelAlive"`     // 面板窗口是否还在
	PanelMode      string `json:"panelMode"`      // 当时的面板模式
	PanelReapplied bool   `json:"panelReapplied"` // 是否重新挂回
}

var (
	reshowCount   atomic.Int64
	reshowRehide  atomic.Int64
	restartsMu    sync.Mutex
	restarts      []ExplorerRestart
	onForeground  func(h uintptr)
	hookThreadTid uint32
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

func startHookThread() {
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		hookThreadTid = windows.GetCurrentThreadId()

		msgID, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
		taskbarCreated := uint32(msgID)
		wndProc := syscall.NewCallback(func(h, m, w, l uintptr) uintptr {
			if uint32(m) == taskbarCreated {
				go onTaskbarCreated()
			}
			r, _, _ := pDefWindowProcW.Call(h, m, w, l)
			return r
		})
		var hinst windows.Handle
		windows.GetModuleHandleEx(0, nil, &hinst)
		wc := wndClassEx{WndProc: wndProc, Instance: uintptr(hinst), ClassName: utf16("DeskTidyDemoHook")}
		wc.Size = uint32(unsafe.Sizeof(wc))
		pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		// 隐藏的顶层窗口，才能收到广播的 TaskbarCreated
		pCreateWindowExW.Call(wsExToolWindow, uintptr(unsafe.Pointer(utf16("DeskTidyDemoHook"))),
			uintptr(unsafe.Pointer(utf16("DeskTidyDemoHook"))), wsPopup, 0, 0, 0, 0, 0, 0, uintptr(hinst), 0)

		winEvent := syscall.NewCallback(func(hook, event, hwnd, idObject, idChild, thread, t uintptr) uintptr {
			switch uint32(event) {
			case eventObjectShow:
				if int32(idObject) == objidWindow && hwnd != 0 && hwnd == curListView.Load() {
					reshowCount.Add(1)
					if wantHidden.Load() {
						showAsync(hwnd, swHide)
						reshowRehide.Add(1)
						logf("Explorer 自行重新显示了系统图标，已立即再次隐藏")
					} else {
						logf("系统图标列表变为可见")
					}
				}
			case eventSystemForeground:
				if onForeground != nil {
					onForeground(hwnd)
				}
			}
			return 0
		})
		pSetWinEventHook.Call(eventObjectShow, eventObjectShow, 0, winEvent, 0, 0, winEventOutOfContext)
		pSetWinEventHook.Call(eventSystemForeground, eventSystemForeground, 0, winEvent, 0, 0, winEventOutOfContext)
		close(ready)

		var m msg
		for {
			r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	<-ready
}

var restartMu sync.Mutex

func onTaskbarCreated() {
	restartMu.Lock()
	defer restartMu.Unlock()
	t0 := time.Now()
	logf("收到 TaskbarCreated：Explorer 已重启")
	rec := ExplorerRestart{At: t0.Format("15:04:05"), RehiddenMs: -1, PanelMode: panel.currentMode()}

	var d DesktopWindows
	for i := 0; i < 50; i++ {
		if d = refreshDesktop(); d.ListView != 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	rec.ListViewMs = time.Since(t0).Milliseconds()
	logf("新的桌面窗口：%s（%d ms）", d, rec.ListViewMs)
	updateState(func(s *DemoState) { s.ExplorerPid = d.ExplorerPid })

	rec.PanelAlive = isWindow(panel.hwnd)
	if !rec.PanelAlive {
		logf("面板窗口已随 Explorer 重启被销毁（模式 %s）", rec.PanelMode)
	} else if rec.PanelMode != modeNormal {
		if err := panel.apply(rec.PanelMode); err != nil {
			logf("重新挂回面板失败：%v", err)
		} else {
			rec.PanelReapplied = true
			logf("面板已按 %s 模式重新挂回", rec.PanelMode)
		}
	}
	if wantHidden.Load() && d.ListView != 0 {
		showAsync(d.ListView, swHide)
		if waitVisible(d.ListView, false, 3*time.Second) == nil {
			rec.RehiddenMs = time.Since(t0).Milliseconds()
			logf("Explorer 重启后已重新隐藏系统图标（%d ms）", rec.RehiddenMs)
		}
	}
	restartsMu.Lock()
	restarts = append(restarts, rec)
	restartsMu.Unlock()
}

// ---- 风险 4 自动测试 ----

type TestStep struct {
	Name   string `json:"name"`
	Result string `json:"result"` // pass / fail / info / skip
	Detail string `json:"detail"`
}

func step(name string, ok bool, detail string, args ...any) TestStep {
	r := "fail"
	if ok {
		r = "pass"
	}
	return TestStep{name, r, fmt.Sprintf(detail, args...)}
}

func info(name, detail string, args ...any) TestStep {
	return TestStep{name, "info", fmt.Sprintf(detail, args...)}
}

// 触发一个动作后观察：Explorer 是否把图标列表重新显示、钩子是否把它再次隐藏
func observe(name string, action func(), wait time.Duration) TestStep {
	before := reshowCount.Load()
	action()
	time.Sleep(wait)
	lv := curListView.Load()
	n := reshowCount.Load() - before
	hidden := lv != 0 && !isVisible(lv)
	if n == 0 {
		return step(name, hidden, "Explorer 没有重新显示图标列表；当前隐藏=%v", hidden)
	}
	return step(name, hidden, "Explorer 重新显示了 %d 次，钩子已再次隐藏；当前隐藏=%v", n, hidden)
}

func runRisk4Tests() []TestStep {
	var out []TestStep
	d := refreshDesktop()
	out = append(out, info("桌面窗口结构", "%s", d))
	if d.ListView == 0 {
		return append(out, step("找到桌面图标列表", false, "没有找到 SysListView32，无法继续"))
	}
	orig := hideIconsSetting()
	updateState(func(s *DemoState) { s.OrigHideIcons = orig })
	if orig != 0 {
		return append(out, TestStep{"前提条件", "skip", "系统已关闭“显示桌面图标”（HideIcons=1），请先在桌面右键 → 查看 → 勾选“显示桌面图标”后再测"})
	}

	// 1. 临时隐藏
	err := hideIcons()
	out = append(out, step("SW_HIDE 临时隐藏", err == nil, "隐藏后可见=%v；注册表 HideIcons=%d（应保持 0）", isVisible(d.ListView), hideIconsSetting()))
	if err != nil {
		return out
	}

	// 2. 桌面刷新（相当于在桌面按 F5）
	out = append(out, observe("桌面刷新（F5）", func() {
		postMessage(curDefView.Load(), wmCommand, cmdDesktopRefresh, 0)
	}, 2*time.Second))

	// 3. 全局图标/关联刷新（安装软件后常见）
	out = append(out, observe("SHChangeNotify 全局刷新", func() {
		pSHChangeNotify.Call(0x08000000, 0, 0, 0) // SHCNE_ASSOCCHANGED, SHCNF_IDLIST
	}, 2*time.Second))

	// 4. 模拟用户在右键菜单里关闭再打开“显示桌面图标”
	updateState(func(s *DemoState) { s.FallbackToggled = true })
	out = append(out, observe("用户切换“显示桌面图标”两次", func() {
		postMessage(curDefView.Load(), wmCommand, cmdToggleDesktopIcons, 0)
		time.Sleep(1500 * time.Millisecond)
		postMessage(curDefView.Load(), wmCommand, cmdToggleDesktopIcons, 0)
	}, 2*time.Second))
	settingBack := hideIconsSetting() == orig
	out = append(out, step("切换后系统设置复原", settingBack, "HideIcons=%d（原值 %d）", hideIconsSetting(), orig))
	if settingBack {
		updateState(func(s *DemoState) { s.FallbackToggled = false })
	}

	// 5. 显示回来
	err = showIcons()
	out = append(out, step("SW_SHOW 恢复显示", err == nil, "恢复后可见=%v", isVisible(curListView.Load())))

	// 6. 退路方案：切换系统设置
	updateState(func(s *DemoState) { s.FallbackToggled = true })
	postMessage(curDefView.Load(), wmCommand, cmdToggleDesktopIcons, 0)
	time.Sleep(1500 * time.Millisecond)
	lv := refreshDesktop().ListView
	out = append(out, step("退路方案：切换设置隐藏", lv != 0 && !isVisible(lv) && hideIconsSetting() == 1, "可见=%v，HideIcons=%d（应为 1）", isVisible(lv), hideIconsSetting()))
	postMessage(curDefView.Load(), wmCommand, cmdToggleDesktopIcons, 0)
	time.Sleep(1500 * time.Millisecond)
	lv = refreshDesktop().ListView
	ok := isVisible(lv) && hideIconsSetting() == orig
	out = append(out, step("退路方案：切换设置恢复", ok, "可见=%v，HideIcons=%d（应为 %d）", isVisible(lv), hideIconsSetting(), orig))
	if ok {
		updateState(func(s *DemoState) { s.FallbackToggled = false })
	}
	return out
}
