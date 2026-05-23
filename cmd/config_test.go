package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigParsesArchiveAndThumbnailSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qbit-upload.yaml")
	data := []byte(`
dest_dir: /tmp/out
password: secret
archive:
  allow_tgz_fallback: false
  embedded_7z_dir: bundled
  split: true
  temp_dir: /tmp/qbit-upload-work
thumbnail:
  enabled: false
  ffmpeg: /usr/bin/ffmpeg
  ffprobe: /usr/bin/ffprobe
  columns: 3
  rows: 8
  width: 240
watch:
  enabled: true
  dirs:
    - /downloads/incoming
  stable_delay: 2s
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	oldConfig := config
	config = path
	defer func() { config = oldConfig }()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.Archive.AllowTgzFallback == nil || *cfg.Archive.AllowTgzFallback {
		t.Fatalf("archive allow_tgz_fallback = %#v, want false", cfg.Archive.AllowTgzFallback)
	}
	if cfg.Archive.Embedded7zDir != "bundled" {
		t.Fatalf("embedded_7z_dir = %q", cfg.Archive.Embedded7zDir)
	}
	if cfg.Archive.Split == nil || !*cfg.Archive.Split {
		t.Fatalf("archive split = %#v, want true", cfg.Archive.Split)
	}
	if cfg.Archive.TempDir != "/tmp/qbit-upload-work" {
		t.Fatalf("archive temp_dir = %q", cfg.Archive.TempDir)
	}
	if cfg.Thumbnail.Enabled == nil || *cfg.Thumbnail.Enabled {
		t.Fatalf("thumbnail enabled = %#v, want false", cfg.Thumbnail.Enabled)
	}
	if cfg.Thumbnail.FFmpeg != "/usr/bin/ffmpeg" || cfg.Thumbnail.FFprobe != "/usr/bin/ffprobe" {
		t.Fatalf("thumbnail tools = %#v", cfg.Thumbnail)
	}
	if cfg.Thumbnail.Columns != 3 || cfg.Thumbnail.Rows != 8 || cfg.Thumbnail.Width != 240 {
		t.Fatalf("thumbnail grid = %#v", cfg.Thumbnail)
	}
	if cfg.Watch.Enabled == nil || !*cfg.Watch.Enabled {
		t.Fatalf("watch enabled = %#v, want true", cfg.Watch.Enabled)
	}
	if len(cfg.Watch.Dirs) != 1 || cfg.Watch.Dirs[0] != "/downloads/incoming" {
		t.Fatalf("watch dirs = %#v", cfg.Watch.Dirs)
	}
	if cfg.Watch.StableDelay != "2s" {
		t.Fatalf("watch stable_delay = %q", cfg.Watch.StableDelay)
	}
}

func TestResolveOptionsUsesArchiveTempDir(t *testing.T) {
	cfg := appConfig{
		DestDir: "/tmp/out",
		Archive: archiveConfig{
			TempDir: "/tmp/qbit-upload-work",
		},
	}

	opts, err := resolveOptions(newRootCmd(), cfg)
	if err != nil {
		t.Fatalf("resolveOptions returned error: %v", err)
	}
	if opts.Archive.TempDir != "/tmp/qbit-upload-work" {
		t.Fatalf("Archive.TempDir = %q", opts.Archive.TempDir)
	}
}

func TestResolveWatchOptionsParsesStableDelay(t *testing.T) {
	enabled := true
	opts, err := resolveWatchOptions(watchConfig{
		Enabled:     &enabled,
		Dirs:        []string{"/downloads/incoming"},
		StableDelay: "2s",
	})
	if err != nil {
		t.Fatalf("resolveWatchOptions returned error: %v", err)
	}
	if !opts.Enabled {
		t.Fatalf("Enabled = false, want true")
	}
	if opts.StableDelay.String() != "2s" {
		t.Fatalf("StableDelay = %s, want 2s", opts.StableDelay)
	}
}
