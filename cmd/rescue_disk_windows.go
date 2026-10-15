//go:build windows

package cmd

import (
	"syscall"
	"unsafe"
)

func rescueDiskFree(dir string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var available uint64
	ok, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW").Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&available)), 0, 0)
	if ok == 0 {
		return 0, callErr
	}
	return available, nil
}
