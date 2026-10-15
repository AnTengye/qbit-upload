package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const rescueTag = "qbit-rescue"

var rescueCodeRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{1,64}$`)

func validRescueCode(s string) bool { return rescueCodeRE.MatchString(s) }

type torrentRescueRunner struct {
	scopeReady bool
	opts       torrentRescueOptions
	client     *qbitClient
	store      *rescueStore
	search     func(context.Context, string) ([]rescueCandidate, error)
	verify     func(context.Context, []string) error
	now        func() time.Time
	diskFree   func(string) (uint64, error)
}

func newTorrentRescueRunner(opts torrentRescueOptions) (*torrentRescueRunner, error) {
	client, err := newQbitClient(opts.Qbit)
	if err != nil {
		return nil, err
	}
	store, err := openRescueStore(opts.StateDir, opts.Qbit.BaseURL, opts.DryRun)
	if err != nil {
		return nil, err
	}
	r := &torrentRescueRunner{opts: opts, client: client, store: store, search: newTorrentSearcher(opts).search, now: time.Now, diskFree: rescueDiskFree}
	r.verify = r.verifyVideos
	return r, nil
}
func newRescueTorrentsCmd() *cobra.Command {
	var once bool
	var forceCode string
	c := &cobra.Command{Use: "rescue-torrents", Short: "检查停滞任务、检索候选并管理试下载和唯一归档交付", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		opts, err := resolveTorrentRescueOptions(cfg, false)
		if err != nil {
			return err
		}
		opts.DryRun = dryRun
		if forceCode != "" {
			parsed, ok := preprocessCatalogNumber(forceCode)
			if !ok {
				return fmt.Errorf("force-code 无法识别番号")
			}
			opts.ForceCode = parsed.Code
		}
		if _, err = initLogging(cfg); err != nil {
			return err
		}
		runner, err := newTorrentRescueRunner(opts)
		if err != nil {
			return err
		}
		defer runner.store.close()
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return runner.run(ctx, once)
	}}
	c.Flags().BoolVar(&once, "once", false, "仅检查一轮；持续观察记录保留供下次运行")
	c.Flags().StringVar(&forceCode, "force-code", "", "优先调度指定的超龄停滞番号；仍检查年龄和任务状态")
	return c
}
func startWatchTorrentRescue(ctx context.Context, cfg appConfig, dry bool) (func(), error) {
	if !cfg.TorrentRescue.Enabled {
		return func() {}, nil
	}
	opts, err := resolveTorrentRescueOptions(cfg, false)
	if err != nil {
		return nil, err
	}
	opts.DryRun = dry
	r, err := newTorrentRescueRunner(opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); defer r.store.close(); _ = r.run(ctx, false) }()
	return func() { cancel(); <-done }, nil
}
func (r *torrentRescueRunner) run(ctx context.Context, once bool) error {
	stepLog("开始种子补救，每番号并发=%d，全局并发=%d，添加年龄阈值=%s，首次检查无需观察等待", r.opts.MaxPerCode, r.opts.MaxActive, r.opts.MinAge)
	for {
		err := r.poll(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			stepLog("WARN: 种子补救检查失败: %v", err)
		}
		if once {
			return err
		}
		if !waitWatchContext(ctx, r.opts.PollInterval) {
			return nil
		}
	}
}
func (c *qbitClient) getObject(ctx context.Context, endpoint string, query url.Values, target any) error {
	data, ctype, err := c.request(ctx, endpoint, query, false)
	if err != nil {
		return err
	}
	media, _, _ := mime.ParseMediaType(ctype)
	if media != "application/json" || !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return fmt.Errorf("qBittorrent %s 未返回 JSON 对象", endpoint)
	}
	if err = json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("解析 qBittorrent %s 失败", endpoint)
	}
	return nil
}
func (r *torrentRescueRunner) checkHook(ctx context.Context) error {
	if r.opts.DryRun {
		return nil
	}
	var prefs struct {
		Enabled bool   `json:"autorun_enabled"`
		Program string `json:"autorun_program"`
	}
	if err := r.client.getObject(ctx, "app/preferences", nil, &prefs); err != nil {
		return err
	}
	if !prefs.Enabled || !strings.Contains(prefs.Program, r.opts.HookPath) || !strings.Contains(prefs.Program, "%I") {
		return fmt.Errorf("必须先安装补救完成回调并传入 %%I hash，才能管理候选")
	}
	return nil
}
func observeRescue(old rescueObservation, t qbitTorrent, now time.Time, gap time.Duration) rescueObservation {
	if old.AddedOn != t.AddedOn || old.ObservedAt.IsZero() || now.Before(old.ObservedAt) || now.Sub(old.ObservedAt) > gap || t.Completed < old.Completed {
		return rescueObservation{AddedOn: t.AddedOn, Completed: t.Completed, LastProgress: now, ObservedAt: now}
	}
	if t.Completed > old.Completed {
		old.LastProgress = now
	}
	old.Completed = t.Completed
	old.ObservedAt = now
	return old
}
func rescueEligible(t qbitTorrent, now time.Time, opts torrentRescueOptions) bool {
	if t.AddedOn <= 0 || !validTorrentHash(t.Hash) || t.Progress >= 1 || t.Size <= 0 || t.State != "stalledDL" || now.Sub(time.Unix(t.AddedOn, 0)) < opts.MinAge {
		return false
	}
	return true
}
func (r *torrentRescueRunner) poll(ctx context.Context) error {
	if err := r.client.refreshVersion(ctx); err != nil {
		return err
	}
	if err := r.checkHook(ctx); err != nil {
		return err
	}
	ts, err := r.client.torrents(ctx, nil)
	if err != nil {
		return err
	}
	now := r.now().UTC()
	byHash := map[string]qbitTorrent{}
	for _, t := range ts {
		if !validTorrentHash(t.Hash) || t.AddedOn <= 0 {
			continue
		}
		byHash[t.Hash] = t
		r.store.state.Observations[t.Hash] = observeRescue(r.store.state.Observations[t.Hash], t, now, 2*r.opts.PollInterval+r.opts.Qbit.RequestTimeout)
	}
	for h := range r.store.state.Observations {
		if _, exists := byHash[h]; !exists {
			delete(r.store.state.Observations, h)
		}
	}
	var transfer struct {
		Status string `json:"connection_status"`
	}
	if err = r.client.getObject(ctx, "transfer/info", nil, &transfer); err != nil {
		return err
	}
	networkAvailable := transfer.Status == "connected" || transfer.Status == "firewalled"
	for _, t := range ts {
		if torrentHasTag(t, rescueTag) {
			continue
		}
		code, ok := preprocessCatalogNumber(t.Name)
		if !ok || !validRescueCode(code.Code) {
			continue
		}
		if !rescueEligible(t, now, r.opts) {
			continue
		}
		if _, exists := r.store.state.Groups[code.Code]; exists {
			continue
		}
		originalFiles, err := r.client.files(ctx, t.Hash)
		if err != nil {
			return err
		}
		if _, err = selectRescueVideos(originalFiles, code.Code, r.opts.MinVideoSize); err != nil {
			continue
		}
		d := &rescueDownload{Candidate: rescueCandidate{Hash: t.Hash, Title: t.Name, Labels: rescueLabels(t.Name)}, Original: true, Stage: "original", AddedOn: t.AddedOn, CreatedAt: now}
		g := &rescueGroup{Code: code.Code, Stage: "open", Downloads: map[string]*rescueDownload{t.Hash: d}}
		r.store.state.Groups[g.Code] = g
		if err = r.store.save(); err != nil {
			return err
		}
		if err = r.store.guard(r.opts.GuardDir, g, d); err != nil {
			return err
		}
		stepLog("补救候选组: %s，原任务=%s", g.Code, t.Hash)
	}
	if err = r.store.save(); err != nil {
		return err
	}
	codes := make([]string, 0, len(r.store.state.Groups))
	for code := range r.store.state.Groups {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool {
		if (codes[i] == r.opts.ForceCode) != (codes[j] == r.opts.ForceCode) {
			return codes[i] == r.opts.ForceCode
		}
		a, b := rescueGroupAddedOn(r.store.state.Groups[codes[i]]), rescueGroupAddedOn(r.store.state.Groups[codes[j]])
		if a != b {
			return a < b
		}
		return codes[i] < codes[j]
	})
	active := 0
	for _, g := range r.store.state.Groups {
		for _, d := range g.Downloads {
			if !d.Original && rescueActiveStage(d.Stage) {
				active++
			}
		}
	}
	searched := 0
	var problems []error
	for _, code := range codes {
		g := r.store.state.Groups[code]
		if err = r.processGroup(ctx, g, byHash, now, networkAvailable, &active, &searched); err != nil {
			g.LastError = err.Error()
			problems = append(problems, fmt.Errorf("%s: %w", code, err))
			if saveErr := r.store.save(); saveErr != nil {
				return saveErr
			}
		} else {
			g.LastError = ""
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		active = 0
		for _, group := range r.store.state.Groups {
			for _, d := range group.Downloads {
				if !d.Original && rescueActiveStage(d.Stage) {
					active++
				}
			}
		}
	}
	if err = r.store.save(); err != nil {
		return err
	}
	return errors.Join(problems...)
}
func rescueGroupAddedOn(g *rescueGroup) int64 {
	oldest := int64(1<<63 - 1)
	for _, d := range g.Downloads {
		if d.Original && d.AddedOn > 0 && d.AddedOn < oldest {
			oldest = d.AddedOn
		}
	}
	return oldest
}
func rescueActiveStage(stage string) bool {
	return stage == "adding" || stage == "metadata" || stage == "start-requested" || stage == "running" || stage == "quality-stopping" || stage == "stopping"
}
func (r *torrentRescueRunner) processGroup(ctx context.Context, g *rescueGroup, live map[string]qbitTorrent, now time.Time, network bool, active, searched *int) error {
	// Empty searches made under the old title/version restrictions must be
	// refreshed when upgrading to selection based on actual film quality.
	if len(g.Labels) > 0 {
		g.Labels = nil
		if g.Stage == "open" {
			g.NextSearch = time.Time{}
		}
		if err := r.store.save(); err != nil {
			return err
		}
	}
	completed := []*rescueDownload{}
	var sourceErr error
	hashes := make([]string, 0, len(g.Downloads))
	for h := range g.Downloads {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	if g.Stage == "publishing" {
		if err := r.publishWinner(ctx, g, live); err != nil {
			return err
		}
	}
	if g.Stage == "published" {
		return r.cleanupPublishedGroup(ctx, g)
	}
	for _, h := range hashes {
		d := g.Downloads[h]
		t, exists := live[h]
		if d.AddedOn > 0 && (!exists || t.AddedOn != d.AddedOn) {
			if rescueActiveStage(d.Stage) {
				*active--
			}
			d.Stage = "removed"
			d.Reason = "任务被删除或重新添加，停止自动管理"
			continue
		}
		if !exists {
			if d.Stage == "adding" && network && now.Sub(d.CreatedAt) >= r.opts.MetadataTimeout {
				d.Stage = "failed"
				d.Reason = "添加结果未能确认"
				d.CooldownUntil = now.Add(r.opts.Cooldown)
				*active--
			}
			continue
		}
		if !d.Original && !r.ownsCandidate(g, d, t) {
			if rescueActiveStage(d.Stage) {
				*active--
			}
			d.Stage = "removed"
			d.Reason = "任务归属或保存路径发生变化"
			if !r.opts.DryRun && d.AddedOn == 0 {
				if err := os.Remove(filepath.Join(r.opts.GuardDir, h+".json")); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			continue
		}
		if d.Stage == "adding" {
			d.AddedOn = t.AddedOn
			d.Stage = "metadata"
			if err := r.store.save(); err != nil {
				return err
			}
			if err := r.store.guard(r.opts.GuardDir, g, d); err != nil {
				return err
			}
		}
		if d.Stage == "removed" || d.Stage == "queued" {
			continue
		}
		if d.Stage == "failed" && now.Before(d.CooldownUntil) {
			continue
		}
		if err := r.store.guard(r.opts.GuardDir, g, d); err != nil {
			return err
		}
		if d.Stage == "quality-stopping" {
			if err := r.parkForQuality(ctx, g, d); err != nil {
				return err
			}
			*active--
			continue
		}
		if !d.Original && d.FilmBytes == 0 && (d.Stage == "running" || d.Stage == "start-requested") {
			// State created before quality scheduling can be upgraded from the
			// same torrent's immutable metadata without trusting RSS totals.
			files, err := r.client.files(ctx, h)
			if err != nil {
				return err
			}
			selected, err := selectRescueVideos(files, g.Code, r.opts.MinVideoSize)
			if err != nil {
				if err = r.failDownload(ctx, g, d, t, now, err.Error()); err != nil {
					return err
				}
				*active--
				continue
			}
			d.Files, d.FilmBytes, d.MetadataBytes = selected, rescueFilmBytes(selected), rescueFileBytes(files)
		}
		if t.Progress >= 1 && t.AmountLeft == 0 && t.State != "checkingUP" && t.State != "checkingDL" && t.State != "checkingResumeData" && t.State != "moving" {
			files, err := r.client.files(ctx, h)
			if err != nil {
				return err
			}
			selected, err := selectRescueVideos(files, g.Code, r.opts.MinVideoSize)
			if err != nil {
				if d.Original {
					return err
				}
				wasActive := rescueActiveStage(d.Stage)
				if err = r.failDownload(ctx, g, d, t, now, err.Error()); err != nil {
					return err
				}
				if wasActive {
					*active--
				}
				continue
			}
			allComplete := true
			for _, f := range selected {
				if f.Progress < 1 {
					allComplete = false
				}
			}
			if !allComplete {
				continue
			}
			d.Files = selected
			d.MetadataBytes = rescueFileBytes(files)
			d.FilmBytes = rescueFilmBytes(selected)
			completed = append(completed, d)
			continue
		}
		if d.Original {
			continue
		}
		if d.Stage == "failed" || d.Stage == "manual-stopped" {
			continue
		}
		if d.Stage == "metadata" {
			if t.Size == 0 || t.State == "metaDL" || t.State == "forcedMetaDL" {
				if network && now.Sub(d.CreatedAt) >= r.opts.MetadataTimeout {
					if err := r.failDownload(ctx, g, d, t, now, "元数据超时"); err != nil {
						return err
					}
					*active--
				}
				continue
			}
			if !torrentIsStopped(t) {
				if r.opts.DryRun {
					continue
				}
				if _, err := r.currentOwnedTorrent(ctx, g, d); err != nil {
					return err
				}
				if err := r.client.setRunning(ctx, h, false); err != nil {
					return err
				}
				return nil
			}
			files, err := r.client.files(ctx, h)
			if err != nil {
				return err
			}
			selected, err := selectRescueVideos(files, g.Code, r.opts.MinVideoSize)
			if err != nil {
				if err = r.failDownload(ctx, g, d, t, now, err.Error()); err != nil {
					return err
				}
				*active--
				continue
			}
			keep := map[int]bool{}
			d.MetadataBytes = rescueFileBytes(files)
			d.FilmBytes = rescueFilmBytes(selected)
			capacity, err := r.storageCapacity(0)
			if err != nil {
				return err
			}
			if !capacity {
				if err = r.failDownload(ctx, g, d, t, now, "补救磁盘预算或剩余容量不足"); err != nil {
					return err
				}
				*active--
				continue
			}
			for _, f := range selected {
				keep[f.Index] = true
			}
			exclude := []int{}
			for _, f := range files {
				if f.Priority > 0 && !keep[f.Index] {
					exclude = append(exclude, f.Index)
				}
			}
			if r.opts.DryRun {
				continue
			}
			if current, err := r.currentOwnedTorrent(ctx, g, d); err != nil {
				return err
			} else if !torrentIsStopped(current) {
				return fmt.Errorf("文件筛选前任务不再停止")
			}
			if len(exclude) > 0 {
				if err = r.client.exclude(ctx, h, exclude); err != nil {
					return err
				}
			}
			readback, err := r.client.files(ctx, h)
			if err != nil {
				return err
			}
			for _, f := range readback {
				if (keep[f.Index] && f.Priority == 0) || (!keep[f.Index] && f.Priority > 0) {
					return fmt.Errorf("文件筛选回读不一致")
				}
			}
			d.Files = selected
			d.Stage = "ready"
			d.Reason = "元数据正片核验通过，等待质量优先调度"
			*active--
			if err = r.store.save(); err != nil {
				return err
			}
			continue
		}
		if d.Stage == "ready" && torrentIsRunning(t) {
			// A user may resume a prepared fallback. Adopt its runtime evidence
			// without sending another start request.
			d.Stage = "running"
			d.StartedAt, d.LastProgress, d.LastObserved = now, now, now
			d.LastCompleted = t.Completed
			*active++
		}
		if d.Stage == "start-requested" {
			if torrentIsRunning(t) {
				d.Stage = "running"
			} else {
				d.Stage = "manual-stopped"
				d.Reason = "启动结果不明或被手动暂停，保留停止"
				*active--
			}
			continue
		}
		if d.Stage == "stopping" {
			if err := r.failDownload(ctx, g, d, t, now, d.Reason); err != nil {
				return err
			}
			*active--
			continue
		}
		if d.Stage == "running" {
			// A service/API observation gap is not evidence of stalled downloading.
			if observation, ok := r.store.state.Observations[h]; ok && observation.LastProgress.After(d.LastProgress) {
				d.LastProgress = observation.LastProgress
			}
			if torrentIsStopped(t) {
				d.Stage = "manual-stopped"
				d.Reason = "用户暂停，不自动恢复"
				*active--
				continue
			}
			if t.State == "queuedDL" || t.State == "moving" || strings.HasPrefix(t.State, "checking") || !network {
				d.LastProgress = now
				continue
			}
			if t.Completed > d.LastCompleted {
				d.LastProgress = now
			}
			if t.Completed < d.LastCompleted {
				d.LastProgress = now
			}
			if seconds := now.Sub(d.LastObserved).Seconds(); !d.LastObserved.IsZero() && seconds > 0 {
				rate := float64(max(0, t.Completed-d.LastCompleted)) / seconds
				d.AverageSpeed = 0.5*d.AverageSpeed + 0.5*rate
			}
			d.LastObserved = now
			d.LastCompleted = t.Completed
			d.RuntimeScore = runtimeRescueScore(t, d, now, r.opts.PollInterval)
			if now.Sub(d.LastProgress) >= r.opts.TrialTimeout || (now.Sub(d.LastProgress) >= r.opts.PoorScoreAfter && d.RuntimeScore < r.opts.MinRuntimeScore) {
				if err := r.failDownload(ctx, g, d, t, now, "试下载持续无有效进展"); err != nil {
					return err
				}
				*active--
			}
		}
	}
	if g.Stage != "open" {
		return nil
	}
	if network && now.Compare(g.NextSearch) >= 0 && *searched < r.opts.SearchBatch {
		*searched++
		results, err := r.search(ctx, g.Code)
		if err != nil {
			g.NextSearch = now.Add(r.opts.PollInterval)
			sourceErr = err
		} else {
			g.LastError = ""
			g.NextSearch = now.Add(r.opts.SearchInterval)
			sort.Slice(results, func(i, j int) bool {
				return rescueQualityLess(&rescueDownload{Candidate: results[i]}, &rescueDownload{Candidate: results[j]}, r.opts, true)
			})
			added := 0
			for _, candidate := range results {
				if added >= r.opts.MaxCandidates {
					break
				}
				if _, exists := live[candidate.Hash]; exists {
					continue
				}
				if d, exists := g.Downloads[candidate.Hash]; exists {
					if d.Stage == "queued" {
						d.Candidate = candidate
						added++
					}
					continue
				}
				g.Downloads[candidate.Hash] = &rescueDownload{Candidate: candidate, Stage: "queued", CreatedAt: now}
				added++
			}
			stepLog("补救检索 %s：来源返回=%d，去重后尚未添加=%d", g.Code, len(results), added)
			if err = r.store.save(); err != nil {
				return err
			}
		}
	}
	// Prefer a verified high quality completion over an earlier small copy.
	// Better candidates retain priority until their metadata/trial fails.
	sort.Slice(completed, func(i, j int) bool { return rescueQualityLess(completed[i], completed[j], r.opts, false) })
	for _, d := range completed {
		if rescueQualityTier(d.FilmBytes, r.opts) < rescueBestPotentialTier(g, r.opts, now) {
			d.Reason = "已完成，等待更高质量候选的试下载结果"
			continue
		}
		h := d.Candidate.Hash
		t := live[h]
		paths := []string{}
		for _, f := range d.Files {
			p, err := r.localDownloadPath(t.SavePath, f.Name)
			if err != nil {
				return err
			}
			paths = append(paths, p)
		}
		if r.opts.DryRun {
			stepLog("[dry-run] 验证并交付 %s hash=%s film_bytes=%d", g.Code, h, d.FilmBytes)
			continue
		}
		if err := r.verify(ctx, paths); err != nil {
			if d.Original {
				return err
			}
			wasActive := rescueActiveStage(d.Stage)
			if err = r.failDownload(ctx, g, d, t, now, err.Error()); err != nil {
				return err
			}
			if wasActive {
				*active--
			}
			continue
		}
		g.Winner = h
		g.Stage = "publishing"
		g.PublishPath = filepath.Join(r.opts.PublishDir, "rescue-"+g.Code+"-"+h[:12])
		if err := r.store.save(); err != nil {
			return err
		}
		if err := r.publishWinner(ctx, g, live); err != nil {
			return err
		}
		return r.processGroup(ctx, g, live, now, network, active, searched)
	}
	if !network {
		return nil
	}
	// Make room to inspect a newly discovered better version even when all
	// download slots are occupied by lower quality trials or completions.
	bestTier := rescueBestPotentialTier(g, r.opts, now)
	for _, h := range hashes {
		d := g.Downloads[h]
		if !d.Original && d.Stage == "running" && d.FilmBytes > 0 && rescueQualityTier(d.FilmBytes, r.opts) < bestTier {
			if err := r.parkForQuality(ctx, g, d); err != nil {
				return err
			}
			*active--
		}
	}
	perCode := 0
	queue := []*rescueDownload{}
	ready := []*rescueDownload{}
	for _, d := range g.Downloads {
		if !d.Original && rescueActiveStage(d.Stage) {
			perCode++
		}
		if d.Stage == "queued" && now.Compare(d.CooldownUntil) >= 0 {
			queue = append(queue, d)
		}
		if d.Stage == "ready" {
			ready = append(ready, d)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return rescueQualityLess(ready[i], ready[j], r.opts, false) })
	for _, d := range ready {
		if perCode >= r.opts.MaxPerCode || *active >= r.opts.MaxActive {
			break
		}
		if rescueQualityTier(d.FilmBytes, r.opts) < rescueBestPotentialTier(g, r.opts, now) {
			d.Reason = "保留为后备，优先试下载更高质量的正片"
			continue
		}
		capacity, err := r.storageCapacity(d.FilmBytes)
		if err != nil {
			return err
		}
		if !capacity {
			d.Reason = "磁盘预算不足，保持停止"
			continue
		}
		if r.opts.DryRun {
			stepLog("[dry-run] 启动已核实候选 %s hash=%s film_bytes=%d", g.Code, d.Candidate.Hash, d.FilmBytes)
			perCode++
			*active++
			continue
		}
		if err = r.startPreparedCandidate(ctx, g, d, now); err != nil {
			return err
		}
		perCode++
		*active++
	}
	if sourceErr != nil {
		return sourceErr
	}
	sort.Slice(queue, func(i, j int) bool {
		return rescueQualityLess(queue[i], queue[j], r.opts, true)
	})
	for _, d := range queue {
		if perCode >= r.opts.MaxPerCode || *active >= r.opts.MaxActive {
			break
		}
		if _, exists := live[d.Candidate.Hash]; exists {
			d.Stage = "removed"
			d.Reason = "资源已由其他入口加入"
			continue
		}
		reservation := rescueDownloadReservation(d)
		capacity, err := r.storageCapacity(reservation)
		if err != nil {
			return err
		}
		if !capacity {
			stepLog("补救磁盘预算或剩余容量不足，候选保持等待: %s", g.Code)
			break
		}
		if r.opts.DryRun {
			stepLog("[dry-run] 试下载 %s hash=%s initial_score=%d", g.Code, d.Candidate.Hash, d.Candidate.Score)
			perCode++
			*active++
			continue
		}
		if err := r.ensureCandidateScope(ctx); err != nil {
			return err
		}
		d.Stage = "adding"
		d.CreatedAt = now
		if err := r.store.save(); err != nil {
			return err
		}
		if err := r.store.guard(r.opts.GuardDir, g, d); err != nil {
			return err
		}
		if err := r.addCandidate(ctx, g, d); err != nil {
			return err
		}
		perCode++
		*active++
		stepLog("加入补救试下载 %s hash=%s，初筛分=%d", g.Code, d.Candidate.Hash, d.Candidate.Score)
	}
	return nil
}
func (r *torrentRescueRunner) ownsCandidate(g *rescueGroup, d *rescueDownload, t qbitTorrent) bool {
	return torrentHasTag(t, rescueTag) && path.Clean(t.SavePath) == path.Join(r.opts.CandidatePath, g.Code, d.Candidate.Hash) && (d.AddedOn == 0 || t.AddedOn == d.AddedOn)
}
func (r *torrentRescueRunner) currentOwnedTorrent(ctx context.Context, g *rescueGroup, d *rescueDownload) (qbitTorrent, error) {
	t, err := r.client.torrent(ctx, d.Candidate.Hash)
	if err != nil {
		return t, err
	}
	if t.AddedOn != d.AddedOn || (!d.Original && !r.ownsCandidate(g, d, t)) {
		return t, fmt.Errorf("任务身份或归属已变，取消本次操作")
	}
	return t, nil
}
func (r *torrentRescueRunner) startPreparedCandidate(ctx context.Context, g *rescueGroup, d *rescueDownload, now time.Time) error {
	t, err := r.currentOwnedTorrent(ctx, g, d)
	if err != nil {
		return err
	}
	if !torrentIsStopped(t) {
		return fmt.Errorf("启动前候选不再停止")
	}
	files, err := r.client.files(ctx, t.Hash)
	if err != nil {
		return err
	}
	selected, err := selectRescueVideos(files, g.Code, r.opts.MinVideoSize)
	if err != nil || rescueFilmBytes(selected) != d.FilmBytes || len(selected) != len(d.Files) {
		return fmt.Errorf("启动前正片选择发生变化，保持停止")
	}
	keep := map[int]qbitFile{}
	for _, f := range d.Files {
		keep[f.Index] = f
	}
	for _, f := range files {
		old, exists := keep[f.Index]
		if exists && (old.Name != f.Name || old.Size != f.Size || f.Priority == 0) || !exists && f.Priority > 0 {
			return fmt.Errorf("启动前文件选择回读不一致，保持停止")
		}
	}
	d.Stage = "start-requested"
	d.Reason = ""
	d.StartedAt, d.LastProgress, d.LastObserved = now, now, now
	d.LastCompleted = t.Completed
	if err = r.store.save(); err != nil {
		return err
	}
	// Start intent is durable. A lost reply never causes a blind resume.
	err = r.client.setRunning(ctx, t.Hash, true)
	current, readErr := r.currentOwnedTorrent(ctx, g, d)
	if readErr != nil {
		return readErr
	}
	if torrentIsRunning(current) {
		d.Stage = "running"
		stepLog("启动质量优先补救 %s hash=%s 正片=%.2f GiB 质量档=%d", g.Code, t.Hash, float64(d.FilmBytes)/(1<<30), rescueQualityTier(d.FilmBytes, r.opts))
	} else if err != nil {
		return err
	}
	return r.store.save()
}
func (r *torrentRescueRunner) parkForQuality(ctx context.Context, g *rescueGroup, d *rescueDownload) error {
	if r.opts.DryRun {
		stepLog("[dry-run] 暂停较低质量候选 %s hash=%s，保留为后备", g.Code, d.Candidate.Hash)
		d.Stage = "ready"
		return nil
	}
	t, err := r.currentOwnedTorrent(ctx, g, d)
	if err != nil {
		return err
	}
	d.Stage = "quality-stopping"
	d.Reason = "保留为后备，优先试下载更高质量的正片"
	if err = r.store.save(); err != nil {
		return err
	}
	if !torrentIsStopped(t) {
		if err = r.client.setRunning(ctx, t.Hash, false); err != nil {
			return err
		}
	}
	check, err := r.currentOwnedTorrent(ctx, g, d)
	if err != nil {
		return err
	}
	if !torrentIsStopped(check) {
		return fmt.Errorf("质量后备候选停止未获确认")
	}
	d.Stage = "ready"
	return r.store.save()
}
func (r *torrentRescueRunner) addCandidate(ctx context.Context, g *rescueGroup, d *rescueDownload) error {
	values := url.Values{"urls": {d.Candidate.Magnet}, "tags": {rescueTag}, "category": {r.opts.Category}, "savepath": {path.Join(r.opts.CandidatePath, g.Code, d.Candidate.Hash)}, "autoTMM": {"false"}, "stopCondition": {"MetadataReceived"}}
	if r.client.major == 4 {
		values.Set("paused", "false")
	} else {
		values.Set("stopped", "false")
	}
	data, _, err := r.client.request(ctx, "torrents/add", values, true)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "Ok." {
		return fmt.Errorf("qBittorrent 未确认添加候选")
	}
	return nil
}

func (r *torrentRescueRunner) ensureCandidateScope(ctx context.Context) error {
	if r.scopeReady {
		return nil
	}
	var categories map[string]json.RawMessage
	if err := r.client.getObject(ctx, "torrents/categories", nil, &categories); err != nil {
		return err
	}
	if _, exists := categories[r.opts.Category]; !exists {
		_, _, err := r.client.request(ctx, "torrents/createCategory", url.Values{"category": {r.opts.Category}}, true)
		if err != nil {
			return err
		}
		if err = r.client.getObject(ctx, "torrents/categories", nil, &categories); err != nil {
			return err
		}
		if _, exists = categories[r.opts.Category]; !exists {
			return fmt.Errorf("补救分类创建未获确认")
		}
	}
	if _, _, err := r.client.request(ctx, "torrents/createTags", url.Values{"tags": {rescueTag}}, true); err != nil {
		return err
	}
	var tags []string
	if err := r.client.getArray(ctx, "torrents/tags", nil, &tags); err != nil {
		return err
	}
	for _, tag := range tags {
		if tag == rescueTag {
			r.scopeReady = true
			return nil
		}
	}
	return fmt.Errorf("补救标签创建未获确认")
}
func (r *torrentRescueRunner) failDownload(ctx context.Context, g *rescueGroup, d *rescueDownload, t qbitTorrent, now time.Time, reason string) error {
	if r.opts.DryRun {
		stepLog("[dry-run] 淘汰 %s hash=%s：%s", g.Code, t.Hash, reason)
		return nil
	}
	current, err := r.currentOwnedTorrent(ctx, g, d)
	if err != nil {
		return err
	}
	t = current
	d.Stage = "stopping"
	d.Reason = reason
	if err := r.store.save(); err != nil {
		return err
	}
	if !torrentIsStopped(t) {
		if err := r.client.setRunning(ctx, t.Hash, false); err != nil {
			return err
		}
		check, err := r.client.torrent(ctx, t.Hash)
		if err != nil {
			return err
		}
		if check.AddedOn != d.AddedOn || !torrentIsStopped(check) {
			return fmt.Errorf("淘汰任务未确认停止")
		}
	}
	d.Stage = "failed"
	d.CooldownUntil = now.Add(r.opts.Cooldown)
	stepLog("淘汰补救候选 %s hash=%s：%s", g.Code, t.Hash, reason)
	return r.store.save()
}
func runtimeRescueScore(t qbitTorrent, d *rescueDownload, now time.Time, poll time.Duration) int {
	score := 10
	if t.Availability >= 1 {
		score += 35
	} else if t.Availability > 0 {
		score += 10
	}
	if now.Sub(d.LastProgress) <= 2*poll {
		score += 30
	}
	if d.AverageSpeed > 0 {
		eta := float64(t.AmountLeft) / d.AverageSpeed
		if eta < 86400 {
			score += 15
		} else {
			score += 5
		}
	}
	if t.Seeds > 0 {
		score += min(10, t.Seeds*2)
	}
	return score
}
func selectRescueVideos(files []qbitFile, code string, minSize int64) ([]qbitFile, error) {
	selected := []qbitFile{}
	parts := map[int]bool{}
	videoCount := 0
	var total int64
	for _, f := range files {
		if f.Size < minSize {
			continue
		}
		if _, ok := videoExts[strings.ToLower(path.Ext(f.Name))]; !ok {
			continue
		}
		if strings.Contains(strings.ToLower(path.Base(f.Name)), "sample") || strings.Contains(strings.ToLower(path.Base(f.Name)), "trailer") {
			continue
		}
		parsed, ok := preprocessCatalogNumber(f.Name)
		if !ok || parsed.Code != code {
			return nil, fmt.Errorf("候选包含其他番号或无法核实的正片，拒绝合集")
		}
		videoCount++
		if videoCount > 8 || parts[parsed.Part] {
			return nil, fmt.Errorf("候选包含过多视频或多份完整影片，拒绝合并计算大小")
		}
		parts[parsed.Part] = true
		if f.Priority == 0 {
			return nil, fmt.Errorf("正片或分段未全部选择，无法核实完整影片大小")
		}
		if f.Size > (1<<63-1)-total {
			return nil, fmt.Errorf("正片大小溢出")
		}
		total += f.Size
		selected = append(selected, f)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("候选没有符合番号及大小要求的正片")
	}
	if len(selected) > 1 {
		for i := 1; i <= len(selected); i++ {
			if !parts[i] {
				return nil, fmt.Errorf("多个视频必须是同一番号的连续分段，拒绝合集或多版本")
			}
		}
	}
	return selected, nil
}

func rescueTreeSize(dir string) (uint64, error) {
	var size uint64
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if os.IsNotExist(err) && p == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			size += uint64(info.Size())
		}
		return nil
	})
	return size, err
}
func (r *torrentRescueRunner) storageCapacity(nextSize int64) (bool, error) {
	root, err := r.localDownloadPath(r.opts.CandidatePath, ".budget")
	if err != nil {
		return false, err
	}
	root = filepath.Dir(root)
	used, err := rescueTreeSize(root)
	if err != nil {
		return false, err
	}
	reserved := uint64(0)
	for _, g := range r.store.state.Groups {
		for _, d := range g.Downloads {
			if d.Original || !rescueActiveStage(d.Stage) {
				continue
			}
			actual, err := rescueTreeSize(filepath.Join(root, g.Code, d.Candidate.Hash))
			if err != nil {
				return false, err
			}
			estimate := uint64(rescueDownloadReservation(d))
			if estimate > actual {
				reserved += estimate - actual
			}
		}
	}
	if nextSize != 0 {
		if nextSize < 0 {
			return false, fmt.Errorf("候选大小无效")
		}
		reserved += uint64(nextSize)
	} else if used == 0 && reserved == 0 {
		return true, nil
	}
	free, err := r.diskFree(r.opts.DownloadHostDir)
	if err != nil {
		return false, err
	}
	return used+reserved <= r.opts.StorageBudget && free >= r.opts.MinFree+reserved, nil
}
