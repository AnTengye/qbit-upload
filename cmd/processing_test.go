package cmd

import (
	"path/filepath"
	"reflect"
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
