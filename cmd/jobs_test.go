package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type jobFixture struct {
	opts   runOptions
	job    *archiveJob
	store  *jobStore
	runner *jobRunner
}

func newJobFixture(t *testing.T, reportURL string, names ...string) *jobFixture {
	t.Helper()
	source, dest, state, temp := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	if len(names) == 0 {
		names = []string{"ABC-123.mp4"}
	}
	for _, name := range names {
		mustWriteFile(t, filepath.Join(source, name), "test-video-data")
	}
	opts := runOptions{DestDir: dest, SevenZip: filepath.Join(t.TempDir(), "missing-seven-zip"),
		Archive: archiveOptions{AllowTgzFallback: true, DestDir: dest, TempDir: temp, DeleteSource: true},
		Report:  reportOptions{Enabled: true, URL: reportURL, APIKey: "private-test-key", Timeout: 2 * time.Second},
		Upload:  uploadOptions{Enabled: true, BaseURL: "https://nas.test", Username: "test-user", RemoteDir: "/archive", StateDir: state, RetryInterval: 12 * time.Hour}}
	job, err := planUploadJob(source, opts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openJobStore(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(job); err != nil {
		t.Fatal(err)
	}
	f := &jobFixture{opts: opts, job: job, store: store, runner: newJobRunner(store, opts)}
	t.Cleanup(func() { f.store.close() })
	return f
}

func (f *jobFixture) restart(t *testing.T) {
	t.Helper()
	f.store.close()
	store, err := openJobStore(f.opts.Upload.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	jobs, err := store.loadAll()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("load: %v %v", jobs, err)
	}
	f.job = jobs[0]
	f.runner = newJobRunner(store, f.opts)
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected retained %s: %v", path, err)
	}
}
func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected removed %s: %v", path, err)
	}
}

func successfulTestUpload(ctx context.Context, a *archiveJobFile, j *archiveJob, save func() error) error {
	a.NativeTaskID = "verified-native-task"
	return save()
}

func TestUploadPipelineMovesUploadsReportsStatus5ThenDeletes(t *testing.T) {
	var uploaded atomic.Bool
	requests := make(chan map[string]any, 1)
	var archivePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !uploaded.Load() {
			t.Error("report happened before upload completion")
		}
		if _, err := os.Stat(archivePath); err != nil {
			t.Error("archive deleted before report acknowledgement")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Key") != "private-test-key" || r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing report headers")
		}
		requests <- payload
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	f := newJobFixture(t, server.URL)
	archivePath = f.job.Archives[0].Path
	f.runner.upload = func(ctx context.Context, a *archiveJobFile, j *archiveJob, save func() error) error {
		assertExists(t, a.Path)
		assertRemoved(t, a.TempPath)
		uploaded.Store(true)
		return successfulTestUpload(ctx, a, j, save)
	}
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	payload := <-requests
	if payload["status"] != float64(5) || payload["code"] != "ABC-123" {
		t.Fatalf("payload: %v", payload)
	}
	assertRemoved(t, archivePath)
	assertRemoved(t, f.job.Sources[0].Path)
	f.restart(t)
	if !f.job.Done || !f.job.Reports[0].Reported || f.job.Archives[0].Stage != "deleted" {
		t.Fatalf("not durably completed: %+v", f.job)
	}
	data, err := os.ReadFile(filepath.Join(f.store.dir, f.job.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-test-key") {
		t.Fatal("credential written to job record")
	}
}

func TestUploadFailurePreservesFilesAndSchedules12HourRetry(t *testing.T) {
	f := newJobFixture(t, "http://report.test")
	when := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time { return when }
	f.runner.upload = func(ctx context.Context, a *archiveJobFile, j *archiveJob, save func() error) error {
		a.SubmissionIntent = when
		if err := save(); err != nil {
			return err
		}
		return errors.New("interrupted upload")
	}
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("expected failed upload")
	}
	assertExists(t, f.job.Archives[0].Path)
	assertExists(t, f.job.Sources[0].Path)
	f.restart(t)
	if f.job.NextRetry != when.Add(12*time.Hour) || f.job.LastError == "" || f.job.Archives[0].SubmissionIntent.IsZero() {
		t.Fatalf("missing retry state: %+v", f.job)
	}
}

