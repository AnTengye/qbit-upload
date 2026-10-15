package cmd

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type torrentRescueConfig struct {
	StorageBudgetGB int64  `json:"storage_budget_gb" yaml:"storage_budget_gb"`
	MinFreeGB       int64  `json:"min_free_gb" yaml:"min_free_gb"`
	Enabled         bool   `json:"enabled" yaml:"enabled"`
	RSSURL          string `json:"rss_url" yaml:"rss_url"`
	ProxyURL        string `json:"proxy_url" yaml:"proxy_url"`
	PollInterval    string `json:"poll_interval" yaml:"poll_interval"`
	MinAge          string `json:"min_age" yaml:"min_age"`
	// Deprecated: accepted for existing configs; admission uses MinAge only.
	NoProgress       string `json:"no_progress" yaml:"no_progress"`
	SearchInterval   string `json:"search_interval" yaml:"search_interval"`
	SearchTimeout    string `json:"search_timeout" yaml:"search_timeout"`
	MetadataTimeout  string `json:"metadata_timeout" yaml:"metadata_timeout"`
	TrialTimeout     string `json:"trial_timeout" yaml:"trial_timeout"`
	Cooldown         string `json:"cooldown" yaml:"cooldown"`
	PoorScoreAfter   string `json:"poor_score_after" yaml:"poor_score_after"`
	MinRuntimeScore  int    `json:"min_runtime_score" yaml:"min_runtime_score"`
	MaxPerCode       int    `json:"max_per_code" yaml:"max_per_code"`
	MaxActive        int    `json:"max_active" yaml:"max_active"`
	MaxCandidates    int    `json:"max_candidates" yaml:"max_candidates"`
	SearchBatch      int    `json:"search_batch" yaml:"search_batch"`
	MinVideoSizeMB   int64  `json:"min_video_size_mb" yaml:"min_video_size_mb"`
	PreferredVideoGB int64  `json:"preferred_video_size_gb" yaml:"preferred_video_size_gb"`
	IdealVideoGB     int64  `json:"ideal_video_size_gb" yaml:"ideal_video_size_gb"`
	// Deprecated: title labels no longer restrict candidate selection.
	PreserveLabels  *bool  `json:"preserve_labels" yaml:"preserve_labels"`
	Category        string `json:"category" yaml:"category"`
	StateDir        string `json:"state_dir" yaml:"state_dir"`
	GuardDir        string `json:"guard_dir" yaml:"guard_dir"`
	HookPath        string `json:"hook_path" yaml:"hook_path"`
	DownloadPrefix  string `json:"download_path_prefix" yaml:"download_path_prefix"`
	DownloadHostDir string `json:"download_host_dir" yaml:"download_host_dir"`
	CandidatePath   string `json:"candidate_save_path" yaml:"candidate_save_path"`
	PublishDir      string `json:"publish_dir" yaml:"publish_dir"`
	MinDuration     string `json:"min_video_duration" yaml:"min_video_duration"`
	MinHeight       int    `json:"min_video_height" yaml:"min_video_height"`
}
type torrentRescueOptions struct {
	ForceCode                                                                                                            string
	StorageBudget, MinFree                                                                                               uint64
	PoorScoreAfter                                                                                                       time.Duration
	MinRuntimeScore                                                                                                      int
	Qbit                                                                                                                 torrentFilterOptions
	RSSURL, ProxyURL, StateDir, GuardDir, HookPath, DownloadPrefix, DownloadHostDir, CandidatePath, PublishDir, Category string
	PollInterval, MinAge, SearchInterval, SearchTimeout, MetadataTimeout, TrialTimeout, Cooldown, MinDuration            time.Duration
	MaxPerCode, MaxActive, MaxCandidates, SearchBatch, MinHeight                                                         int
	MinVideoSize                                                                                                         int64
	PreferredVideoSize, IdealVideoSize                                                                                   int64
	DryRun                                                                                                               bool
	FFprobe, FFmpeg                                                                                                      string
}

