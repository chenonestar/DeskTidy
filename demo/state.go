//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 所有数据写在程序目录 demo-data\ 下（与正式版的绿色便携约定一致）
var dataDir string

func initDataDir() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dataDir = filepath.Join(filepath.Dir(exe), "demo-data")
	return os.MkdirAll(filepath.Join(dataDir, "snapshots"), 0o755)
}

// ---- 日志 ----

var (
	logMu   sync.Mutex
	logSink func(string) // 主进程里把日志同步推给界面
)

func logf(format string, args ...any) {
	line := time.Now().Format("15:04:05.000") + "  " + fmt.Sprintf(format, args...)
	logMu.Lock()
	defer logMu.Unlock()
	if f, err := os.OpenFile(filepath.Join(dataDir, "demo-log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		f.WriteString(line + "\r\n")
		f.Close()
	}
	if logSink != nil {
		logSink(line)
	}
}

// ---- 状态文件：先记录再修改，守护进程据此恢复 ----

type DemoState struct {
	Pid              int    `json:"pid"`
	CleanExit        bool   `json:"cleanExit"`
	WantHidden       bool   `json:"wantHidden"`       // 用 SW_HIDE 临时隐藏了系统图标
	FallbackToggled  bool   `json:"fallbackToggled"`  // 正在测试退路方案，系统设置被切换过
	OrigHideIcons    uint32 `json:"origHideIcons"`    // 启动时注册表 HideIcons 原值
	PanelMode        string `json:"panelMode"`        // 风险 1 当前面板模式
	ExplorerPid      uint32 `json:"explorerPid"`      // 用于判断主程序退出前 Explorer 是否重启过
	PositionsShuffle string `json:"positionsShuffle"` // 风险 6 打乱前的快照文件，未恢复时非空
}

var (
	stateMu sync.Mutex
	state   DemoState
)

func statePath() string { return filepath.Join(dataDir, "demo-state.json") }

func updateState(fn func(s *DemoState)) {
	stateMu.Lock()
	defer stateMu.Unlock()
	fn(&state)
	b, _ := json.MarshalIndent(state, "", "  ")
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, statePath())
	}
}

func readState() (DemoState, error) {
	var s DemoState
	b, err := os.ReadFile(statePath())
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
