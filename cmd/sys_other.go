//go:build !windows

package cmd

// getAvailableMemoryMB returns 0 on non-Windows platforms,
// so the graceful default dynamic logic falls back seamlessly.
func getAvailableMemoryMB() uint64 {
	return 0
}
