package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const filterTestHash = "0123456789012345678901234567890123456789"
const otherFilterTestHash = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"

func TestPlanTorrentSizeFilter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []qbitFile
		wantIDs  string
		retained int
	}{
		{"boundary", []qbitFile{{Index: 7, Size: 100*mb - 1, Priority: 1}, {Index: 12, Size: 100 * mb, Priority: 1}}, "7", 1},
		{"size only", []qbitFile{{Index: 3, Name: "large.txt", Size: 101 * mb, Priority: 6}}, "", 1},
		{"preserve deselection", []qbitFile{{Index: 4, Size: mb, Priority: 0}, {Index: 9, Size: 200 * mb, Priority: 0}}, "", 0},
		{"empty", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, retained := planTorrentSizeFilter(tc.files, 100*mb)
			var values []string
			for _, id := range ids {
				values = append(values, strconv.Itoa(id))
			}
			if got := strings.Join(values, "|"); got != tc.wantIDs || retained != tc.retained {
				t.Fatalf("ids=%q retained=%d, want %q %d", got, retained, tc.wantIDs, tc.retained)
			}
		})
	}
}

// This fake exercises the actual HTTP client, qBittorrent indexes, versioned
// endpoints, persisted jobs, and readback ordering rather than mocking the runner.
type filterTestServer struct {
	mu             sync.Mutex
	version        string
	torrents       []qbitTorrent
	files          map[string][]qbitFile
	calls          []string
	applyExclusion bool
	loseStartReply bool
	ignoreStop     bool
	badFiles       string
	requireAuth    bool
	expireAuth     bool
	loginCount     int
	session        string
	t              *testing.T
}

func newFilterTestServer(t *testing.T, version string) (*filterTestServer, *httptest.Server) {
	f := &filterTestServer{t: t, version: version, applyExclusion: true,
		torrents: []qbitTorrent{{Hash: filterTestHash, Tags: "other, filter-small", State: "stoppedDL", AddedOn: 123}},
		files: map[string][]qbitFile{filterTestHash: {
			{Index: 7, Name: "readme.txt", Size: mb, Priority: 1},
			{Index: 2, Name: "movie.mkv", Size: 100 * mb, Priority: 6},
			{Index: 14, Name: "unselected.mp4", Size: 200 * mb, Priority: 0},
		}}}
	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	return f, server
}

