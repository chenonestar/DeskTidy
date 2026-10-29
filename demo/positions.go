//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 风险 6：通过公开 COM 接口读写桌面图标坐标（IShellWindows → IShellBrowser → IShellView → IFolderView）

var (
	clsidShellWindows     = mustGUID("{9BA05972-F6A8-11CF-A442-00A0C90A8F39}")
	iidShellWindows       = mustGUID("{85CB6900-4D95-11CF-960C-0080C7F4EE85}")
	iidServiceProvider    = mustGUID("{6D5140C1-7436-11CE-8034-00AA006009FA}")
	sidSTopLevelBrowser   = mustGUID("{4C96BE40-915C-11CF-99D3-00AA004AE837}")
	iidShellBrowser       = mustGUID("{000214E2-0000-0000-C000-000000000046}")
	iidFolderView         = mustGUID("{CDE725B0-CCC9-4519-917E-325D72FAB4CE}")
	iidFolderView2        = mustGUID("{1AF3A467-214F-4298-908E-06B03E0B39F9}")
	iidShellFolder        = mustGUID("{000214E6-0000-0000-C000-000000000046}")
	iidEnumIDList         = mustGUID("{000214F2-0000-0000-C000-000000000046}")
	iidShellItemImageFact = mustGUID("{BCC18B79-BA16-442F-80C4-8A59C30C463B}")
)

const (
	swcDesktop        = 8
	swfoNeedDispatch  = 1
	svgioAllView      = 2
	shgdnForParsing   = 0x8000
	svsiPositionItem  = 0x80
	fwfAutoArrange    = 0x1
	fwfSnapToGrid     = 0x4
	vtI4              = 3
	sOK               = 0
	sFalse            = 1
	idxFindWindowSW   = 15 // IShellWindows
	idxQueryService   = 3  // IServiceProvider
	idxQueryActiveSV  = 15 // IShellBrowser
	idxFVGetFolder    = 5  // IFolderView
	idxFVItems        = 8
	idxFVGetItemPos   = 11
	idxFVGetAutoArr   = 14
	idxFVSelAndPos    = 16
	idxFV2GetFlags    = 25 // IFolderView2::GetCurrentFolderFlags
	idxEnumNext       = 3  // IEnumIDList
	idxSFGetDisplayOf = 11 // IShellFolder
)

type variant struct {
	VT  uint16
	_   [3]uint16
	Val int64
	_   int64
}

type IconPos struct {
	Name string `json:"name"` // 解析名（完整路径或 ::{CLSID}）
	X    int32  `json:"x"`
	Y    int32  `json:"y"`
}

type PositionSnapshot struct {
	Time        string        `json:"time"`
	AutoArrange bool          `json:"autoArrange"`
	SnapToGrid  bool          `json:"snapToGrid"`
	Dpi         uint32        `json:"dpi"`
	Monitors    []MonitorDesc `json:"monitors"`
	Items       []IconPos     `json:"items"`
	File        string        `json:"file"`
	BagsBackup  string        `json:"bagsBackup"`
}

type desktopView struct {
	fv, fv2, sf uintptr
	items       []uintptr // PIDL
	names       []string
}

func (v *desktopView) close() {
	for _, p := range v.items {
		coTaskMemFree(p)
	}
	release(v.sf)
	release(v.fv2)
	release(v.fv)
}

// withDesktopView 在单独的 STA 线程里打开桌面视图并枚举所有图标
func withDesktopView(fn func(v *desktopView) error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pCoInitializeEx.Call(0, coinitApartmentThreaded)
		defer pCoUninitialize.Call()
		v, err := openDesktopView()
		if err != nil {
			errc <- err
			return
		}
		defer v.close()
		errc <- fn(v)
	}()
	return <-errc
}

func hrErr(what string, hr uintptr) error {
	return fmt.Errorf("%s 失败：HRESULT 0x%08X", what, uint32(hr))
}