func TestReportFailureRetriesAfterRestartWithoutUploadingAgain(t *testing.T) {
	var attempts atomic.Int32
	keys := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer server.Close()
	f := newJobFixture(t, server.URL)
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("expected failed report")
	}
	assertExists(t, f.job.Archives[0].Path)
	assertExists(t, f.job.Sources[0].Path)
	f.restart(t)
	f.runner.upload = func(context.Context, *archiveJobFile, *archiveJob, func() error) error {
		t.Fatal("upload repeated after completion was saved")
		return nil
	}
	if err := f.runner.retry(true); err != nil {
		t.Fatal(err)
	}
	if first, second := <-keys, <-keys; first != second {
		t.Fatal("idempotency key changed across restart")
	}
	assertRemoved(t, f.job.Archives[0].Path)
}

func TestRetryWaitsUntilDueAndStartupForcesRecovery(t *testing.T) {
	f := newJobFixture(t, "http://report.test")
	now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time { return now }
	count := 0
	f.runner.upload = func(context.Context, *archiveJobFile, *archiveJob, func() error) error {
		count++
		return errors.New("offline")
	}
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("expected offline")
	}
	now = now.Add(11*time.Hour + 59*time.Minute)
	if err := f.runner.retry(false); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("retried before 12 hours")
	}
	now = now.Add(time.Minute)
	if err := f.runner.retry(false); err == nil || count != 2 {
		t.Fatalf("due retry count=%d err=%v", count, err)
	}
	if err := f.runner.retry(true); err == nil || count != 3 {
		t.Fatalf("startup retry count=%d err=%v", count, err)
	}
}

func TestRecoveryAfterAcknowledgementOnlyCleansUp(t *testing.T) {
	f := newJobFixture(t, "http://unreachable-report.test")
	a := &f.job.Archives[0]
	if err := f.runner.prepareArchive(f.job, a); err != nil {
		t.Fatal(err)
	}
	a.Stage = "uploaded"
	f.job.ThumbnailsReady = true
	f.job.Reports[0].Reported = true
	if err := f.store.save(f.job); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.runner.opts.Report.APIKey = ""
	f.runner.upload = func(context.Context, *archiveJobFile, *archiveJob, func() error) error {
		t.Fatal("upload during cleanup")
		return nil
	}
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, a.Path)
	assertRemoved(t, f.job.Sources[0].Path)
}

func TestRecoveryAfterMoveDoesNotRecompress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer server.Close()
	f := newJobFixture(t, server.URL)
	a := &f.job.Archives[0]
	if err := f.runner.prepareArchive(f.job, a); err != nil {
		t.Fatal(err)
	}
	a.Stage = "moving" // Simulate interruption after rename but before save("ready").
	f.job.ThumbnailsReady = true
	if err := f.store.save(f.job); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, a.Path)
}

func TestConflictDoesNotAuthorizeArchiveDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer server.Close()
	f := newJobFixture(t, server.URL)
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("409 is not confirmation of archived status")
	}
	assertExists(t, f.job.Archives[0].Path)
	assertExists(t, f.job.Sources[0].Path)
	if f.job.Reports[0].Reported {
		t.Fatal("conflict marked as acknowledged")
	}
}

