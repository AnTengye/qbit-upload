package cmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type torrentFilterConfig struct {
	Enabled        bool   `json:"enabled" yaml:"enabled"`
	BaseURL        string `json:"base_url" yaml:"base_url"`
	Username       string `json:"username" yaml:"username"`
	Password       string `json:"password" yaml:"password"`
	MinSizeMB      *int64 `json:"min_size_mb" yaml:"min_size_mb"`
	Tag            string `json:"tag" yaml:"tag"`
	PollInterval   string `json:"poll_interval" yaml:"poll_interval"`
	RetryInterval  string `json:"retry_interval" yaml:"retry_interval"`
	RequestTimeout string `json:"request_timeout" yaml:"request_timeout"`
	StateDir       string `json:"state_dir" yaml:"state_dir"`
}

type torrentFilterOptions struct {
	BaseURL        string
	Username       string
	Password       string
	MinSizeBytes   int64
	Tag            string
	PollInterval   time.Duration
	RetryInterval  time.Duration
	RequestTimeout time.Duration
	StateDir       string
	DryRun         bool
}

func resolveTorrentFilterOptions(cfg torrentFilterConfig) (torrentFilterOptions, error) {
	opts := torrentFilterOptions{BaseURL: strings.TrimSpace(cfg.BaseURL), Username: strings.TrimSpace(cfg.Username),
		Password: cfg.Password, MinSizeBytes: 100 * mb, Tag: strings.TrimSpace(cfg.Tag),
		PollInterval: 5 * time.Second, RetryInterval: 30 * time.Second, RequestTimeout: 10 * time.Second,
		StateDir: strings.TrimSpace(cfg.StateDir)}
	if value := strings.TrimSpace(os.Getenv("QBIT_UPLOAD_QBITTORRENT_URL")); value != "" {
		opts.BaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv("QBIT_UPLOAD_QBITTORRENT_USERNAME")); value != "" {
		opts.Username = value
	}
	if value, ok := os.LookupEnv("QBIT_UPLOAD_QBITTORRENT_PASSWORD"); ok {
		opts.Password = value
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return torrentFilterOptions{}, fmt.Errorf("torrent_filter.base_url 必须是 http(s) 地址，且不能包含凭据、查询参数或片段")
	}
	opts.BaseURL = strings.TrimRight(u.String(), "/")
	if (opts.Username == "") != (opts.Password == "") {
		return torrentFilterOptions{}, fmt.Errorf("qBittorrent 用户名和密码必须同时配置；密码可通过 QBIT_UPLOAD_QBITTORRENT_PASSWORD 提供")
	}
	if cfg.MinSizeMB != nil {
		if *cfg.MinSizeMB <= 0 || *cfg.MinSizeMB > math.MaxInt64/mb {
			return torrentFilterOptions{}, fmt.Errorf("torrent_filter.min_size_mb 必须大于 0 且不能溢出")
		}
		opts.MinSizeBytes = *cfg.MinSizeMB * mb
	}
	if opts.Tag == "" {
		opts.Tag = "filter-small"
	}
	if strings.ContainsAny(opts.Tag, ",\r\n") {
		return torrentFilterOptions{}, fmt.Errorf("torrent_filter.tag 必须是单个标签")
	}
	for _, setting := range []struct {
		name  string
		value string
		dest  *time.Duration
	}{
		{"poll_interval", cfg.PollInterval, &opts.PollInterval},
		{"retry_interval", cfg.RetryInterval, &opts.RetryInterval},
		{"request_timeout", cfg.RequestTimeout, &opts.RequestTimeout},
	} {
		if setting.value == "" {
			continue
		}
		value, err := time.ParseDuration(setting.value)
		if err != nil || value <= 0 {
			return torrentFilterOptions{}, fmt.Errorf("torrent_filter.%s 必须是大于 0 的时间间隔", setting.name)
		}
		*setting.dest = value
	}
	if opts.StateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return torrentFilterOptions{}, fmt.Errorf("解析种子过滤状态目录失败: %w", err)
		}
		opts.StateDir = filepath.Join(base, "qbit-upload", "torrent-filter")
	}
	opts.StateDir, err = filepath.Abs(opts.StateDir)
	if err != nil {
		return torrentFilterOptions{}, err
	}
	return opts, nil
}

