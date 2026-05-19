//go:build linux

package cmd

import "os"

func getAvailableMemoryMB() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	available, err := parseMemAvailableMB(f)
	if err != nil {
		return 0
	}
	return available
}
