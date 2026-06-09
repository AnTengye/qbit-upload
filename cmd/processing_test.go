package cmd

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareArchiveInputForDirectoryUsesDirectoryBaseAndDeletesDirectory(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Movie.mp4"), stringsRepeat("x", 2))

	input, err := prepareArchiveInput(root, 1)
	if err != nil {
		t.Fatalf("prepareArchiveInput returned error: %v", err)
	}

	if input.SourceDir != root {
		t.Fatalf("SourceDir = %q, want %q", input.SourceDir, root)
	}
	if input.SourceBase != filepath.Base(root) {
		t.Fatalf("SourceBase = %q, want %q", input.SourceBase, filepath.Base(root))
	}
	if input.DeletePath != root {
		t.Fatalf("DeletePath = %q, want %q", input.DeletePath, root)
	}
	if !reflect.DeepEqual(input.Files, []string{"Movie.mp4"}) {
		t.Fatalf("Files = %#v", input.Files)
	}
}

func TestPrepareArchiveInputForSingleVideoUsesVideoBaseAndDeletesFile(t *testing.T) {
	root := t.TempDir()
	video := filepath.Join(root, "Movie.mp4")
	mustWriteFile(t, video, stringsRepeat("x", 2))

	input, err := prepareArchiveInput(video, 1)
	if err != nil {
		t.Fatalf("prepareArchiveInput returned error: %v", err)
	}

	if input.SourceDir != root {
		t.Fatalf("SourceDir = %q, want parent %q", input.SourceDir, root)
	}
	if input.SourceBase != "Movie" {
		t.Fatalf("SourceBase = %q, want Movie", input.SourceBase)
	}
	if input.DeletePath != video {
		t.Fatalf("DeletePath = %q, want %q", input.DeletePath, video)
	}
	if !reflect.DeepEqual(input.Files, []string{"Movie.mp4"}) {
		t.Fatalf("Files = %#v", input.Files)
	}

	outputs, err := planArchiveOutputs(input.SourceBase, t.TempDir(), input.Files, archiveFormat7z, false)
	if err != nil {
		t.Fatalf("planArchiveOutputs returned error: %v", err)
	}
	if got := filepath.Base(outputs[0].Path); got != "Movie.7z" {
		t.Fatalf("archive name = %q, want Movie.7z", got)
	}
}

func TestWatchedTopLevelVideoFileProcessesTheFileItself(t *testing.T) {
	watchDir := t.TempDir()
	video := filepath.Join(watchDir, "Movie.mp4")
	mustWriteFile(t, video, stringsRepeat("x", 2))

	got, err := watchedProcessingPath(watchDir, video)
	if err != nil {
		t.Fatalf("watchedProcessingPath returned error: %v", err)
	}
	if got != video {
		t.Fatalf("watchedProcessingPath = %q, want %q", got, video)
	}
}

func TestWatchedNestedFileProcessesTopChildDirectory(t *testing.T) {
	watchDir := t.TempDir()
	video := filepath.Join(watchDir, "Show", "Season 1", "Episode.mkv")
	mustWriteFile(t, video, stringsRepeat("x", 2))

	got, err := watchedProcessingPath(watchDir, video)
	if err != nil {
		t.Fatalf("watchedProcessingPath returned error: %v", err)
	}
	want := filepath.Join(watchDir, "Show")
	if got != want {
		t.Fatalf("watchedProcessingPath = %q, want %q", got, want)
	}
}

func stringsRepeat(s string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += s
	}
	return out
}

func TestProcessSourceDryRunRespectsDeleteSource(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Movie.mp4"), stringsRepeat("x", 2))

	// Capturing stdout to verify dry-run prints
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe error: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	opts := runOptions{
		DestDir:   t.TempDir(),
		MinSizeMB: 0,
		DryRun:    true,
	}
	opts.Archive.DeleteSource = false
	opts.Archive.AllowTgzFallback = true

	// Call processSource with DeleteSource = false
	if err := processSource(root, opts); err != nil {
		t.Fatalf("processSource DryRun returned error: %v", err)
	}

	w.Close()
	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	output := string(outBytes)

	if !strings.Contains(output, "保留源路径") {
		t.Errorf("expected output to contain '保留源路径', got: %s", output)
	}
	if strings.Contains(output, "将删除源路径") {
		t.Errorf("expected output NOT to contain '将删除源路径', got: %s", output)
	}

	// Call processSource with DeleteSource = true
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe error: %v", err)
	}
	os.Stdout = w2

	opts.Archive.DeleteSource = true
	if err := processSource(root, opts); err != nil {
		t.Fatalf("processSource DryRun returned error: %v", err)
	}

	w2.Close()
	outBytes2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	output2 := string(outBytes2)

	if !strings.Contains(output2, "将删除源路径") {
		t.Errorf("expected output to contain '将删除源路径', got: %s", output2)
	}
	if strings.Contains(output2, "保留源路径") {
		t.Errorf("expected output NOT to contain '保留源路径', got: %s", output2)
	}
}

func TestProcessSourceDryRunSkipsExistingSplitArchiveAndContinues(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "Exists.mp4"), stringsRepeat("x", 2))
	mustWriteFile(t, filepath.Join(root, "New.mp4"), stringsRepeat("x", 2))
	dest := t.TempDir()
	mustWriteFile(t, filepath.Join(dest, "Exists.7z"), "already archived")

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe error: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	opts := runOptions{
		DestDir:   dest,
		MinSizeMB: 0,
		DryRun:    true,
	}
	opts.Archive.AllowTgzFallback = true
	opts.Archive.Split = true
	opts.Archive.DeleteSource = true

	if err := processSource(root, opts); err != nil {
		t.Fatalf("processSource DryRun returned error: %v", err)
	}

	w.Close()
	outBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	output := string(outBytes)

	if strings.Contains(output, "Exists.7z") {
		t.Errorf("expected existing archive to be skipped from planned outputs, got: %s", output)
	}
	if !strings.Contains(output, "New.7z") {
		t.Errorf("expected non-conflicting archive to remain planned, got: %s", output)
	}
	if strings.Contains(output, "将删除源路径") {
		t.Errorf("expected partial split processing not to delete whole source path, got: %s", output)
	}
	if !strings.Contains(output, "将删除已处理视频数量: 1") {
		t.Errorf("expected partial split processing to delete only processed videos, got: %s", output)
	}
}