func resolveTorrentRescueOptions(cfg appConfig, readOnly bool) (torrentRescueOptions, error) {
	c := cfg.TorrentRescue
	o := torrentRescueOptions{RSSURL: c.RSSURL, ProxyURL: c.ProxyURL, StateDir: c.StateDir, GuardDir: c.GuardDir, HookPath: c.HookPath, DownloadPrefix: c.DownloadPrefix, DownloadHostDir: c.DownloadHostDir, CandidatePath: c.CandidatePath, PublishDir: c.PublishDir, Category: c.Category, PollInterval: 15 * time.Minute, MinAge: 240 * time.Hour, SearchInterval: 24 * time.Hour, SearchTimeout: 30 * time.Second, MetadataTimeout: time.Hour, TrialTimeout: 24 * time.Hour, Cooldown: 168 * time.Hour, MinDuration: time.Minute, MaxPerCode: 3, MaxActive: 6, MaxCandidates: 10, SearchBatch: 2, MinVideoSize: 100 * mb, MinHeight: 720}
	o.PoorScoreAfter = 6 * time.Hour
	o.StorageBudget = 200 << 30
	o.MinFree = 20 << 30
	o.PreferredVideoSize = 4 << 30
	o.IdealVideoSize = 20 << 30
	for _, s := range []struct {
		name  string
		value int64
		dest  *int64
	}{{"preferred_video_size_gb", c.PreferredVideoGB, &o.PreferredVideoSize}, {"ideal_video_size_gb", c.IdealVideoGB, &o.IdealVideoSize}} {
		if s.value < 0 || s.value > 1024 {
			return o, fmt.Errorf("torrent_rescue.%s 必须在 0 到 1024 GiB 之间", s.name)
		}
		if s.value > 0 {
			*s.dest = s.value << 30
		}
	}
	if o.IdealVideoSize < o.PreferredVideoSize {
		return o, fmt.Errorf("理想正片大小不能小于优先正片大小")
	}
	if c.StorageBudgetGB < 0 || c.StorageBudgetGB > 1<<30 || c.MinFreeGB < 0 || c.MinFreeGB > 1<<30 {
		return o, fmt.Errorf("补救磁盘容量参数无效")
	}
	if c.StorageBudgetGB > 0 {
		o.StorageBudget = uint64(c.StorageBudgetGB) << 30
	}
	if c.MinFreeGB > 0 {
		o.MinFree = uint64(c.MinFreeGB) << 30
	}
	o.MinRuntimeScore = 40
	if c.MinRuntimeScore < 0 || c.MinRuntimeScore > 100 {
		return o, fmt.Errorf("min_runtime_score 必须在 0 到 100 之间")
	}
	if c.MinRuntimeScore > 0 {
		o.MinRuntimeScore = c.MinRuntimeScore
	}
	if c.PoorScoreAfter != "" {
		v, err := time.ParseDuration(c.PoorScoreAfter)
		if err != nil || v <= 0 {
			return o, fmt.Errorf("poor_score_after 必须是正时间间隔")
		}
		o.PoorScoreAfter = v
	}
	var legacyNoProgress time.Duration
	for _, s := range []struct {
		name, value string
		dest        *time.Duration
	}{
		{"poll_interval", c.PollInterval, &o.PollInterval}, {"min_age", c.MinAge, &o.MinAge}, {"no_progress", c.NoProgress, &legacyNoProgress}, {"search_interval", c.SearchInterval, &o.SearchInterval}, {"search_timeout", c.SearchTimeout, &o.SearchTimeout}, {"metadata_timeout", c.MetadataTimeout, &o.MetadataTimeout}, {"trial_timeout", c.TrialTimeout, &o.TrialTimeout}, {"cooldown", c.Cooldown, &o.Cooldown}, {"min_video_duration", c.MinDuration, &o.MinDuration},
	} {
		if s.value != "" {
			v, err := time.ParseDuration(s.value)
			if err != nil || v <= 0 {
				return o, fmt.Errorf("torrent_rescue.%s 必须是正时间间隔", s.name)
			}
			*s.dest = v
		}
	}
	for _, s := range []struct {
		name  string
		value int
		dest  *int
	}{{"max_per_code", c.MaxPerCode, &o.MaxPerCode}, {"max_active", c.MaxActive, &o.MaxActive}, {"max_candidates", c.MaxCandidates, &o.MaxCandidates}, {"search_batch", c.SearchBatch, &o.SearchBatch}, {"min_video_height", c.MinHeight, &o.MinHeight}} {
		if s.value < 0 {
			return o, fmt.Errorf("torrent_rescue.%s 不能为负数", s.name)
		}
		if s.value > 0 {
			*s.dest = s.value
		}
	}
	if o.MaxCandidates < o.MaxPerCode || o.MaxPerCode > o.MaxActive || o.MaxActive > 100 || o.SearchBatch > 20 {
		return o, fmt.Errorf("补救并发参数无效：候选数 >= 每番号并发，且全局并发 >= 每番号并发、<=100；搜索批次 <=20")
	}
	if c.MinVideoSizeMB < 0 || c.MinVideoSizeMB > 1<<40 {
		return o, fmt.Errorf("补救最小视频大小无效")
	}
	if c.MinVideoSizeMB > 0 {
		o.MinVideoSize = c.MinVideoSizeMB * mb
	}
	if o.RSSURL == "" {
		o.RSSURL = "https://sukebei.nyaa.si/"
	}
	if v := os.Getenv("QBIT_UPLOAD_SEARCH_PROXY"); v != "" {
		o.ProxyURL = v
	}
	for _, s := range []struct {
		name, value string
		proxy       bool
	}{{"rss_url", o.RSSURL, false}, {"proxy_url", o.ProxyURL, true}} {
		if s.value == "" {
			continue
		}
		u, err := url.Parse(s.value)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && !(s.proxy && (u.Scheme == "socks5" || u.Scheme == "socks5h"))) {
			return o, fmt.Errorf("torrent_rescue.%s 地址无效，不能包含凭据、查询或片段", s.name)
		}
	}
	if o.StateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return o, err
		}
		o.StateDir = filepath.Join(base, "qbit-upload", "torrent-rescue")
	}
	var err error
	o.StateDir, err = filepath.Abs(o.StateDir)
	if err != nil {
		return o, err
	}
	o.FFprobe = cfg.Thumbnail.FFprobe
	if o.FFprobe == "" {
		o.FFprobe = "ffprobe"
	}
	o.FFmpeg = cfg.Thumbnail.FFmpeg
	if o.FFmpeg == "" {
		o.FFmpeg = "ffmpeg"
	}
	if readOnly {
		return o, nil
	}
	o.Qbit, err = resolveTorrentFilterOptions(cfg.TorrentFilter)
	if err != nil {
		return o, err
	}
	if o.HookPath == "" {
		o.HookPath = "/config/qbit-rescue-hook.py"
	}
	for _, s := range []struct{ name, value string }{{"guard_dir", o.GuardDir}, {"download_host_dir", o.DownloadHostDir}, {"publish_dir", o.PublishDir}} {
		if !filepath.IsAbs(s.value) {
			return o, fmt.Errorf("torrent_rescue.%s 必须是绝对路径", s.name)
		}
	}
	for _, s := range []struct{ name, value string }{{"download_path_prefix", o.DownloadPrefix}, {"candidate_save_path", o.CandidatePath}} {
		if !strings.HasPrefix(s.value, "/") || strings.Contains(s.value, `\`) {
			return o, fmt.Errorf("torrent_rescue.%s 必须是 qBittorrent 内的绝对 POSIX 路径", s.name)
		}
	}
	if !strings.HasPrefix(strings.TrimRight(o.CandidatePath, "/"), strings.TrimRight(o.DownloadPrefix, "/")+"/") {
		return o, fmt.Errorf("候选保存路径必须在下载映射范围内")
	}
	if o.Category == "" {
		o.Category = "qbit-rescue"
	}
	if strings.ContainsAny(o.Category, ",\r\n") {
		return o, fmt.Errorf("补救 category 无效")
	}
	return o, nil
}
