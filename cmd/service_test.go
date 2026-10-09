package cmd

import (
	"strings"
	"testing"
)

func TestBuildSystemdUnitDefaultsToWatchMode(t *testing.T) {
	unit := buildSystemdUnit(systemdUnitOptions{
		BinaryPath: "/usr/local/bin/qbit-upload",
		ConfigPath: "/etc/qbit-upload.yaml",
		User:       "qbit",
	})

	for _, want := range []string{
		"User=qbit",
		"ExecStart=/usr/local/bin/qbit-upload watch --config /etc/qbit-upload.yaml",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestBuildSystemdUnitForTorrentFilter(t *testing.T) {
	unit := buildSystemdUnit(systemdUnitOptions{
		BinaryPath: "/usr/local/bin/qbit-upload", ConfigPath: "/etc/qbit-upload.yaml",
		Mode: "filter-torrents", EnvironmentFile: "/etc/qbit-upload-qbittorrent.env",
	})
	for _, want := range []string{
		"ExecStart=/usr/local/bin/qbit-upload filter-torrents --config /etc/qbit-upload.yaml",
		"EnvironmentFile=/etc/qbit-upload-qbittorrent.env",
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("filter service missing %q:\n%s", want, unit)
		}
	}
}
