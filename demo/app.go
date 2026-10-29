//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/wailsapp/go-webview2/webviewloader"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
)

// App 的公开方法都会绑定到前端 window.go.main.App
type App struct {
	ctx       context.Context
	readyOnce sync.Once
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	logSink = func(line string) { wruntime.EventsEmit(ctx, "log", line) }
}

func (a *App) domReady(ctx context.Context) {
	a.readyOnce.Do(func() {
		if err := panel.attach(); err != nil {
			logf("面板初始化失败：%v", err)
		}
		startHookThread()
		logf("面板窗口 %#x，DPI %d", panel.hwnd, dpiForWindow(panel.hwnd))
	})
}

func (a *App) shutdown(ctx context.Context) {
	logSink = nil
	stateMu.Lock()
	s := state
	stateMu.Unlock()
	restoreDesktop(s, "正常退出")
	updateState(func(s *DemoState) { s.CleanExit = true })
	logf("==== Demo 正常退出")
}

func (a *App) Quit() { wruntime.Quit(a.ctx) }

// ---- 通用 ----

type SystemInfo struct {
	Windows  string        `json:"windows"`
	WebView2 string        `json:"webview2"`
	CPUs     int           `json:"cpus"`
	Dpi      uint32        `json:"dpi"`
	Monitors []MonitorDesc `json:"monitors"`
	DataDir  string        `json:"dataDir"`
	Desktop  string        `json:"desktop"`
}

func (a *App) SystemInfo() SystemInfo {
	v := windows.RtlGetVersion()
	wv, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	if err != nil {
		wv = "未知：" + err.Error()
	}
	return SystemInfo{
		Windows:  fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber),
		WebView2: wv,
		CPUs:     runtime.NumCPU(),
		Dpi:      dpiForWindow(panel.hwnd),
		Monitors: monitors(),
		DataDir:  dataDir,
		Desktop:  findDesktop().String(),
	}
}

type Status struct {
	Mode            string            `json:"mode"`
	WantHidden      bool              `json:"wantHidden"`
	ListViewVisible bool              `json:"listViewVisible"`
	HideIcons       uint32            `json:"hideIcons"`
	ReshowCount     int64             `json:"reshowCount"`
	ReshowRehide    int64             `json:"reshowRehide"`
	Restarts        []ExplorerRestart `json:"restarts"`
	Foreground      string            `json:"foreground"`
}

func (a *App) Status() Status {
	restartsMu.Lock()
	rs := append([]ExplorerRestart(nil), restarts...)
	restartsMu.Unlock()
	return Status{
		Mode:            panel.currentMode(),
		WantHidden:      wantHidden.Load(),
		ListViewVisible: isVisible(curListView.Load()),
		HideIcons:       hideIconsSetting(),
		ReshowCount:     reshowCount.Load(),
		ReshowRehide:    reshowRehide.Load(),
		Restarts:        rs,
		Foreground:      className(foregroundWindow()),
	}
}

// ---- 风险 1 ----

func (a *App) SetPanelMode(mode string) error { return panel.apply(mode) }

func (a *App) AutoTestPanel(req PanelTestRequest) PanelTestResult { return panel.autoTest(req) }

// ---- 风险 4 ----

func (a *App) HideIcons() error {
	if hideIconsSetting() != 0 {
		return fmt.Errorf("系统已关闭“显示桌面图标”，请先在桌面右键 → 查看中打开")
	}
	err := hideIcons()
	logf("临时隐藏系统图标：%v", errText(err))
	return err
}

func (a *App) ShowIcons() error {
	err := showIcons()
	logf("恢复显示系统图标：%v", errText(err))
	return err
}

func (a *App) RunRisk4() []TestStep {
	logf("---- 风险 4 自动测试开始")
	r := runRisk4Tests()
	logf("---- 风险 4 自动测试结束")
	return r
}

// ---- 风险 5 ----

func (a *App) LoadIcons(n int) IconLoadResult { return loadIcons(n) }

func (a *App) MeasureMemory(seconds int) MemoryReport { return measureMemory(seconds) }

// ---- 风险 6 ----

func (a *App) RunRisk6() []TestStep {
	logf("---- 风险 6 自动测试开始")
	r := runRisk6Tests()
	logf("---- 风险 6 自动测试结束")
	return r
}

func (a *App) TakeSnapshot() (*PositionSnapshot, error) { return takeSnapshot() }

func (a *App) RestoreLatestSnapshot() (*RestoreResult, error) {
	files, _ := filepath.Glob(filepath.Join(dataDir, "snapshots", "*.json"))
	if len(files) == 0 {
		return nil, fmt.Errorf("还没有快照")
	}
	sort.Strings(files)
	r, err := restorePositions(files[len(files)-1])
	if err == nil {
		logf("按快照 %s 恢复：写回 %d 个，最大偏差 %d px", filepath.Base(files[len(files)-1]), r.Applied, r.MaxDelta)
	}
	return r, err
}

// ---- 报告 ----

func (a *App) SaveReport(markdown string) (string, error) {
	p := filepath.Join(dataDir, "DeskTidy-Demo-报告.md")
	if err := os.WriteFile(p, []byte(markdown), 0o644); err != nil {
		return "", err
	}
	exec.Command("explorer.exe", "/select,", p).Start()
	logf("报告已保存：%s", p)
	return p, nil
}

func (a *App) LogTail() string {
	b, _ := os.ReadFile(filepath.Join(dataDir, "demo-log.txt"))
	if len(b) > 20000 {
		b = b[len(b)-20000:]
	}
	return string(b)
}

func errText(err error) string {
	if err == nil {
		return "成功"
	}
	return err.Error()
}
