package cmd

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func startWatchForTest(t *testing.T, cfg appConfig, args ...string) (func(), <-chan struct{}) {
	t.Helper()
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_URL", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_USERNAME", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_PASSWORD", "")
	oldConfig, oldOutput, oldLogger := config, runtimeLogOutput, log.Writer()
	t.Cleanup(func() {
		config, runtimeLogOutput = oldConfig, oldOutput
		log.SetOutput(oldLogger)
	})
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	command := newRootCmd()
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(append([]string{"watch", "--config", path, "--thumbnail=false", "--report=false"}, args...))
	ctx, cancel := context.WithCancel(context.Background())
	command.SetContext(ctx)
	completed := make(chan struct{})
	var runErr error
	go func() {
		runErr = command.Execute()
		close(completed)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-completed:
				if runErr != nil {
					t.Errorf("watch command failed: %v", runErr)
				}
			case <-time.After(3 * time.Second):
				t.Error("watch and its filter worker did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop, completed
}

func createWatchTestVideo(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "movie.mkv")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(mb); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWatchFiltersWhileWaitingForStableVideo(t *testing.T) {
	fake, server := newFilterTestServer(t, "v5.1.3")
	dir, stateDir, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	video := createWatchTestVideo(t, dir)
	stop, _ := startWatchForTest(t, appConfig{
		MinSizeMB: 1, Log: logConfig{Path: logDir},
		Watch: watchConfig{Dirs: []string{dir}, StableDelay: "5h", PollInterval: "5h"},
		TorrentFilter: torrentFilterConfig{Enabled: true, BaseURL: server.URL + "/qb", StateDir: stateDir,
			PollInterval: "10ms", RetryInterval: "10ms", RequestTimeout: "1s"},
	})
	deadline := time.Now().Add(3 * time.Second)
	filtered := false
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(stateDir, filterTestHash+".json"))
		var job torrentFilterJob
		if json.Unmarshal(data, &job) == nil && job.Stage == "done" {
			filtered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !filtered || countFilterCalls(fake, "POST torrents/start") != 1 {
		t.Fatal("waiting for a stable watched video blocked torrent filtering")
	}
	stop()
	if _, err := os.Stat(video); err != nil {
		t.Fatalf("watch touched the unstable source: %v", err)
	}
	store, err := openTorrentFilterStore(stateDir)
	if err != nil {
		t.Fatalf("filter worker did not release its state lock: %v", err)
	}
	defer store.close()
	if job := store.jobs[filterTestHash]; job.Stage != "done" {
		t.Fatalf("shared watch did not persist the filtered task: stage=%q", job.Stage)
	}
	if got := store.jobs[filterTestHash].Files[1].Priority; got != 6 {
		t.Fatalf("original selection was not preserved: priority=%d", got)
	}
}

func TestFilterAPIOutageDoesNotStopWatchProcessing(t *testing.T) {
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dir, logDir := t.TempDir(), t.TempDir()
	stop, completed := startWatchForTest(t, appConfig{
		MinSizeMB: 1, Log: logConfig{Path: logDir},
		Watch: watchConfig{Dirs: []string{dir}, StableDelay: "10ms", PollInterval: "10ms"},
		TorrentFilter: torrentFilterConfig{Enabled: true, BaseURL: server.URL, StateDir: t.TempDir(),
			PollInterval: "10ms", RequestTimeout: "1s"},
	}, "--dry-run")
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("filter did not contact qBittorrent")
	}
	createWatchTestVideo(t, dir)
	deadline := time.Now().Add(3 * time.Second)
	processed := false
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(logDir, "*.log"))
		for _, path := range paths {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "发现稳定视频，开始处理:") {
				processed = true
			}
		}
		if processed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !processed {
		t.Fatal("qBittorrent API outage prevented the watcher from processing a stable video")
	}
	select {
	case <-completed:
		t.Fatal("qBittorrent API outage stopped the unified watch command")
	default:
	}
	stop()
}

func TestWatchFilterRequiresConfigurationOnlyWhenEnabled(t *testing.T) {
	stop, err := startWatchTorrentFilter(context.Background(), torrentFilterConfig{}, "", false)
	if err != nil {
		t.Fatalf("legacy watch config unexpectedly requires qBittorrent: %v", err)
	}
	stop()
	if _, err := startWatchTorrentFilter(context.Background(), torrentFilterConfig{Enabled: true}, "", false); err == nil {
		t.Fatal("enabled filtering silently accepted a missing qBittorrent URL")
	}
}
