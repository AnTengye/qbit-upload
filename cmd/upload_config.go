package cmd

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const defaultRetryInterval = 12 * time.Hour

type uploadConfig struct {
	Enabled           bool   `json:"enabled" yaml:"enabled"`
	BaseURL           string `json:"base_url" yaml:"base_url"`
	Username          string `json:"username" yaml:"username"`
	CertificateSHA256 string `json:"certificate_sha256" yaml:"certificate_sha256"`
	RemoteDir         string `json:"remote_dir" yaml:"remote_dir"`
	StateDir          string `json:"state_dir" yaml:"state_dir"`
	TaskDB            string `json:"task_db" yaml:"task_db"`
	SQLite            string `json:"sqlite" yaml:"sqlite"`
	Timeout           string `json:"timeout" yaml:"timeout"`
	PollInterval      string `json:"poll_interval" yaml:"poll_interval"`
	RetryInterval     string `json:"retry_interval" yaml:"retry_interval"`
}

type uploadOptions struct {
	Enabled                                                                   bool
	BaseURL, Username, CertificateSHA256, RemoteDir, StateDir, TaskDB, SQLite string
	Timeout, PollInterval, RetryInterval                                      time.Duration
}

func resolveUploadOptions(cfg uploadConfig, archiveDest string) (uploadOptions, error) {
	opts := uploadOptions{
		Enabled: cfg.Enabled, BaseURL: "https://127.0.0.1:9443", Username: strings.TrimSpace(cfg.Username),
		CertificateSHA256: strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.CertificateSHA256), ":", "")),
		RemoteDir:         "/绿联网盘", StateDir: filepath.Join(archiveDest, ".qbit-upload-jobs"),
		TaskDB: "/volume1/@appstore/com.ugreen.netdisk/db/net_disk.db", SQLite: "sqlite3",
		Timeout: 24 * time.Hour, PollInterval: 5 * time.Second, RetryInterval: defaultRetryInterval,
	}
	for _, pair := range []struct {
		input  string
		output *string
	}{
		{cfg.BaseURL, &opts.BaseURL}, {cfg.RemoteDir, &opts.RemoteDir}, {cfg.StateDir, &opts.StateDir},
		{cfg.TaskDB, &opts.TaskDB}, {cfg.SQLite, &opts.SQLite},
	} {
		if strings.TrimSpace(pair.input) != "" {
			*pair.output = strings.TrimSpace(pair.input)
		}
	}
	for _, pair := range []struct {
		input, name string
		output      *time.Duration
	}{
		{cfg.Timeout, "timeout", &opts.Timeout}, {cfg.PollInterval, "poll_interval", &opts.PollInterval}, {cfg.RetryInterval, "retry_interval", &opts.RetryInterval},
	} {
		if pair.input == "" {
			continue
		}
		d, err := time.ParseDuration(pair.input)
		if err != nil || d <= 0 {
			return opts, fmt.Errorf("upload.%s 必须是大于 0 的时长", pair.name)
		}
		*pair.output = d
	}
	abs, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return opts, err
	}
	opts.StateDir = abs
	if opts.Enabled {
		u, err := url.Parse(opts.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return opts, fmt.Errorf("upload.base_url 必须是 NAS 的 HTTPS 地址，不含路径或凭据")
		}
		if opts.CertificateSHA256 != "" {
			pin, err := hex.DecodeString(opts.CertificateSHA256)
			if err != nil || len(pin) != 32 {
				return opts, fmt.Errorf("upload.certificate_sha256 必须是 SHA256 证书指纹")
			}
		}
		if !strings.HasPrefix(opts.RemoteDir, "/") {
			return opts, fmt.Errorf("upload.remote_dir 必须是网盘绝对路径")
		}
		opts.RemoteDir = path.Clean(opts.RemoteDir)
	}
	return opts, nil
}
