//go:build windows

// DeskTidy 预研 Demo：验证需求说明第 8 节风险 1、4、5、6
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailswin "github.com/wailsapp/wails/v2/pkg/options/windows"
	"golang.org/x/sys/windows"
)

//go:embed all:frontend
var assets embed.FS

const panelClass = "DeskTidyDemoPanel"

func main() {
	if err := initDataDir(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--restore":
			s, _ := readState()
			restoreDesktop(s, "手动执行 --restore")
			return
		case "--guard":
			if len(os.Args) >= 3 {
				if pid, err := strconv.Atoi(os.Args[2]); err == nil {
					runGuard(pid)
				}
			}
			return
		}
	}

	// 单实例：必须在改动状态文件、启动守护进程之前判断
	if _, err := windows.CreateMutex(nil, false, utf16(`Local\DeskTidyDemo`)); err == windows.ERROR_ALREADY_EXISTS {
		windows.MessageBox(0, utf16("DeskTidy Demo 已经在运行。"), utf16("DeskTidy Demo"), 0x40)
		return
	}

	// 与正式版一致：按显示器感知 DPI，坐标都是物理像素
	pSetProcessDpiAwarenessContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4

	// 上次异常退出且守护进程没来得及处理时，先恢复
	if prev, err := readState(); err == nil && !prev.CleanExit && !processAlive(prev.Pid) {
		restoreDesktop(prev, "上次未正常退出")
	}
	d := refreshDesktop()
	updateState(func(s *DemoState) {
		*s = DemoState{Pid: os.Getpid(), OrigHideIcons: hideIconsSetting(), PanelMode: modeNormal, ExplorerPid: d.ExplorerPid}
	})
	logf("==== Demo 启动，PID %d；%s", os.Getpid(), d)
	startGuard()

	sub, _ := fs.Sub(assets, "frontend")
	app := &App{}
	err := wails.Run(&options.App{
		Title:            "DeskTidy Demo",
		Width:            980,
		Height:           800,
		MinWidth:         720,
		MinHeight:        560,
		Frameless:        true,
		BackgroundColour: &options.RGBA{R: 243, G: 245, B: 248, A: 255},
		AssetServer:      &assetserver.Options{Assets: sub, Handler: iconHandler{}},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Windows: &wailswin.Options{
			WindowClassName:     panelClass,
			WebviewUserDataPath: filepath.Join(dataDir, "webview2"),
		},
	})
	if err != nil {
		logf("Wails 运行失败：%v", err)
	}
}

func processAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// ---- 守护进程 ----

func startGuard() {
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "--guard", strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		logf("启动守护进程失败：%v", err)
		return
	}
	logf("守护进程已启动，PID %d", cmd.Process.Pid)
}

func runGuard(pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	windows.WaitForSingleObject(h, windows.INFINITE)
	windows.CloseHandle(h)

	s, err := readState()
	if err != nil || s.Pid != pid || s.CleanExit {
		return
	}
	logf("守护进程：主程序 PID %d 异常退出（面板模式 %s）", pid, s.PanelMode)
	if cur := findDesktop(); s.ExplorerPid != 0 && cur.ExplorerPid != s.ExplorerPid {
		logf("守护进程：主程序退出前 Explorer 重启过（PID %d → %d），面板可能随之被销毁", s.ExplorerPid, cur.ExplorerPid)
	}
	restoreDesktop(s, "守护进程")
}
