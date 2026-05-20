package cmd

import (
	"strings"
	"testing"
)

func TestProgressSnapshotKeepsLatestOutput(t *testing.T) {
	tracker := newProgressSnapshot(20)

	if _, err := tracker.Write([]byte("first line\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if _, err := tracker.Write([]byte("second line with progress 42%\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	got := tracker.Latest()
	if !strings.Contains(got, "progress 42%") {
		t.Fatalf("Latest() = %q, want latest output", got)
	}
	if strings.Contains(got, "first line") {
		t.Fatalf("Latest() = %q, should trim older output", got)
	}
}

func TestProgressSnapshotReportsNoOutput(t *testing.T) {
	tracker := newProgressSnapshot(20)

	got := tracker.Latest()
	if got != "暂无输出" {
		t.Fatalf("Latest() = %q, want no output marker", got)
	}
}
