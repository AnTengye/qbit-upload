package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type sourceStamp struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime_ns"`
}

type archiveJobFile struct {
	Path             string        `json:"path"`
	TempPath         string        `json:"temp_path"`
	Files            []string      `json:"files"`
	Format           archiveFormat `json:"format"`
	Stage            string        `json:"stage"` // planned -> moving -> ready -> uploaded -> deleted
	Size             int64         `json:"size"`
	SHA256           string        `json:"sha256"`
	NativeTaskID     string        `json:"native_task_id,omitempty"`
	SubmissionIntent time.Time     `json:"submission_intent,omitempty"`
	PreviousTaskIDs  []string      `json:"previous_task_ids,omitempty"`
}

type jobReport struct {
	Code        string `json:"code"`
	PreviewPath string `json:"preview_path,omitempty"`
	Reported    bool   `json:"reported"`
}

type archiveJob struct {
	Version          int              `json:"version"`
	ID               string           `json:"id"`
	SourcePath       string           `json:"source_path"`
	SourceDir        string           `json:"source_dir"`
	SourceBase       string           `json:"source_base"`
	Sources          []sourceStamp    `json:"sources"`
	Archives         []archiveJobFile `json:"archives"`
	Reports          []jobReport      `json:"reports"`
	Unmatched        []string         `json:"unmatched,omitempty"`
	RemoteDir        string           `json:"remote_dir"`
	NASURL           string           `json:"nas_url"`
	NASUsername      string           `json:"nas_username"`
	ReportURL        string           `json:"report_url"`
	DeleteSource     bool             `json:"delete_source"`
	AllowTgzFallback bool             `json:"allow_tgz_fallback"`
	ThumbnailsReady  bool             `json:"thumbnails_ready"`
	Done             bool             `json:"done"`
	Attempts         int              `json:"attempts"`
	LastError        string           `json:"last_error,omitempty"`
	NextRetry        time.Time        `json:"next_retry,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

type jobStore struct {
	dir     string
	release func()
}

var errJobWaiting = errors.New("已记录的任务等待定时重试")

func openJobStore(dir string) (*jobStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	release, err := lockJobStore(filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, err
	}
	return &jobStore{dir: dir, release: release}, nil
}

func (s *jobStore) close() { s.release() }

func (s *jobStore) save(job *archiveJob) error {
	job.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".job-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, job.ID+".json")); err != nil {
		return err
	}
	return syncJobDirectory(s.dir)
}

func (s *jobStore) loadAll() ([]*archiveJob, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var jobs []*archiveJob
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var job archiveJob
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, fmt.Errorf("任务记录损坏 %s: %w", entry.Name(), err)
		}
		if job.Version != 1 || job.ID+".json" != entry.Name() {
			return nil, fmt.Errorf("任务记录版本或 ID 不匹配: %s", entry.Name())
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

// uploadArchive must persist its submission intent BEFORE making a request.
// This lets a new process reconcile a native task after an interrupted POST.
type uploadArchiveFunc func(context.Context, *archiveJobFile, *archiveJob, func() error) error

type jobRunner struct {
	store  *jobStore
	opts   runOptions
	upload uploadArchiveFunc
	now    func() time.Time
}

func newJobRunner(store *jobStore, opts runOptions) *jobRunner {
	return &jobRunner{store: store, opts: opts, upload: func(ctx context.Context, a *archiveJobFile, j *archiveJob, save func() error) error {
		return uploadViaUGREEN(ctx, opts.Upload, a, j, save)
	}, now: time.Now}
}

func processSourceWithUpload(sourcePath string, opts runOptions) error {
	if opts.DryRun {
		return dryRunUpload(sourcePath, opts)
	}
	store, err := openJobStore(opts.Upload.StateDir)
	if err != nil {
		return err
	}
	defer store.close()
	runner := newJobRunner(store, opts)
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	jobs, err := store.loadAll()
	if err != nil {
		return err
	}
	// Recover an existing job even when its source has already been cleaned up.
	for _, job := range jobs {
		if job.SourcePath == absSource && !job.Done {
			if runner.now().Before(job.NextRetry) {
				stepLog("任务 %s 等待重试: %s", job.ID, job.NextRetry.Local().Format(time.RFC3339))
				return fmt.Errorf("%w: %s", errJobWaiting, job.ID)
			}
			return runner.attempt(context.Background(), job)
		}
	}
	job, err := planUploadJob(sourcePath, opts)
	if err != nil {
		return err
	}
	for _, old := range jobs {
		if old.ID == job.ID {
			stepLog("任务已完成，跳过重复处理: %s", old.ID)
			return nil
		}
	}
	if err := store.save(job); err != nil {
		return fmt.Errorf("保存待处理任务失败: %w", err)
	}
	return runner.attempt(context.Background(), job)
}

func planUploadJob(sourcePath string, opts runOptions) (*archiveJob, error) {
	input, err := prepareArchiveInput(sourcePath, opts.MinSizeMB*mb)
	if err != nil {
		return nil, err
	}
	dest := opts.Archive.DestDir
	if dest == "" {
		dest = opts.DestDir
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	temp, err := resolveArchiveTempDir(opts.Archive.TempDir)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	format := archiveFormat7z
	if _, err := discoverSevenZip(opts.SevenZip, filepath.Dir(exe), opts.Archive.Embedded7zDir, nil); err != nil {
		if opts.Archive.AllowTgzFallback {
			format = archiveFormatTgz
		}
	}
	outputs, err := planArchiveOutputs(input.SourceBase, dest, input.Files, format, opts.Archive.Split)
	if err != nil {
		return nil, err
	}
	job := &archiveJob{Version: 1, SourcePath: input.DeletePath, SourceDir: input.SourceDir, SourceBase: input.SourceBase,
		RemoteDir: opts.Upload.RemoteDir, NASURL: opts.Upload.BaseURL, NASUsername: opts.Upload.Username, ReportURL: opts.Report.URL,
		DeleteSource: opts.Archive.DeleteSource, AllowTgzFallback: opts.Archive.AllowTgzFallback, CreatedAt: time.Now().UTC()}
	for _, rel := range input.Files {
		stamp, err := stampSource(filepath.Join(input.SourceDir, rel))
		if err != nil {
			return nil, err
		}
		job.Sources = append(job.Sources, stamp)
	}
	identity, _ := json.Marshal(struct {
		Source            string
		Sources           []sourceStamp
		Dest, Remote, NAS string
	}{job.SourcePath, job.Sources, dest, job.RemoteDir, job.NASURL})
	sum := sha256.Sum256(identity)
	job.ID = hex.EncodeToString(sum[:])[:32]
	for i, out := range outputs {
		job.Archives = append(job.Archives, archiveJobFile{Path: out.Path,
			TempPath: filepath.Join(temp, fmt.Sprintf("qbit-%s-%d.%s", job.ID, i, format)), Files: out.Files, Format: format, Stage: "planned"})
	}
	previewDir := filepath.Join(opts.Upload.StateDir, job.ID+"-previews")
	tracker, err := newFilmReportTracker(input.SourceBase, previewDir, input.Files, opts.Thumbnail.Enabled)
	if err != nil {
		return nil, err
	}
	for _, plan := range tracker.Plans() {
		job.Reports = append(job.Reports, jobReport{Code: plan.Code, PreviewPath: plan.PreviewPath})
	}
	for _, rel := range tracker.Unmatched() {
		job.Unmatched = append(job.Unmatched, rel)
		stepLog("WARN: 无法识别番号，上传后会保留压缩包: %s", rel)
	}
	return job, nil
}

func dryRunUpload(sourcePath string, opts runOptions) error {
	job, err := planUploadJob(sourcePath, opts)
	if err != nil {
		return err
	}
	fmt.Printf("[dry-run] 先移动到目标目录，再上传至 %s\n", job.RemoteDir)
	for _, archive := range job.Archives {
		fmt.Printf("[dry-run]   - %s\n", archive.Path)
	}
	for _, report := range job.Reports {
		fmt.Printf("[dry-run] 上传完成后上报 %s status=5（已归档）\n", report.Code)
	}
	fmt.Printf("[dry-run] 上报成功后删除压缩包；失败或中断保留，重启恢复或 %s 后重试\n", opts.Upload.RetryInterval)
	return nil
}

func (r *jobRunner) attempt(ctx context.Context, job *archiveJob) error {
	if job.Done {
		return nil
	}
	job.Attempts++
	if err := r.store.save(job); err != nil {
		return err
	}
	err := r.advance(ctx, job)
	if err != nil {
		job.LastError = err.Error()
		job.NextRetry = r.now().Add(r.opts.Upload.RetryInterval).UTC()
		if saveErr := r.store.save(job); saveErr != nil {
			return errors.Join(err, fmt.Errorf("保存重试记录失败: %w", saveErr))
		}
		stepLog("任务 %s 未完成，已记录；下一次重试 %s: %v", job.ID, job.NextRetry.Local().Format(time.RFC3339), err)
		return err
	}
	job.LastError = ""
	job.NextRetry = time.Time{}
	return r.store.save(job)
}

func (r *jobRunner) advance(ctx context.Context, job *archiveJob) error {
	if job.NASURL != r.opts.Upload.BaseURL || job.NASUsername != r.opts.Upload.Username || job.RemoteDir != r.opts.Upload.RemoteDir || job.ReportURL != r.opts.Report.URL {
		return fmt.Errorf("待处理任务的 NAS 账号、网盘路径或上报地址与当前配置不同；请恢复原配置")
	}
	if !job.ThumbnailsReady {
		if r.opts.Thumbnail.Enabled {
			// Each job owns a private preview directory, so another job cannot
			// replace an image while an earlier report is waiting to retry.
			previewDir := filepath.Join(r.store.dir, job.ID+"-previews")
			if err := os.RemoveAll(previewDir); err != nil {
				return err
			}
			if err := os.MkdirAll(previewDir, 0o700); err != nil {
				return err
			}
			thumbOpts := r.opts.Thumbnail
			thumbOpts.DestDir = previewDir
			var files []string
			for _, a := range job.Archives {
				files = append(files, a.Files...)
			}
			if err := generateThumbnails(job.SourceDir, job.SourceBase, previewDir, files, thumbOpts); err != nil {
				return err
			}
		}
		job.ThumbnailsReady = true
		if err := r.store.save(job); err != nil {
			return err
		}
	}
	for i := range job.Archives {
		a := &job.Archives[i]
		if a.Stage == "planned" || a.Stage == "moving" {
			if err := r.prepareArchive(job, a); err != nil {
				return err
			}
		}
		if a.Stage == "ready" {
			if err := verifyArchive(a); err != nil {
				return err
			}
			stepLog("调用绿联网盘工具上传: %s", a.Path)
			if err := r.upload(ctx, a, job, func() error { return r.store.save(job) }); err != nil {
				return err
			}
			a.Stage = "uploaded"
			if err := r.store.save(job); err != nil {
				return err
			}
			stepLog("网盘上传已完成: %s（原生任务 %s）", a.Path, a.NativeTaskID)
		}
		if a.Stage != "uploaded" && a.Stage != "deleted" {
			return fmt.Errorf("未知压缩包阶段: %s", a.Stage)
		}
	}
	if len(job.Reports) == 0 || len(job.Unmatched) > 0 {
		return fmt.Errorf("未识别到番号，保留本地压缩包")
	}
	reporter := newFilmReporter(r.opts.Report)
	for i := range job.Reports {
		report := &job.Reports[i]
		if report.Reported {
			continue
		}
		if !r.opts.Report.Enabled {
			return fmt.Errorf("未启用上报，保留本地压缩包")
		}
		if strings.TrimSpace(r.opts.Report.APIKey) == "" {
			return fmt.Errorf("未配置上报 API Key，保留本地压缩包")
		}
		status := 5
		payload := reportPayload{Code: report.Code, Status: &status, IdempotencyKey: job.ID + "-" + report.Code}
		if report.PreviewPath != "" {
			preview, err := loadReportPreview(report.PreviewPath)
			if err != nil {
				return err
			}
			payload.Preview = preview
		}
		requestCtx, cancel := context.WithTimeout(ctx, r.opts.Report.Timeout)
		_, err := reporter.send(requestCtx, payload)
		cancel()
		if err != nil {
			return fmt.Errorf("上报 %s status=5 失败: %w", report.Code, err)
		}
		report.Reported = true
		// Persist acknowledgement before ANY local cleanup. A restart from
		// this point performs cleanup without another upload/report request.
		if err := r.store.save(job); err != nil {
			return err
		}
		stepLog("影片上报成功: %s status=5（已归档）", report.Code)
	}
	for i := range job.Archives {
		a := &job.Archives[i]
		if a.Stage == "deleted" {
			continue
		}
		if _, err := os.Lstat(a.Path); !errors.Is(err, os.ErrNotExist) {
			if err := verifyArchive(a); err != nil {
				return err
			}
			if err := os.Remove(a.Path); err != nil {
				return err
			}
			if err := syncJobDirectory(filepath.Dir(a.Path)); err != nil {
				return err
			}
		}
		a.Stage = "deleted"
		if err := r.store.save(job); err != nil {
			return err
		}
	}
	if job.DeleteSource {
		for _, original := range job.Sources {
			current, err := stampSource(original.Path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if current != original {
				stepLog("WARN: 源视频在处理后发生变化，保留文件: %s", original.Path)
				continue
			}
			if err := os.Remove(original.Path); err != nil {
				return err
			}
		}
	}
	// Only previews belonging to this job can be removed.
	previewDir := filepath.Join(r.store.dir, job.ID+"-previews")
	if err := os.RemoveAll(previewDir); err != nil {
		return err
	}
	job.Done = true
	stepLog("上传、上报和本地清理完成: %s", job.ID)
	return nil
}

func (r *jobRunner) prepareArchive(job *archiveJob, a *archiveJobFile) error {
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return err
	}
	if a.Stage == "planned" {
		if err := ensureOutputDoesNotExist(a.Path); err != nil {
			return err
		}
		for _, stamp := range job.Sources {
			current, err := stampSource(stamp.Path)
			if err != nil {
				return err
			}
			if current != stamp {
				return fmt.Errorf("源视频已变化，不能继续打包: %s", stamp.Path)
			}
		}
		// This exact temporary filename was recorded before compression.
		if err := os.Remove(a.TempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if a.Format == archiveFormat7z {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			seven, err := discoverSevenZip(r.opts.SevenZip, filepath.Dir(exe), r.opts.Archive.Embedded7zDir, nil)
			if err == nil && strings.TrimSpace(r.opts.Password) == "" {
				return fmt.Errorf("7z 加密密码不能为空")
			}
			if err == nil {
				err = compressWith7z(job.SourceDir, a.TempPath, a.Files, seven, r.opts.Password, r.opts.ReserveMemoryMB)
			}
			if err != nil {
				if !job.AllowTgzFallback {
					return err
				}
				if removeErr := os.Remove(a.TempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				a.Format = archiveFormatTgz
				a.Path = archivePathWithFormat(a.Path, a.Format)
				a.TempPath = archivePathWithFormat(a.TempPath, a.Format)
				if err := r.store.save(job); err != nil {
					return err
				}
				if err := ensureOutputDoesNotExist(a.Path); err != nil {
					return err
				}
			}
		}
		if a.Format == archiveFormatTgz {
			if err := createTgzArchive(job.SourceDir, a.TempPath, a.Files); err != nil {
				return err
			}
		}
		size, hash, err := hashRegularFile(a.TempPath)
		if err != nil {
			return err
		}
		a.Size, a.SHA256, a.Stage = size, hash, "moving"
		if err := r.store.save(job); err != nil {
			return err
		}
	}
	// A previous process may have renamed the archive before it could save
	// "ready". Verify the bytes instead of overwriting or recompressing it.
	if _, err := os.Lstat(a.Path); err == nil {
		if err := verifyArchive(a); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if err := moveArchiveAtomically(a.TempPath, a.Path, job.ID); err != nil {
			return err
		}
		if err := verifyArchive(a); err != nil {
			return err
		}
	}
	if err := os.Remove(a.TempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a.Stage = "ready"
	return r.store.save(job)
}

func stampSource(path string) (sourceStamp, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return sourceStamp{}, err
	}
	if !info.Mode().IsRegular() {
		return sourceStamp{}, fmt.Errorf("不是普通文件: %s", path)
	}
	return sourceStamp{Path: path, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

func hashRegularFile(path string) (int64, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", fmt.Errorf("不是普通文件: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if n != info.Size() || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) || after.Size() != n {
		return 0, "", fmt.Errorf("计算校验值时文件发生变化: %s", path)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func verifyArchive(a *archiveJobFile) error {
	size, hash, err := hashRegularFile(a.Path)
	if err != nil {
		return err
	}
	if size != a.Size || hash != a.SHA256 {
		return fmt.Errorf("压缩包内容与任务记录不一致，保留文件: %s", a.Path)
	}
	return nil
}

func moveArchiveAtomically(src, dst, id string) error {
	if err := ensureOutputDoesNotExist(dst); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return syncJobDirectory(filepath.Dir(dst))
	}
	// Cross-filesystem copies are invisible to the uploader until a complete,
	// fsynced file is renamed into the final destination.
	staging := dst + "." + id + ".moving"
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	sourceInfo, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer os.Remove(staging)
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if err := ensureOutputDoesNotExist(dst); err != nil {
		return err
	}
	if err := os.Rename(staging, dst); err != nil {
		return err
	}
	return syncJobDirectory(filepath.Dir(dst))
}

func retryPendingJobs(opts runOptions, force bool) error {
	if !opts.Upload.Enabled || opts.DryRun {
		return nil
	}
	store, err := openJobStore(opts.Upload.StateDir)
	if err != nil {
		return err
	}
	defer store.close()
	return newJobRunner(store, opts).retry(force)
}

func (runner *jobRunner) retry(force bool) error {
	jobs, err := runner.store.loadAll()
	if err != nil {
		return err
	}
	var failures []error
	for _, job := range jobs {
		if job.Done || (!force && runner.now().Before(job.NextRetry)) {
			continue
		}
		if err := runner.attempt(context.Background(), job); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func newRetryCmd() *cobra.Command {
	return &cobra.Command{Use: "retry", Short: "立即恢复持久化队列中的未完成上传、上报和清理任务", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if _, err := initLogging(cfg); err != nil {
				return err
			}
			opts, err := resolveOptions(cmd, cfg)
			if err != nil {
				return err
			}
			if !opts.Upload.Enabled {
				return fmt.Errorf("retry 需要 upload.enabled: true")
			}
			return retryPendingJobs(opts, true)
		}}
}
