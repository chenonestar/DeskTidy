//go:build windows

package main

import (
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processMemoryCountersEx struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type ProcMem struct {
	Pid        uint32  `json:"pid"`
	Name       string  `json:"name"`
	WorkingMB  float64 `json:"workingMB"`
	PrivateMB  float64 `json:"privateMB"`
	CPUPercent float64 `json:"cpuPercent"`
}

type MemoryReport struct {
	Seconds        int       `json:"seconds"`
	Processes      []ProcMem `json:"processes"`
	TotalWorkingMB float64   `json:"totalWorkingMB"`
	TotalPrivateMB float64   `json:"totalPrivateMB"`
	TotalCPU       float64   `json:"totalCpuPercent"`
}

// 本进程及其所有子孙进程（WebView2 的 msedgewebview2.exe 都是子孙进程）
func processTree() map[uint32]string {
	self := uint32(os.Getpid())
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return map[uint32]string{self: "self"}
	}
	defer windows.CloseHandle(snap)
	parent := map[uint32]uint32{}
	names := map[uint32]string{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
		names[e.ProcessID] = windows.UTF16ToString(e.ExeFile[:])
	}
	out := map[uint32]string{self: names[self]}
	for changed := true; changed; {
		changed = false
		for pid, pp := range parent {
			if _, in := out[pid]; !in && pid != pp {
				if _, pin := out[pp]; pin {
					out[pid] = names[pid]
					changed = true
				}
			}
		}
	}
	return out
}

func cpuTime(h windows.Handle) time.Duration {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return time.Duration(k.Nanoseconds() + u.Nanoseconds())
}

// measureMemory 统计 seconds 秒内的 CPU 占用，结束时读取内存
func measureMemory(seconds int) MemoryReport {
	rep := MemoryReport{Seconds: seconds}
	tree := processTree()
	handles := map[uint32]windows.Handle{}
	start := map[uint32]time.Duration{}
	for pid := range tree {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, pid)
		if err != nil {
			continue
		}
		handles[pid] = h
		start[pid] = cpuTime(h)
	}
	t0 := time.Now()
	time.Sleep(time.Duration(seconds) * time.Second)
	elapsed := time.Since(t0)
	for pid, h := range handles {
		var pmc processMemoryCountersEx
		pmc.Cb = uint32(unsafe.Sizeof(pmc))
		pK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb))
		cpu := float64(cpuTime(h)-start[pid]) / float64(elapsed) / float64(runtime.NumCPU()) * 100
		p := ProcMem{pid, tree[pid], float64(pmc.WorkingSetSize) / 1048576, float64(pmc.PrivateUsage) / 1048576, cpu}
		rep.Processes = append(rep.Processes, p)
		rep.TotalWorkingMB += p.WorkingMB
		rep.TotalPrivateMB += p.PrivateMB
		rep.TotalCPU += cpu
		windows.CloseHandle(h)
	}
	logf("内存：%d 个进程，工作集合计 %.1f MB，专用提交合计 %.1f MB，%d 秒内 CPU %.2f%%",
		len(rep.Processes), rep.TotalWorkingMB, rep.TotalPrivateMB, seconds, rep.TotalCPU)
	return rep
}
