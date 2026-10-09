package cmd

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveTorrentFilterOptions(t *testing.T) {
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_URL", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_USERNAME", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_PASSWORD", "")
	for _, tc := range []struct {
		name string
		cfg  torrentFilterConfig
		bad  bool
	}{
		{"defaults", torrentFilterConfig{BaseURL: "http://localhost:8080/qb/"}, false},
		{"missing URL", torrentFilterConfig{}, true},
		{"credentials in URL", torrentFilterConfig{BaseURL: "http://user:secret@localhost"}, true},
		{"query in URL", torrentFilterConfig{BaseURL: "http://localhost/?password=secret"}, true},
		{"invalid scheme", torrentFilterConfig{BaseURL: "ftp://localhost"}, true},
		{"zero size", torrentFilterConfig{BaseURL: "http://localhost", MinSizeMB: filterSizePointer(0)}, true},
		{"negative size", torrentFilterConfig{BaseURL: "http://localhost", MinSizeMB: filterSizePointer(-1)}, true},
		{"overflow", torrentFilterConfig{BaseURL: "http://localhost", MinSizeMB: filterSizePointer(math.MaxInt64)}, true},
		{"custom size", torrentFilterConfig{BaseURL: "http://localhost", MinSizeMB: filterSizePointer(256)}, false},
		{"invalid interval", torrentFilterConfig{BaseURL: "http://localhost", PollInterval: "0s"}, true},
		{"invalid timeout", torrentFilterConfig{BaseURL: "http://localhost", RequestTimeout: "invalid"}, true},
		{"multiple tags", torrentFilterConfig{BaseURL: "http://localhost", Tag: "first,second"}, true},
		{"incomplete credentials", torrentFilterConfig{BaseURL: "http://localhost", Username: "user"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := resolveTorrentFilterOptions(tc.cfg)
			if (err != nil) != tc.bad {
				t.Fatalf("resolve error=%v, want bad=%v", err, tc.bad)
			}
			if err != nil {
				return
			}
			if tc.name == "defaults" && (opts.MinSizeBytes != 100*mb || opts.Tag != "filter-small" ||
				opts.PollInterval != 5*time.Second || opts.RetryInterval != 30*time.Second || opts.BaseURL != "http://localhost:8080/qb") {
				t.Fatalf("wrong defaults: %#v", opts)
			}
			if tc.name == "custom size" && opts.MinSizeBytes != 256*mb {
				t.Fatalf("wrong threshold: %d", opts.MinSizeBytes)
			}
		})
	}
}

func filterSizePointer(value int64) *int64 { return &value }

func TestLoadConfigParsesTorrentFilterAndCredentialsFromEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qbit-upload.yaml")
	if err := os.WriteFile(path, []byte(`torrent_filter:
  enabled: true
  base_url: http://localhost:8080/qb
  username: config-user
  min_size_mb: 128
  tag: movie-downloads
  poll_interval: 8s
  retry_interval: 45s
  request_timeout: 12s
  state_dir: /tmp/filter-state
`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldConfig := config
	config = path
	defer func() { config = oldConfig }()
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_URL", "https://example.test/qb")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_USERNAME", "env-user")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_PASSWORD", "test-password")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	opts, err := resolveTorrentFilterOptions(cfg.TorrentFilter)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TorrentFilter.Enabled || opts.BaseURL != "https://example.test/qb" || opts.Username != "env-user" || opts.Password != "test-password" ||
		opts.MinSizeBytes != 128*mb || opts.Tag != "movie-downloads" || opts.PollInterval != 8*time.Second ||
		opts.RetryInterval != 45*time.Second || opts.RequestTimeout != 12*time.Second || opts.StateDir != "/tmp/filter-state" {
		t.Fatal("torrent filter config or environment overrides were not applied")
	}
}
