//go:build windows

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 风险 5：加载 N 个真实图标到 WebView2，测量常驻内存

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmap struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

type IconEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var (
	iconMu    sync.RWMutex
	iconCache = map[int][]byte{}
)

// 图标来源：两个桌面 → 开始菜单快捷方式 → System32 下的程序，凑满 n 个
func iconSources(n int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) bool {
		k := strings.ToLower(p)
		if !seen[k] && !strings.EqualFold(filepath.Base(p), "desktop.ini") {
			seen[k] = true
			out = append(out, p)
		}
		return len(out) >= n
	}
	for _, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Desktop, windows.FOLDERID_PublicDesktop} {
		if dir, err := windows.KnownFolderPath(id, 0); err == nil {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if add(filepath.Join(dir, e.Name())) {
					return out
				}
			}
		}
	}
	for _, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Programs, windows.FOLDERID_CommonPrograms} {
		dir, err := windows.KnownFolderPath(id, 0)
		if err != nil {
			continue
		}
		done := false
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if done || err != nil {
				return filepath.SkipAll
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".lnk") {
				done = add(p)
			}
			return nil
		})
		if done {
			return out
		}
	}
	sys, _ := windows.GetSystemDirectory()
	matches, _ := filepath.Glob(filepath.Join(sys, "*.exe"))
	for _, p := range matches {
		if add(p) {
			break
		}
	}
	return out
}

func extractIcon(path string, size int32) ([]byte, error) {
	var factory uintptr
	hr, _, _ := pSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(utf16(path))), 0,
		uintptr(unsafe.Pointer(&iidShellItemImageFact)), uintptr(unsafe.Pointer(&factory)))
	if hr != sOK {
		return nil, hrErr("SHCreateItemFromParsingName", hr)
	}
	defer release(factory)
	var hbm uintptr
	// GetImage(SIZE, SIIGBF, HBITMAP*)：SIZE 按值传递，x64 上打包进一个寄存器
	sz := uintptr(uint32(size)) | uintptr(uint32(size))<<32
	if hr = vcall(factory, 3, sz, 0, uintptr(unsafe.Pointer(&hbm))); hr != sOK {
		return nil, hrErr("IShellItemImageFactory::GetImage", hr)
	}
	defer pDeleteObject.Call(hbm)
	return hbitmapToPNG(hbm)
}

func hbitmapToPNG(hbm uintptr) ([]byte, error) {
	var bm bitmap
	pGetObjectW.Call(hbm, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm)))
	w, h := int(bm.Width), int(bm.Height)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("无效的位图尺寸 %dx%d", w, h)
	}
	bi := bitmapInfoHeader{Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	buf := make([]byte, w*h*4)
	hdc, _, _ := pGetDC.Call(0)
	pGetDIBits.Call(hdc, hbm, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), 0)
	pReleaseDC.Call(0, hdc)

	img := image.NewRGBA(image.Rect(0, 0, w, h)) // 预乘 alpha，与 Shell 返回的格式一致
	hasAlpha := false
	for i := 3; i < len(buf); i += 4 {
		if buf[i] != 0 {
			hasAlpha = true
			break
		}
	}
	for i := 0; i < len(buf); i += 4 {
		a := buf[i+3]
		if !hasAlpha {
			a = 255
		}
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = buf[i+2], buf[i+1], buf[i], a
	}
	var out bytes.Buffer
	err := png.Encode(&out, img)
	return out.Bytes(), err
}

type IconLoadResult struct {
	Requested int         `json:"requested"`
	Loaded    int         `json:"loaded"`
	Failed    int         `json:"failed"`
	Ms        int64       `json:"ms"`
	Items     []IconEntry `json:"items"`
}

func loadIcons(n int) IconLoadResult {
	res := IconLoadResult{Requested: n}
	t0 := time.Now()
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		pCoInitializeEx.Call(0, coinitApartmentThreaded)
		defer pCoUninitialize.Call()
		cache := map[int][]byte{}
		for i, p := range iconSources(n) {
			b, err := extractIcon(p, 48)
			if err != nil {
				res.Failed++
				continue
			}
			cache[i] = b
			res.Items = append(res.Items, IconEntry{i, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))})
		}
		iconMu.Lock()
		iconCache = cache
		iconMu.Unlock()
		close(done)
	}()
	<-done
	res.Loaded = len(res.Items)
	res.Ms = time.Since(t0).Milliseconds()
	logf("加载图标：请求 %d，成功 %d，失败 %d，耗时 %d ms", n, res.Loaded, res.Failed, res.Ms)
	return res
}

// /icons/<id>.png 由这里提供，相当于正式版的 PNG 图标缓存
type iconHandler struct{}

func (iconHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/icons/"), ".png")
	id, err := strconv.Atoi(name)
	iconMu.RLock()
	b, ok := iconCache[id]
	iconMu.RUnlock()
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(b)
}