func (f *filterTestServer) serveHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	endpoint := strings.TrimPrefix(req.URL.Path, "/qb/api/v2/")
	f.calls = append(f.calls, req.Method+" "+endpoint)
	if req.Header.Get("Origin") != "http://"+req.Host || req.Header.Get("Referer") != "http://"+req.Host+"/" {
		f.t.Error("missing matching Origin/Referer headers")
	}
	if endpoint == "auth/login" {
		if req.Method != http.MethodPost || req.FormValue("username") != "test-user" || req.FormValue("password") != "test-password" {
			fmt.Fprint(w, "Fails.")
			return
		}
		f.loginCount++
		f.session = fmt.Sprintf("session-%d", f.loginCount)
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: f.session, Path: "/"})
		fmt.Fprint(w, "Ok.")
		return
	}
	if f.requireAuth {
		cookie, err := req.Cookie("SID")
		if err != nil || cookie.Value != f.session || f.expireAuth {
			f.expireAuth = false
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	if endpoint == "app/version" {
		fmt.Fprint(w, f.version)
		return
	}
	if req.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if endpoint == "torrents/info" {
			items := make([]qbitTorrent, 0)
			for _, torrent := range f.torrents {
				if hash := req.URL.Query().Get("hashes"); hash == "" || hash == torrent.Hash {
					// Intentionally ignore the tag query to test local scope checks.
					items = append(items, torrent)
				}
			}
			json.NewEncoder(w).Encode(items)
			return
		}
		if endpoint == "torrents/files" {
			if f.badFiles != "" {
				fmt.Fprint(w, f.badFiles)
				return
			}
			files := f.files[req.URL.Query().Get("hash")]
			if files == nil {
				files = []qbitFile{}
			}
			json.NewEncoder(w).Encode(files)
			return
		}
	}
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		f.t.Error("mutation is not form encoded")
	}
	if endpoint == "torrents/filePrio" {
		if req.FormValue("priority") != "0" {
			f.t.Error("filter attempted to re-enable a file")
		}
		if f.applyExclusion {
			ids := strings.Split(req.FormValue("id"), "|")
			for _, value := range ids {
				index, err := strconv.Atoi(value)
				if err != nil {
					f.t.Error(err)
				}
				for i := range f.files[req.FormValue("hash")] {
					if f.files[req.FormValue("hash")][i].Index == index {
						f.files[req.FormValue("hash")][i].Priority = 0
					}
				}
			}
		}
		return
	}
	if endpoint == "torrents/stop" || endpoint == "torrents/pause" || endpoint == "torrents/start" || endpoint == "torrents/resume" {
		hash := req.FormValue("hashes")
		if hash == "all" || hash == "" {
			f.t.Error("mutation does not target exactly one torrent")
		}
		for i := range f.torrents {
			if f.torrents[i].Hash != hash {
				continue
			}
			if endpoint == "torrents/start" || endpoint == "torrents/resume" {
				for _, file := range f.files[hash] {
					if file.Size < 100*mb && file.Priority != 0 {
						f.t.Error("torrent started before exclusion took effect")
					}
				}
				f.torrents[i].State = "stalledDL"
				if f.loseStartReply {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			} else if !f.ignoreStop {
				f.torrents[i].State = "stoppedDL"
				if strings.HasPrefix(f.version, "v4.") {
					f.torrents[i].State = "pausedDL"
				}
			}
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func newFilterTestRunner(t *testing.T, server *httptest.Server, stateDir string) *torrentFilterRunner {
	t.Helper()
	opts := torrentFilterOptions{BaseURL: server.URL + "/qb", Tag: "filter-small", MinSizeBytes: 100 * mb,
		RequestTimeout: time.Second, RetryInterval: time.Second, StateDir: stateDir}
	client, err := newQbitClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openTorrentFilterStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.close)
	return &torrentFilterRunner{client: client, store: store, opts: opts, now: time.Now}
}

func countFilterCalls(f *filterTestServer, call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, value := range f.calls {
		if value == call {
			count++
		}
	}
	return count
}

func TestTorrentFilterWaitsForMetadataAndUsesFileIndexes(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	original := f.files[filterTestHash]
	f.files[filterTestHash] = nil
	f.torrents[0].State = "metaDL"
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.store.jobs[filterTestHash].Stage != "waiting" || countFilterCalls(f, "POST torrents/stop") != 0 {
		t.Fatal("metadata fetch was interrupted")
	}
	f.mu.Lock()
	f.files[filterTestHash], f.torrents[0].State = original, "stoppedDL"
	f.mu.Unlock()
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runner.store.jobs[filterTestHash].Stage; got != "done" {
		t.Fatalf("stage=%s", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	files := f.files[filterTestHash]
	if files[0].Priority != 0 || files[1].Priority != 6 || files[2].Priority != 0 {
		t.Fatalf("wrong selection after filtering: %#v", files)
	}
}

func TestTorrentFilterSupportsQbit4And5AndStopsBeforeMutation(t *testing.T) {
	for _, tc := range []struct{ version, stop, start string }{
		{"v4.5.5", "pause", "resume"}, {"v5.0.4", "stop", "start"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			f, server := newFilterTestServer(t, tc.version)
			f.torrents[0].State = "downloading"
			runner := newFilterTestRunner(t, server, t.TempDir())
			if err := runner.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if countFilterCalls(f, "POST torrents/"+tc.stop) != 1 || countFilterCalls(f, "POST torrents/"+tc.start) != 1 {
				t.Fatal("wrong versioned endpoints")
			}
		})
	}
}

func TestTorrentFilterReadbackFailureDoesNotStartAndRetrySurvivesRestart(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.applyExclusion = false
	dir := t.TempDir()
	runner := newFilterTestRunner(t, server, dir)
	if err := runner.poll(context.Background()); err == nil || !strings.Contains(err.Error(), "核验失败") {
		t.Fatalf("want readback failure, got %v", err)
	}
	if countFilterCalls(f, "POST torrents/start") != 0 {
		t.Fatal("started despite failed readback")
	}
	runner.store.close()
	f.mu.Lock()
	f.applyExclusion = true
	f.mu.Unlock()
	restarted := newFilterTestRunner(t, server, dir)
	restarted.now = func() time.Time { return time.Now().Add(2 * time.Second) }
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.store.jobs[filterTestHash].Stage != "done" || countFilterCalls(f, "POST torrents/start") != 1 {
		t.Fatal("restart did not recover filtering")
	}
	f.mu.Lock()
	f.torrents[0].State = "stoppedDL" // User pauses after completion.
	f.mu.Unlock()
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countFilterCalls(f, "POST torrents/start") != 1 {
		t.Fatal("overrode manual pause")
	}
}

func TestTorrentFilterNoRetainedFilesStaysStopped(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.files[filterTestHash] = f.files[filterTestHash][:1]
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err == nil || !strings.Contains(err.Error(), "没有达到") {
		t.Fatalf("want no retained files, got %v", err)
	}
	if countFilterCalls(f, "POST torrents/start") != 0 {
		t.Fatal("empty selection was started")
	}
	if runner.store.jobs[filterTestHash].LastError == "" {
		t.Fatal("failure reason was not persisted")
	}
}

func TestTorrentFilterLostStartReplyIsReconciledWithoutSecondStart(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.loseStartReply = true
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err == nil {
		t.Fatal("expected lost response error")
	}
	runner.now = func() time.Time { return time.Now().Add(2 * time.Second) }
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countFilterCalls(f, "POST torrents/start") != 1 || runner.store.jobs[filterTestHash].Stage != "done" {
		t.Fatal("startup intent was replayed")
	}
}

func TestTorrentFilterPendingAndUntaggedTasksDoNotBlockOtherTasks(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.torrents = append([]qbitTorrent{{Hash: otherFilterTestHash, Tags: "filter-small", State: "metaDL", AddedOn: 234}}, f.torrents...)
	f.torrents = append(f.torrents, qbitTorrent{Hash: strings.Repeat("a", 40), Tags: "filter-small-other", State: "stoppedDL", AddedOn: 345})
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.store.jobs) != 2 || runner.store.jobs[otherFilterTestHash].Stage != "waiting" || runner.store.jobs[filterTestHash].Stage != "done" {
		t.Fatal("unrelated/pending task affected filtering")
	}
}

func TestTorrentFilterDryRunDoesNotMutate(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	runner := newFilterTestRunner(t, server, t.TempDir())
	runner.opts.DryRun = true
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if strings.HasPrefix(call, "POST torrents/") {
			t.Fatalf("dry-run mutated qBittorrent: %s", call)
		}
	}
	if len(runner.store.jobs) != 0 {
		t.Fatal("dry-run persisted a job")
	}
}

func TestTorrentFilterInvalidFileResponsesStayStopped(t *testing.T) {
	for _, response := range []string{"null", "<html>login</html>", `[{"index":0,"name":"movie.mkv","priority":1}]`,
		`[{"index":0,"name":"a","size":1,"priority":1},{"index":0,"name":"b","size":2,"priority":1}]`} {
		t.Run(response, func(t *testing.T) {
			f, server := newFilterTestServer(t, "v5.0.4")
			f.badFiles = response
			runner := newFilterTestRunner(t, server, t.TempDir())
			if err := runner.poll(context.Background()); err == nil {
				t.Fatal("invalid file response accepted")
			}
			if countFilterCalls(f, "POST torrents/start") != 0 || countFilterCalls(f, "POST torrents/filePrio") != 0 {
				t.Fatal("invalid file response caused mutation")
			}
		})
	}
}

func TestTorrentFilterCannotMutateBeforeStoppedReadback(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.torrents[0].State, f.ignoreStop = "downloading", true
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err == nil {
		t.Fatal("missing stopped readback was accepted")
	}
	if countFilterCalls(f, "POST torrents/filePrio") != 0 || countFilterCalls(f, "POST torrents/start") != 0 {
		t.Fatal("modified running torrent")
	}
}

func TestQbitClientReauthenticatesExpiredSession(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.requireAuth = true
	runner := newFilterTestRunner(t, server, t.TempDir())
	runner.client.username, runner.client.password = "test-user", "test-password"
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expireAuth = true
	f.mu.Unlock()
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loginCount != 2 {
		t.Fatalf("login count=%d, want 2", f.loginCount)
	}
}

func TestTorrentFilterRestartFromVerifiedAndAmbiguousStart(t *testing.T) {
	for _, stage := range []string{"verified", "start-requested"} {
		t.Run(stage, func(t *testing.T) {
			f, server := newFilterTestServer(t, "v5.0.4")
			runner := newFilterTestRunner(t, server, t.TempDir())
			before := append([]qbitFile(nil), f.files[filterTestHash]...)
			f.files[filterTestHash][0].Priority = 0
			job := torrentFilterJob{Version: 1, Hash: filterTestHash, AddedOn: 123, Server: runner.opts.BaseURL,
				Tag: runner.opts.Tag, Stage: stage, MinSizeBytes: 100 * mb, Files: before}
			if err := runner.store.save(&job); err != nil {
				t.Fatal(err)
			}
			err := runner.poll(context.Background())
			if stage == "verified" {
				if err != nil || countFilterCalls(f, "POST torrents/start") != 1 {
					t.Fatalf("verified job not resumed: %v", err)
				}
			} else if err == nil || countFilterCalls(f, "POST torrents/start") != 0 {
				t.Fatalf("ambiguous start overridden: %v", err)
			}
		})
	}
}

func TestTorrentFilterReaddedHashGetsNewJob(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	runner := newFilterTestRunner(t, server, t.TempDir())
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.torrents[0].AddedOn, f.torrents[0].State = 456, "stoppedDL"
	f.files[filterTestHash][0].Priority = 1
	f.mu.Unlock()
	if err := runner.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countFilterCalls(f, "POST torrents/start") != 2 || runner.store.jobs[filterTestHash].AddedOn != 456 {
		t.Fatal("readded torrent was mistaken for a completed old job")
	}
}

func TestTorrentFilterStoreIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "filter-state")
	store, err := openTorrentFilterStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if second, err := openTorrentFilterStore(dir); err == nil {
		second.close()
		t.Fatal("two workers acquired the same state queue")
	}
}

