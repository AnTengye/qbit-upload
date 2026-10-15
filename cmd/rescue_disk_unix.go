//go:build !windows

package cmd

import "syscall"

func rescueDiskFree(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(dir, &stat)
	return uint64(stat.Bavail) * uint64(stat.Bsize), err
}