func TestSplitArchivesAllUploadBeforeAnyReport(t *testing.T) {
	var uploaded atomic.Int32
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uploaded.Load() != 2 {
			t.Error("reported before both parts uploaded")
		}
		reports.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	f := newJobFixture(t, server.URL, "ABC-123-A.mp4", "ABC-123-B.mp4")
	f.opts.Archive.Split = true
	job, err := planUploadJob(f.job.SourcePath, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
	f.runner = newJobRunner(f.store, f.opts)
	f.runner.upload = func(ctx context.Context, a *archiveJobFile, j *archiveJob, save func() error) error {
		uploaded.Add(1)
		return successfulTestUpload(ctx, a, j, save)
	}
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	if reports.Load() != 1 {
		t.Fatalf("reports=%d", reports.Load())
	}
	for _, a := range f.job.Archives {
		assertRemoved(t, a.Path)
	}
}

func TestNoCatalogNumberAndMissingKeyKeepArchives(t *testing.T) {
	for _, name := range []string{"video.mp4", "ABC-123.mp4"} {
		t.Run(name, func(t *testing.T) {
			f := newJobFixture(t, "http://report.test", name)
			f.runner.opts.Report.APIKey = ""
			f.runner.upload = successfulTestUpload
			if err := f.runner.attempt(context.Background(), f.job); err == nil {
				t.Fatal("expected retained archive")
			}
			assertExists(t, f.job.Archives[0].Path)
			assertExists(t, f.job.Sources[0].Path)
		})
	}
}

func TestModifiedArchiveIsNeverDeleted(t *testing.T) {
	f := newJobFixture(t, "http://report.test")
	a := &f.job.Archives[0]
	if err := f.runner.prepareArchive(f.job, a); err != nil {
		t.Fatal(err)
	}
	a.Stage = "uploaded"
	f.job.Reports[0].Reported = true
	f.job.ThumbnailsReady = true
	mustWriteFile(t, a.Path, "different file now occupying the path")
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("expected changed archive rejection")
	}
	assertExists(t, a.Path)
	assertExists(t, f.job.Sources[0].Path)
}

func TestSourceCleanupKeepsUnprocessedFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer server.Close()
	f := newJobFixture(t, server.URL)
	note := filepath.Join(f.job.SourceDir, "keep.txt")
	mustWriteFile(t, note, "unrelated data")
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	assertExists(t, note)
}

func TestJobStoreExclusiveLockAndDurableReload(t *testing.T) {
	f := newJobFixture(t, "http://report.test")
	other, err := openJobStore(f.store.dir)
	if err == nil {
		other.close()
		t.Fatal("second writer acquired the lock")
	}
	want := *f.job
	f.restart(t)
	if !reflect.DeepEqual(want, *f.job) {
		t.Fatal("record changed after reload")
	}
}

func TestPartiallyAcknowledgedReportsResumeOnlyMissingFilm(t *testing.T) {
	counts := make(map[string]int)
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Code string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		counts[body.Code]++
		if body.Code == "XYZ-456" && fail {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(201)
		}
	}))
	defer server.Close()
	f := newJobFixture(t, server.URL, "ABC-123.mp4", "XYZ-456.mp4")
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("expected partial report failure")
	}
	assertExists(t, f.job.Archives[0].Path)
	f.restart(t)
	f.runner.upload = func(context.Context, *archiveJobFile, *archiveJob, func() error) error {
		t.Fatal("repeated upload")
		return nil
	}
	fail = false
	if err := f.runner.attempt(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	if counts["ABC-123"] != 1 || counts["XYZ-456"] != 2 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestWatchSignatureChangesWhenAnotherVideoIsAdded(t *testing.T) {
	folder := t.TempDir()
	mustWriteFile(t, filepath.Join(folder, "ABC-123.mp4"), "one")
	before, err := watchTargetSignature(folder, 1)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(folder, "XYZ-456.mp4"), "two")
	after, err := watchTargetSignature(folder, 1)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("new video was hidden by completed target cache")
	}
}

func TestMixedUnmatchedVideosDoNotPermitArchiveCleanup(t *testing.T) {
	f := newJobFixture(t, "http://unused.test", "ABC-123.mp4", "video.mp4")
	f.runner.upload = successfulTestUpload
	if err := f.runner.attempt(context.Background(), f.job); err == nil {
		t.Fatal("unmatched video should retain the archive")
	}
	assertExists(t, f.job.Archives[0].Path)
}