func newFilterTorrentsCmd() *cobra.Command {
	var once bool
	command := &cobra.Command{
		Use: "filter-torrents", Short: "后台按配置的文件大小过滤 qBittorrent 任务，核验后启动下载", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runFilterTorrents(cmd, once) },
	}
	command.Flags().BoolVar(&once, "once", false, "仅扫描一次后退出；等待元数据的任务留待下次处理")
	return command
}

func runFilterTorrents(cmd *cobra.Command, once bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	opts, err := resolveTorrentFilterOptions(cfg.TorrentFilter)
	if err != nil {
		return err
	}
	opts.DryRun = dryRun
	logPath, err := initLogging(cfg)
	if err != nil {
		return err
	}
	stepLog("开始种子过滤，标签=%s，最小大小=%dMiB，日志=%s", opts.Tag, opts.MinSizeBytes/mb, logPath)
	runner, err := newTorrentFilterRunner(opts)
	if err != nil {
		return err
	}
	defer runner.close()
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runner.run(ctx, once)
}

func newTorrentFilterRunner(opts torrentFilterOptions) (*torrentFilterRunner, error) {
	client, err := newQbitClient(opts)
	if err != nil {
		return nil, err
	}
	var store *torrentFilterStore
	if !opts.DryRun {
		store, err = openTorrentFilterStore(opts.StateDir)
		if err != nil {
			return nil, err
		}
	}
	return &torrentFilterRunner{client: client, store: store, opts: opts, now: time.Now}, nil
}

func (r *torrentFilterRunner) close() {
	if r.store != nil {
		r.store.close()
	}
}

