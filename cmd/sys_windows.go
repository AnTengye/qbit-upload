//go:build windows

package cmd

import (
	"syscall"
	"unsafe"
)

// getAvailableMemoryMB returns the currently available physical memory in MB.
// It returns 0 on failure.
func getAvailableMemoryMB() uint64 {
	mod := syscall.NewLazyDLL("kernel32.dll")
	proc := mod.NewProc("GlobalMemoryStatusEx")

	var memStruct struct {
		cbSize                  uint32
		dwMemoryLoad            uint32
		ullTotalPhys            uint64
		ullAvailPhys            uint64
		ullTotalPageFile        uint64
		ullAvailPageFile        uint64
		ullTotalVirtual         uint64
		ullAvailVirtual         uint64
		ullAvailExtendedVirtual uint64
	}

	memStruct.cbSize = uint32(unsafe.Sizeof(memStruct))

	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&memStruct)))
	if ret == 0 {
		return 0
	}
	return memStruct.ullAvailPhys / (1024 * 1024)
}
