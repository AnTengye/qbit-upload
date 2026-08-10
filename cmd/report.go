package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultReportURL       = "http://89.116.88.182:8081/api/external/films"
	defaultReportTimeout   = 30 * time.Second
	maxReportPreviewSize   = int64(8 * 1024 * 1024)
	maxReportResponseBytes = int64(4 * 1024)
	maxConcurrentReports   = 4
)

type reportConfig struct {
	Enabled *bool  `json:"enabled" yaml:"enabled"`
	URL     string `json:"url" yaml:"url"`
	APIKey  string `json:"api_key" yaml:"api_key"`
	Timeout string `json:"timeout" yaml:"timeout"`
}

type reportOptions struct {
	Enabled bool
	URL     string
	APIKey  string
	Timeout time.Duration
}

type filmReportPlan struct {
	Code        string
	PreviewPath string
	Files       []string
}

type filmReportTracker struct {
	plans       map[string]filmReportPlan
	order       []string
	codeByFile  map[string]string
	remaining   map[string]int
	queued      map[string]struct{}
	unmatched   []string
	previewByID map[string]string
}

type reportPreview struct {
	Name        string
	ContentType string
	Data        []byte
}

type reportPayload struct {
	Code    string
	Preview *reportPreview
}

type reportOutcome int

const (
	reportCreated reportOutcome = iota
	reportAlreadyExists
)

type reportHTTPError struct {
	StatusCode int
	Body       string
}

func (e *reportHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

type filmReporter struct {
	opts   reportOptions
	client *http.Client

	wg sync.WaitGroup
	mu sync.Mutex
	// slots bounds preview buffering and outbound requests for large batches.
	slots chan struct{}

	seen             map[string]struct{}
	missingKeyLogged bool
}

func resolveReportOptions(cfg reportConfig) (reportOptions, error) {
	opts := reportOptions{
		Enabled: true,
		URL:     defaultReportURL,
		Timeout: defaultReportTimeout,
	}
	if cfg.Enabled != nil {
		opts.Enabled = *cfg.Enabled
	}
	if value := strings.TrimSpace(cfg.URL); value != "" {
		opts.URL = value
	}
	opts.APIKey = strings.TrimSpace(cfg.APIKey)
	if opts.APIKey == "" {
		opts.APIKey = strings.TrimSpace(os.Getenv("QBIT_UPLOAD_REPORT_API_KEY"))
	}
	if opts.APIKey == "" {
		opts.APIKey = strings.TrimSpace(os.Getenv("AVISTER_FILM_EXTERNAL_API_KEY"))
	}
	if value := strings.TrimSpace(cfg.Timeout); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return reportOptions{}, fmt.Errorf("report timeout 无效: %w", err)
		}
		opts.Timeout = timeout
	}
	if opts.Timeout <= 0 {
		return reportOptions{}, fmt.Errorf("report timeout 必须大于 0")
	}
	if opts.Enabled {
		parsed, err := url.Parse(opts.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return reportOptions{}, fmt.Errorf("report url 必须是有效的 http/https 地址: %s", opts.URL)
		}
		if parsed.User != nil {
			return reportOptions{}, fmt.Errorf("report url 不允许包含用户名或密码")
		}
	}
	return opts, nil
}

func newFilmReportTracker(sourceBase, thumbnailDest string, files []string, thumbnailsEnabled bool) (*filmReportTracker, error) {
	tracker := &filmReportTracker{
		plans:       make(map[string]filmReportPlan),
		codeByFile:  make(map[string]string),
		remaining:   make(map[string]int),
		queued:      make(map[string]struct{}),
		previewByID: make(map[string]string),
	}

	if thumbnailsEnabled {
		outputs, err := planThumbnailOutputs(sourceBase, thumbnailDest, files)
		if err != nil {
			return nil, err
		}
		for _, output := range outputs {
			tracker.previewByID[output.InputRel] = output.OutputPath
		}
	}

	for _, rel := range files {
		parsed, ok := preprocessCatalogNumber(filepath.Join(sourceBase, rel))
		if !ok {
			tracker.unmatched = append(tracker.unmatched, rel)
			continue
		}
		code := parsed.Code
		tracker.codeByFile[rel] = code
		tracker.remaining[code]++
		plan, exists := tracker.plans[code]
		if !exists {
			plan = filmReportPlan{Code: code, PreviewPath: tracker.previewByID[rel]}
			tracker.order = append(tracker.order, code)
		}
		plan.Files = append(plan.Files, rel)
		if plan.PreviewPath == "" {
			plan.PreviewPath = tracker.previewByID[rel]
		}
		tracker.plans[code] = plan
	}
	return tracker, nil
}

func (t *filmReportTracker) Plans() []filmReportPlan {
	plans := make([]filmReportPlan, 0, len(t.order))
	for _, code := range t.order {
		plans = append(plans, t.plans[code])
	}
	return plans
}

func (t *filmReportTracker) Unmatched() []string {
	return append([]string(nil), t.unmatched...)
}

