package cmd

import (
	"strings"
	"testing"
)

func TestParseMemAvailableMB(t *testing.T) {
	data := strings.NewReader("MemTotal:       16384256 kB\nMemFree:         1024000 kB\nMemAvailable:    3145728 kB\n")

	got, err := parseMemAvailableMB(data)
	if err != nil {
		t.Fatalf("parseMemAvailableMB returned error: %v", err)
	}
	if got != 3072 {
		t.Fatalf("parseMemAvailableMB = %d, want 3072", got)
	}
}

func TestParseMemAvailableMBMissing(t *testing.T) {
	_, err := parseMemAvailableMB(strings.NewReader("MemTotal: 1 kB\n"))
	if err == nil {
		t.Fatal("parseMemAvailableMB returned nil error for missing MemAvailable")
	}
}