func openDesktopView() (*desktopView, error) {
	var sw uintptr
	hr, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidShellWindows)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidShellWindows)), uintptr(unsafe.Pointer(&sw)))
	if hr != sOK {
		return nil, hrErr("CoCreateInstance(ShellWindows)", hr)
	}
	defer release(sw)

	loc := variant{VT: vtI4} // CSIDL_DESKTOP = 0
	var empty variant
	var hwnd int32
	var disp uintptr
	hr = vcall(sw, idxFindWindowSW, uintptr(unsafe.Pointer(&loc)), uintptr(unsafe.Pointer(&empty)),
		swcDesktop, uintptr(unsafe.Pointer(&hwnd)), swfoNeedDispatch, uintptr(unsafe.Pointer(&disp)))
	if hr != sOK || disp == 0 {
		return nil, hrErr("IShellWindows::FindWindowSW", hr)
	}
	defer release(disp)

	sp, hr32 := queryInterface(disp, &iidServiceProvider)
	if hr32 != sOK {
		return nil, hrErr("QI(IServiceProvider)", uintptr(hr32))
	}
	defer release(sp)
	var browser uintptr
	hr = vcall(sp, idxQueryService, uintptr(unsafe.Pointer(&sidSTopLevelBrowser)), uintptr(unsafe.Pointer(&iidShellBrowser)), uintptr(unsafe.Pointer(&browser)))
	if hr != sOK {
		return nil, hrErr("QueryService(IShellBrowser)", hr)
	}
	defer release(browser)
	var view uintptr
	hr = vcall(browser, idxQueryActiveSV, uintptr(unsafe.Pointer(&view)))
	if hr != sOK {
		return nil, hrErr("IShellBrowser::QueryActiveShellView", hr)
	}
	defer release(view)

	v := &desktopView{}
	if v.fv, hr32 = queryInterface(view, &iidFolderView); hr32 != sOK {
		return nil, hrErr("QI(IFolderView)", uintptr(hr32))
	}
	v.fv2, _ = queryInterface(view, &iidFolderView2)
	if hr = vcall(v.fv, idxFVGetFolder, uintptr(unsafe.Pointer(&iidShellFolder)), uintptr(unsafe.Pointer(&v.sf))); hr != sOK {
		v.close()
		return nil, hrErr("IFolderView::GetFolder", hr)
	}
	var enum uintptr
	if hr = vcall(v.fv, idxFVItems, svgioAllView, uintptr(unsafe.Pointer(&iidEnumIDList)), uintptr(unsafe.Pointer(&enum))); hr != sOK {
		v.close()
		return nil, hrErr("IFolderView::Items", hr)
	}
	defer release(enum)
	for {
		var pidl uintptr
		var fetched uint32
		if vcall(enum, idxEnumNext, 1, uintptr(unsafe.Pointer(&pidl)), uintptr(unsafe.Pointer(&fetched))) != sOK || fetched == 0 {
			break
		}
		v.items = append(v.items, pidl)
		v.names = append(v.names, v.displayName(pidl))
	}
	return v, nil
}

func (v *desktopView) displayName(pidl uintptr) string {
	var strret [34]uint64 // STRRET（272 字节，按 8 字节对齐）
	if vcall(v.sf, idxSFGetDisplayOf, pidl, shgdnForParsing, uintptr(unsafe.Pointer(&strret[0]))) != sOK {
		return ""
	}
	var psz *uint16
	pStrRetToStrW.Call(uintptr(unsafe.Pointer(&strret[0])), pidl, uintptr(unsafe.Pointer(&psz)))
	if psz == nil {
		return ""
	}
	defer coTaskMemFree(uintptr(unsafe.Pointer(psz)))
	return windows.UTF16PtrToString(psz)
}

func (v *desktopView) position(pidl uintptr) point {
	var pt point
	vcall(v.fv, idxFVGetItemPos, pidl, uintptr(unsafe.Pointer(&pt)))
	return pt
}

func (v *desktopView) setPosition(pidl uintptr, pt point) uintptr {
	return vcall(v.fv, idxFVSelAndPos, 1, uintptr(unsafe.Pointer(&pidl)), uintptr(unsafe.Pointer(&pt)), svsiPositionItem)
}

func (v *desktopView) flags() (autoArrange, snap bool) {
	if v.fv2 != 0 {
		var f uint32
		if vcall(v.fv2, idxFV2GetFlags, uintptr(unsafe.Pointer(&f))) == sOK {
			return f&fwfAutoArrange != 0, f&fwfSnapToGrid != 0
		}
	}
	return vcall(v.fv, idxFVGetAutoArr) == sOK, false
}

func (v *desktopView) readAll() []IconPos {
	out := make([]IconPos, len(v.items))
	for i, p := range v.items {
		pt := v.position(p)
		out[i] = IconPos{v.names[i], pt.X, pt.Y}
	}
	return out
}

// ---- 快照 ----

func takeSnapshot() (*PositionSnapshot, error) {
	snap := &PositionSnapshot{Time: time.Now().Format("2006-01-02 15:04:05"), Monitors: monitors()}
	if lv := curListView.Load(); lv != 0 {
		snap.Dpi = dpiForWindow(lv)
	}
	err := withDesktopView(func(v *desktopView) error {
		snap.AutoArrange, snap.SnapToGrid = v.flags()
		snap.Items = v.readAll()
		return nil
	})
	if err != nil {
		return nil, err
	}
	stamp := time.Now().Format("20060102-150405")
	snap.File = filepath.Join(dataDir, "snapshots", stamp+".json")
	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(snap.File, b, 0o644); err != nil {
		return nil, err
	}
	// 退路方案的备份：Explorer 自己保存的桌面布局（只读导出，不修改）
	bags := filepath.Join(dataDir, "snapshots", stamp+"-bags.reg")
	cmd := exec.Command("reg.exe", "export", `HKCU\Software\Microsoft\Windows\Shell\Bags\1\Desktop`, bags, "/y")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if cmd.Run() == nil {
		snap.BagsBackup = bags
	}
	logf("图标位置快照：%d 个图标，自动排列=%v，对齐网格=%v → %s", len(snap.Items), snap.AutoArrange, snap.SnapToGrid, snap.File)
	return snap, nil
}