// Complete marks successfully archived files. A film becomes reportable only
// after all pending files with the same canonical code have completed.
func (t *filmReportTracker) Complete(files []string) []filmReportPlan {
	ready := make([]filmReportPlan, 0)
	for _, rel := range files {
		code, ok := t.codeByFile[rel]
		if !ok || t.remaining[code] <= 0 {
			continue
		}
		t.remaining[code]--
		if t.remaining[code] != 0 {
			continue
		}
		if _, exists := t.queued[code]; exists {
			continue
		}
		t.queued[code] = struct{}{}
		ready = append(ready, t.plans[code])
	}
	return ready
}

func newFilmReporter(opts reportOptions) *filmReporter {
	return &filmReporter{
		opts:   opts,
		client: &http.Client{Timeout: opts.Timeout},
		seen:   make(map[string]struct{}),
		slots:  make(chan struct{}, maxConcurrentReports),
	}
}

// Queue preprocesses the code immediately before upload, loads the optional
// preview while it is still available, and performs the HTTP request in the
// background. Reporting is best effort and never changes the archive result.
func (r *filmReporter) Queue(rawCode, previewPath string) bool {
	if !r.opts.Enabled {
		return false
	}
	parsed, ok := preprocessCatalogNumber(rawCode)
	if !ok {
		stepLog("WARN: 无法从文件名预处理番号，跳过上报: %s", rawCode)
		return false
	}
	code := parsed.Code

	r.mu.Lock()
	if _, exists := r.seen[code]; exists {
		r.mu.Unlock()
		return false
	}
	if strings.TrimSpace(r.opts.APIKey) == "" {
		if !r.missingKeyLogged {
			stepLog("WARN: 上报默认开启，但未配置 API Key；本次跳过影片上报")
			r.missingKeyLogged = true
		}
		r.mu.Unlock()
		return false
	}
	r.seen[code] = struct{}{}
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.slots <- struct{}{}
		defer func() { <-r.slots }()

		payload := reportPayload{Code: code}
		if strings.TrimSpace(previewPath) != "" {
			preview, err := loadReportPreview(previewPath)
			if err != nil {
				stepLog("WARN: 读取番号 %s 的预览图失败，将仅上报番号: %v", code, err)
			} else {
				payload.Preview = preview
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
		defer cancel()
		outcome, err := r.send(ctx, payload)
		if err != nil {
			if httpErr, ok := err.(*reportHTTPError); ok && httpErr.StatusCode == http.StatusBadGateway {
				stepLog("WARN: 番号 %s 的影片记录可能已创建，但预览图上传失败；不自动重试: %v", code, err)
				return
			}
			stepLog("WARN: 番号 %s 异步上报失败（不影响归档）: %v", code, err)
			return
		}
		if outcome == reportAlreadyExists {
			stepLog("影片上报完成（已存在）: %s", code)
			return
		}
		stepLog("影片异步上报完成: %s", code)
	}()
	return true
}

func (r *filmReporter) Wait() {
	r.wg.Wait()
}

func (r *filmReporter) send(ctx context.Context, payload reportPayload) (reportOutcome, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("code", payload.Code); err != nil {
		return reportCreated, fmt.Errorf("写入上报番号失败: %w", err)
	}
	if payload.Preview != nil {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name":     "previewFile",
			"filename": payload.Preview.Name,
		}))
		header.Set("Content-Type", payload.Preview.ContentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return reportCreated, fmt.Errorf("创建预览图表单失败: %w", err)
		}
		if _, err := part.Write(payload.Preview.Data); err != nil {
			return reportCreated, fmt.Errorf("写入预览图表单失败: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return reportCreated, fmt.Errorf("结束上报表单失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.opts.URL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return reportCreated, fmt.Errorf("创建上报请求失败: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Key", r.opts.APIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return reportCreated, fmt.Errorf("调用影片上报接口失败: %w", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxReportResponseBytes))

	switch resp.StatusCode {
	case http.StatusCreated:
		return reportCreated, nil
	case http.StatusConflict:
		return reportAlreadyExists, nil
	default:
		return reportCreated, &reportHTTPError{
			StatusCode: resp.StatusCode,
			Body:       compactReportResponse(string(responseBody)),
		}
	}
}

func loadReportPreview(previewPath string) (*reportPreview, error) {
	info, err := os.Stat(previewPath)
	if err != nil {
		return nil, fmt.Errorf("读取文件信息 %s: %w", previewPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("预览图路径是目录: %s", previewPath)
	}
	if info.Size() > maxReportPreviewSize {
		return nil, fmt.Errorf("预览图超过 8 MiB 限制: %s", previewPath)
	}
	data, err := os.ReadFile(previewPath)
	if err != nil {
		return nil, fmt.Errorf("读取预览图 %s: %w", previewPath, err)
	}
	contentType := http.DetectContentType(data)
	if !isReportImageType(contentType) {
		return nil, fmt.Errorf("预览图类型不支持（%s）: %s", contentType, previewPath)
	}
	return &reportPreview{
		Name:        filepath.Base(previewPath),
		ContentType: contentType,
		Data:        data,
	}, nil
}

func isReportImageType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func compactReportResponse(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
