package main

import (
	"syscall"
	"unsafe"
)

// Whole-process CPU and resident memory, including backend and load generator.
func processUsage() (float64, uint64) {
	handle := ^uintptr(0)
	kernel := syscall.NewLazyDLL("kernel32.dll")
	var created, exited, k, u syscall.Filetime
	ok, _, _ := kernel.NewProc("GetProcessTimes").Call(handle, uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if ok == 0 {
		return -1, 0
	}
	ticks := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	var memory struct {
		Size, Faults                                                                       uint32
		Peak, WorkingSet, PagedPeak, Paged, NonPagedPeak, NonPaged, Pagefile, PagefilePeak uintptr
	}
	memory.Size = uint32(unsafe.Sizeof(memory))
	syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo").Call(handle, uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size))
	return float64(ticks(k)+ticks(u)) / 1e7, uint64(memory.WorkingSet)
}