func TestQbitClientDoesNotAcceptFailedLoginOrExposePassword(t *testing.T) {
	f, server := newFilterTestServer(t, "v5.0.4")
	f.requireAuth = true
	runner := newFilterTestRunner(t, server, t.TempDir())
	runner.client.username, runner.client.password = "test-user", "wrong-secret"
	if err := runner.poll(context.Background()); err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("invalid login error: %v", err)
	}
	if countFilterCalls(f, "POST torrents/filePrio") != 0 {
		t.Fatal("failed login caused mutation")
	}
}

func TestVerifyTorrentSizeFilterDetectsChangedOrDeselectedFiles(t *testing.T) {
	expected := []qbitFile{{Index: 4, Name: "video.mkv", Size: 100 * mb, Priority: 1}}
	for _, actual := range [][]qbitFile{nil, {{Index: 4, Name: "changed.mkv", Size: 100 * mb, Priority: 1}},
		{{Index: 4, Name: "video.mkv", Size: 100 * mb, Priority: 0}}, {{Index: 0, Name: "video.mkv", Size: 100 * mb, Priority: 1}}} {
		if _, err := verifyTorrentSizeFilter(expected, actual, 100*mb); err == nil {
			t.Fatalf("changed files passed verification: %#v", actual)
		}
	}
}

func TestQbitClientRejectsNonJSONContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "[]")
	}))
	defer server.Close()
	client, err := newQbitClient(torrentFilterOptions{BaseURL: server.URL, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.torrents(context.Background(), url.Values{}); err == nil {
		t.Fatal("HTML response was accepted as an empty queue")
	}
}
