package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveThumbnailDefaults(t *testing.T) {
	opts, err := resolveThumbnailOptions(thumbnailConfig{})
	if err != nil {
		t.Fatalf("resolveThumbnailOptions returned error: %v", err)
	}
	if !opts.Enabled || opts.FFmpeg != "ffmpeg" || opts.FFprobe != "ffprobe" {
		t.Fatalf("unexpected tool defaults: %#v", opts)
	}
	if opts.Columns != 4 || opts.Rows != 15 || opts.Width != 320 {
		t.Fatalf("unexpected grid defaults: %#v", opts)
	}
}

func TestResolveThumbnailRejectsInvalidGrid(t *testing.T) {
	_, err := resolveThumbnailOptions(thumbnailConfig{Enabled: boolPtr(true), Columns: -1, Rows: 15, Width: 320})
	if err == nil {
		t.Fatal("resolveThumbnailOptions returned nil error for invalid columns")
	}
}

func TestThumbnailOutputPathsAvoidDuplicateVideoBaseNames(t *testing.T) {
	dest := t.TempDir()
	files := []string{"movie.mp4", filepath.Join("nested", "movie.mkv")}
	got, err := planThumbnailOutputs("source", dest, files)
	if err != nil {
		t.Fatalf("planThumbnailOutputs returned error: %v", err)
	}
	if filepath.Base(got[0].OutputPath) != "source-movie-thumbnail.jpg" {
		t.Fatalf("first output = %s", got[0].OutputPath)
	}
	if filepath.Base(got[1].OutputPath) != "source-movie-2-thumbnail.jpg" {
		t.Fatalf("second output = %s", got[1].OutputPath)
	}
}

func TestBuildFFmpegThumbnailArgs(t *testing.T) {
	args := buildFFmpegThumbnailArgs("in.mp4", "out.jpg", thumbnailOptions{Columns: 4, Rows: 15, Width: 320}, "name.mp4", "1.5 GiB", "01:02:03")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i in.mp4", "tile=4x15", "scale=320:-1", "out.jpg"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ffmpeg args %q missing %q", joined, want)
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}