type RestoreResult struct {
	Applied  int      `json:"applied"`
	Missing  []string `json:"missing"`  // 快照里有、现在桌面上没有的
	Mismatch []string `json:"mismatch"` // 写回后坐标仍不一致的
	MaxDelta int32    `json:"maxDelta"`
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func restorePositions(file string) (*RestoreResult, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var snap PositionSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	want := map[string]point{}
	for _, it := range snap.Items {
		want[it.Name] = point{it.X, it.Y}
	}
	res := &RestoreResult{}
	err = withDesktopView(func(v *desktopView) error {
		seen := map[string]bool{}
		for i, p := range v.items {
			pt, ok := want[v.names[i]]
			if !ok {
				continue
			}
			seen[v.names[i]] = true
			if hr := v.setPosition(p, pt); hr == sOK {
				res.Applied++
			}
		}
		for name := range want {
			if !seen[name] {
				res.Missing = append(res.Missing, name)
			}
		}
		time.Sleep(500 * time.Millisecond)
		for _, it := range v.readAll() {
			w, ok := want[it.Name]
			if !ok {
				continue
			}
			d := max(abs32(it.X-w.X), abs32(it.Y-w.Y))
			res.MaxDelta = max(res.MaxDelta, d)
			if d > 2 {
				res.Mismatch = append(res.Mismatch, fmt.Sprintf("%s：期望 (%d,%d) 实际 (%d,%d)", filepath.Base(it.Name), w.X, w.Y, it.X, it.Y))
			}
		}
		return nil
	})
	return res, err
}

// ---- 风险 6 自动测试：快照 → 打乱 → 写回 → 比对 ----

func runRisk6Tests() []TestStep {
	var out []TestStep
	if wantHidden.Load() {
		showIcons()
		out = append(out, info("准备", "已先恢复显示系统图标，方便肉眼观察"))
	}
	snap, err := takeSnapshot()
	if err != nil {
		return append(out, step("读取图标坐标", false, "%v", err))
	}
	out = append(out, step("读取图标坐标", len(snap.Items) > 0, "%d 个图标；自动排列=%v，对齐网格=%v；DPI=%d；显示器 %d 个", len(snap.Items), snap.AutoArrange, snap.SnapToGrid, snap.Dpi, len(snap.Monitors)))
	out = append(out, step("备份注册表 Bags\\1\\Desktop", snap.BagsBackup != "", "%s", snap.BagsBackup))
	for _, m := range snap.Monitors {
		n := 0
		for _, it := range snap.Items {
			if it.X >= m.Left && it.X < m.Right && it.Y >= m.Top && it.Y < m.Bottom {
				n++
			}
		}
		out = append(out, info("显示器坐标", "(%d,%d)-(%d,%d) 主屏=%v：落在其中的图标 %d 个", m.Left, m.Top, m.Right, m.Bottom, m.Primary, n))
	}
	if snap.AutoArrange {
		return append(out, TestStep{"打乱并写回", "skip", "桌面开启了“自动排列图标”，坐标由系统决定，无法测试写回；可在桌面右键 → 查看 中关闭后重测"})
	}
	if len(snap.Items) < 2 {
		return append(out, TestStep{"打乱并写回", "skip", "桌面图标少于 2 个"})
	}

	// 先记录再修改：守护进程据此在异常退出时写回
	updateState(func(s *DemoState) { s.PositionsShuffle = snap.File })
	moved := 0
	err = withDesktopView(func(v *desktopView) error {
		// 把图标坐标倒序互换，保证每个新坐标都是原本合法的格子
		pos := map[string]IconPos{}
		for _, it := range snap.Items {
			pos[it.Name] = it
		}
		n := len(snap.Items)
		for i, p := range v.items {
			if _, ok := pos[v.names[i]]; !ok {
				continue
			}
			target := snap.Items[n-1-indexOf(snap.Items, v.names[i])]
			if v.setPosition(p, point{target.X, target.Y}) == sOK {
				moved++
			}
		}
		return nil
	})
	time.Sleep(1500 * time.Millisecond)
	out = append(out, step("打乱图标位置", err == nil && moved > 0, "已移动 %d 个（肉眼应能看到桌面图标被打乱）", moved))

	res, err := restorePositions(snap.File)
	if err != nil {
		return append(out, step("按快照写回", false, "%v", err))
	}
	updateState(func(s *DemoState) { s.PositionsShuffle = "" })
	out = append(out, step("按快照写回", res.Applied == len(snap.Items)-len(res.Missing), "写回 %d 个，缺失 %d 个", res.Applied, len(res.Missing)))
	detail := fmt.Sprintf("最大偏差 %d px", res.MaxDelta)
	if len(res.Mismatch) > 0 {
		detail += "；不一致：" + fmt.Sprint(res.Mismatch)
	}
	out = append(out, step("写回后坐标与快照一致", len(res.Mismatch) == 0, "%s", detail))
	return out
}

func indexOf(items []IconPos, name string) int {
	for i, it := range items {
		if it.Name == name {
			return i
		}
	}
	return 0
}
