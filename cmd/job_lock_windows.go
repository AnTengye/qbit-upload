//go:build windows

package cmd

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

func lockJobStore(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	var overlapped syscall.Overlapped
	ok, _, lockErr := lock.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if ok == 0 {
		f.Close()
		return nil, fmt.Errorf("任务队列正被另一个进程使用: %w", lockErr)
	}
	return func() { f.Close() }, nil
}

func syncJobDirectory(string) error { return nil }
