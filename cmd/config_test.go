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
thumbnail:
  enabled: false
  ffmpeg: /usr/bin/ffmpeg
  ffprobe: /usr/bin/ffprobe
  columns: 3
  rows: 8
  width: 240
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
	if cfg.Thumbnail.Enabled == nil || *cfg.Thumbnail.Enabled {
		t.Fatalf("thumbnail enabled = %#v, want false", cfg.Thumbnail.Enabled)
	}
	if cfg.Thumbnail.FFmpeg != "/usr/bin/ffmpeg" || cfg.Thumbnail.FFprobe != "/usr/bin/ffprobe" {
		t.Fatalf("thumbnail tools = %#v", cfg.Thumbnail)
	}
	if cfg.Thumbnail.Columns != 3 || cfg.Thumbnail.Rows != 8 || cfg.Thumbnail.Width != 240 {
		t.Fatalf("thumbnail grid = %#v", cfg.Thumbnail)
	}
}
