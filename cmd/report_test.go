package cmd

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestResolveReportOptionsDefaultsToEnabled(t *testing.T) {
	t.Setenv("QBIT_UPLOAD_REPORT_API_KEY", "")
	t.Setenv("AVISTER_FILM_EXTERNAL_API_KEY", "")

	opts, err := resolveReportOptions(reportConfig{})
	if err != nil {
		t.Fatalf("resolveReportOptions returned error: %v", err)
	}
	if !opts.Enabled {
		t.Fatal("Enabled = false, want default true")
	}
	if opts.URL != defaultReportURL {
		t.Fatalf("URL = %q, want %q", opts.URL, defaultReportURL)
	}
	if opts.Timeout != defaultReportTimeout {
		t.Fatalf("Timeout = %s, want %s", opts.Timeout, defaultReportTimeout)
	}
}

func TestResolveReportOptionsCanBeDisabled(t *testing.T) {
	disabled := false
	opts, err := resolveReportOptions(reportConfig{Enabled: &disabled, URL: "not a URL"})
	if err != nil {
		t.Fatalf("disabled reporting should ignore URL validation: %v", err)
	}
	if opts.Enabled {
		t.Fatal("Enabled = true, want false")
	}
}

func TestFilmReporterPreprocessesCodeAndUploadsPreview(t *testing.T) {
	previewPath := writeTestJPEG(t)

	var mu sync.Mutex
	requestCount := 0
	var gotCode, gotKey, gotFilename, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseMultipartForm(maxReportPreviewSize); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("previewFile")
		if err != nil {
			t.Errorf("FormFile(previewFile): %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = file.Close()
		mu.Lock()
		requestCount++
		gotCode = r.FormValue("code")
		gotKey = r.Header.Get("Key")
		gotFilename = header.Filename
		gotContentType = header.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	reporter := newFilmReporter(reportOptions{
		Enabled: true,
		URL:     server.URL,
		APIKey:  "test-api-key",
		Timeout: 2 * time.Second,
	})
	if !reporter.Queue("hhd800.com@MIRD-275-B.7z", previewPath) {
		t.Fatal("Queue returned false")
	}
	// The same canonical film is deduplicated before a second HTTP request.
	if reporter.Queue("MIRD-275-A.mp4", previewPath) {
		t.Fatal("duplicate Queue returned true")
	}
	reporter.Wait()

	mu.Lock()
	defer mu.Unlock()
	if requestCount != 1 || gotCode != "MIRD-275" || gotKey != "test-api-key" {
		t.Fatalf("requestCount=%d code=%q key=%q", requestCount, gotCode, gotKey)
	}
	if gotFilename != filepath.Base(previewPath) {
		t.Fatalf("preview filename = %q, want %q", gotFilename, filepath.Base(previewPath))
	}
	if gotContentType != "image/jpeg" {
		t.Fatalf("preview content type = %q, want image/jpeg", gotContentType)
	}
}

func TestFilmReporterTreatsConflictAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()

	reporter := newFilmReporter(reportOptions{
		Enabled: true,
		URL:     server.URL,
		APIKey:  "test-api-key",
		Timeout: 2 * time.Second,
	})
	if !reporter.Queue("ABC-123.mp4", "") {
		t.Fatal("Queue returned false")
	}
	reporter.Wait()
}

func TestFilmReportTrackerWaitsForAllParts(t *testing.T) {
	tracker, err := newFilmReportTracker("source", "/thumbs", []string{
		"MIRD-275-A.mp4",
		"MIRD-275-B.mp4",
		"CAWD-999.mp4",
		"video.mp4",
	}, true)
	if err != nil {
		t.Fatalf("newFilmReportTracker returned error: %v", err)
	}
	if got := tracker.Unmatched(); !reflect.DeepEqual(got, []string{"video.mp4"}) {
		t.Fatalf("Unmatched = %#v", got)
	}
	if ready := tracker.Complete([]string{"MIRD-275-A.mp4"}); len(ready) != 0 {
		t.Fatalf("first part unexpectedly ready: %#v", ready)
	}
	ready := tracker.Complete([]string{"CAWD-999.mp4", "MIRD-275-B.mp4"})
	if len(ready) != 2 || ready[0].Code != "CAWD-999" || ready[1].Code != "MIRD-275" {
		t.Fatalf("ready = %#v", ready)
	}
	if ready := tracker.Complete([]string{"MIRD-275-B.mp4"}); len(ready) != 0 {
		t.Fatalf("duplicate completion unexpectedly ready: %#v", ready)
	}
}

func TestProcessSourceReportsAfterArchiveCompletes(t *testing.T) {
	sourceDir := t.TempDir()
	videoPath := filepath.Join(sourceDir, "4123.com@CAWD-999.mp4")
	if err := os.WriteFile(videoPath, []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reported := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxReportPreviewSize); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reported <- r.FormValue("code")
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	destDir := t.TempDir()
	opts := runOptions{
		DestDir:   destDir,
		SevenZip:  filepath.Join(t.TempDir(), "missing-7z"),
		MinSizeMB: 0,
		Archive: archiveOptions{
			AllowTgzFallback: true,
			TempDir:          t.TempDir(),
			DeleteSource:     false,
			DestDir:          destDir,
		},
		Thumbnail: thumbnailOptions{Enabled: false, DestDir: destDir},
		Report: reportOptions{
			Enabled: true,
			URL:     server.URL,
			APIKey:  "test-api-key",
			Timeout: 2 * time.Second,
		},
	}
	if err := processSource(videoPath, opts); err != nil {
		t.Fatalf("processSource returned error: %v", err)
	}

	select {
	case code := <-reported:
		if code != "CAWD-999" {
			t.Fatalf("reported code = %q, want CAWD-999", code)
		}
	default:
		t.Fatal("report request was not received")
	}
	if _, err := os.Stat(filepath.Join(destDir, "4123.com@CAWD-999.tgz")); err != nil {
		t.Fatalf("archive was not completed before report: %v", err)
	}
}

func writeTestJPEG(t *testing.T) string {
	t.Helper()
	var data bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	if err := jpeg.Encode(&data, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "preview.jpg")
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