// Watch owns the worker's lifetime and logging. Start before archive recovery,
// since packaging and upload retries can take much longer than a filter poll.
func startWatchTorrentFilter(ctx context.Context, cfg torrentFilterConfig, logPath string, dryRun bool) (func(), error) {
	if !cfg.Enabled {
		return func() {}, nil
	}
	opts, err := resolveTorrentFilterOptions(cfg)
	if err != nil {
		return nil, err
	}
	opts.DryRun = dryRun
	runner, err := newTorrentFilterRunner(opts)
	if err != nil {
		return nil, err
	}
	stepLog("开始种子过滤，标签=%s，最小大小=%dMiB，日志=%s", opts.Tag, opts.MinSizeBytes/mb, logPath)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer runner.close()
		_ = runner.run(ctx, false)
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

func (r *torrentFilterRunner) run(ctx context.Context, once bool) error {
	for {
		err := r.poll(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if once {
			return err
		}
		if err != nil {
			stepLog("WARN: 种子过滤未完成: %v", err)
		}
		timer := time.NewTimer(r.opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

type torrentFilterRunner struct {
	client *qbitClient
	store  *torrentFilterStore
	opts   torrentFilterOptions
	now    func() time.Time
}

func (r *torrentFilterRunner) poll(ctx context.Context) error {
	// A restarted/upgraded qBittorrent may change both the SID and endpoint names.
	if err := r.client.refreshVersion(ctx); err != nil {
		return err
	}
	torrents, err := r.client.torrents(ctx, url.Values{"tag": {r.opts.Tag}})
	if err != nil {
		return err
	}
	var failures []error
	for _, torrent := range torrents {
		if !torrentHasTag(torrent, r.opts.Tag) {
			continue
		}
		if !validTorrentHash(torrent.Hash) || torrent.AddedOn <= 0 || torrent.State == "" {
			failures = append(failures, fmt.Errorf("qBittorrent 返回了无效的任务标识或状态"))
			continue
		}
		if r.opts.DryRun {
			files, err := r.client.files(ctx, torrent.Hash)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			excluded, retained := planTorrentSizeFilter(files, r.opts.MinSizeBytes)
			stepLog("[dry-run] 种子 %s: 文件=%d，取消下载=%d，保留已选文件=%d", torrent.Hash, len(files), len(excluded), retained)
			continue
		}
		job, exists := r.store.jobs[torrent.Hash]
		if exists && (job.Server != r.opts.BaseURL || job.Tag != r.opts.Tag) {
			failures = append(failures, fmt.Errorf("种子 %s 的状态目录属于其他 qBittorrent 地址或标签", torrent.Hash))
			continue
		}
		if !exists || job.AddedOn != torrent.AddedOn {
			job = torrentFilterJob{Version: 1, Hash: torrent.Hash, AddedOn: torrent.AddedOn,
				Server: r.opts.BaseURL, Tag: r.opts.Tag, Stage: "waiting"}
			if err := r.store.save(&job); err != nil {
				return fmt.Errorf("保存待筛选种子记录失败: %w", err)
			}
			stepLog("种子 %s 已进入过滤队列，等待元数据", job.Hash)
		}
		if job.Stage == "done" || r.now().Before(job.NextRetry) {
			continue
		}
		if err := r.process(ctx, torrent, &job); err != nil {
			job.LastError = err.Error()
			job.NextRetry = r.now().Add(r.opts.RetryInterval)
			if saveErr := r.store.save(&job); saveErr != nil {
				return fmt.Errorf("保存种子过滤重试状态失败: %w", saveErr)
			}
			failures = append(failures, fmt.Errorf("种子 %s: %w", torrent.Hash, err))
		}
	}
	return errors.Join(failures...)
}

// Only disable small files. Never re-enable an existing priority-0 selection,
// and use qBittorrent's file indexes rather than JSON array positions.
func planTorrentSizeFilter(files []qbitFile, minSizeBytes int64) (excluded []int, retained int) {
	for _, file := range files {
		if file.Size < minSizeBytes {
			if file.Priority != 0 {
				excluded = append(excluded, file.Index)
			}
		} else if file.Priority != 0 {
			retained++
		}
	}
	return excluded, retained
}

func verifyTorrentSizeFilter(expected, actual []qbitFile, minSizeBytes int64) (int, error) {
	if len(expected) == 0 || len(actual) != len(expected) {
		return 0, fmt.Errorf("核验失败：文件列表为空或数量发生变化")
	}
	byIndex := make(map[int]qbitFile, len(actual))
	for _, file := range actual {
		byIndex[file.Index] = file
	}
	retained := 0
	for _, before := range expected {
		after, ok := byIndex[before.Index]
		if !ok || before.Name != after.Name || before.Size != after.Size {
			return 0, fmt.Errorf("核验失败：文件索引 %d 的元数据发生变化", before.Index)
		}
		priority := before.Priority
		if before.Size < minSizeBytes {
			priority = 0
		}
		if after.Priority != priority {
			return 0, fmt.Errorf("核验失败：文件索引 %d 的优先级未按计划生效", before.Index)
		}
		if after.Priority > 0 {
			retained++
		}
	}
	return retained, nil
}

func (r *torrentFilterRunner) currentTorrent(ctx context.Context, job *torrentFilterJob) (qbitTorrent, error) {
	torrent, err := r.client.torrent(ctx, job.Hash)
	if err != nil {
		return qbitTorrent{}, err
	}
	if torrent.AddedOn != job.AddedOn || !torrentHasTag(torrent, job.Tag) {
		return qbitTorrent{}, fmt.Errorf("任务已被重新添加或移除了过滤标签")
	}
	return torrent, nil
}

func (r *torrentFilterRunner) process(ctx context.Context, torrent qbitTorrent, job *torrentFilterJob) error {
	if job.Stage == "start-requested" {
		return r.confirmStarted(ctx, job)
	}
	files, err := r.client.files(ctx, job.Hash)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil // Metadata is still being fetched; do not pause that fetch.
	}
	// Even if an adding client forgot the stop condition, stop a tagged task
	// before changing priorities and confirm the stopped state through the API.
	torrent, err = r.currentTorrent(ctx, job)
	if err != nil {
		return err
	}
	if !torrentIsStopped(torrent) {
		if err := r.client.setRunning(ctx, job.Hash, false); err != nil {
			return err
		}
		torrent, err = r.currentTorrent(ctx, job)
		if err != nil {
			return err
		}
		if !torrentIsStopped(torrent) {
			return fmt.Errorf("等待 qBittorrent 任务停止后再筛选")
		}
	}
	if job.Stage == "waiting" {
		// Snapshot the selection after the stop has taken effect, since another
		// client may have edited priorities while the task was still running.
		files, err = r.client.files(ctx, job.Hash)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return fmt.Errorf("停止后文件列表为空，等待下次核验")
		}
		job.Stage, job.Files, job.MinSizeBytes = "filtering", files, r.opts.MinSizeBytes
		if err := r.store.save(job); err != nil {
			return fmt.Errorf("保存筛选计划失败: %w", err)
		}
	}
	indexes, _ := planTorrentSizeFilter(files, job.MinSizeBytes)
	if len(indexes) > 0 {
		if err := r.client.exclude(ctx, job.Hash, indexes); err != nil {
			return err
		}
	}
	actual, err := r.client.files(ctx, job.Hash)
	if err != nil {
		return err
	}
	retained, err := verifyTorrentSizeFilter(job.Files, actual, job.MinSizeBytes)
	if err != nil {
		return err
	}
	if retained == 0 {
		return fmt.Errorf("没有达到大小阈值且被选中的文件，任务保持停止")
	}
	torrent, err = r.currentTorrent(ctx, job)
	if err != nil {
		return err
	}
	if !torrentIsStopped(torrent) {
		return fmt.Errorf("核验时任务已被其他操作启动，暂不提交启动请求")
	}
	job.Stage, job.LastError, job.NextRetry = "verified", "", time.Time{}
	if err := r.store.save(job); err != nil {
		return fmt.Errorf("保存核验状态失败: %w", err)
	}
	// Record intent before POST so a lost response cannot cause repeated starts
	// that override a subsequent manual pause. Recovery reads actual state first.
	job.Stage = "start-requested"
	if err := r.store.save(job); err != nil {
		return fmt.Errorf("保存启动意图失败: %w", err)
	}
	if err := r.client.setRunning(ctx, job.Hash, true); err != nil {
		return err
	}
	return r.confirmStarted(ctx, job)
}

func (r *torrentFilterRunner) confirmStarted(ctx context.Context, job *torrentFilterJob) error {
	torrent, err := r.currentTorrent(ctx, job)
	if err != nil {
		return err
	}
	if !torrentIsRunning(torrent) {
		return fmt.Errorf("启动结果尚未确认；若任务已停止，请在 qBittorrent 中手动启动，服务不会重复启动")
	}
	actual, err := r.client.files(ctx, job.Hash)
	if err != nil {
		return err
	}
	retained, err := verifyTorrentSizeFilter(job.Files, actual, job.MinSizeBytes)
	if err != nil {
		return err
	}
	job.Stage, job.LastError, job.NextRetry = "done", "", time.Time{}
	if err := r.store.save(job); err != nil {
		return fmt.Errorf("保存种子过滤完成状态失败: %w", err)
	}
	stepLog("种子 %s 已核验大小过滤并启动，保留已选文件=%d，阈值=%dMiB", job.Hash, retained, job.MinSizeBytes/mb)
	return nil
}
