package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRescueRSSValidationAndRanking(t *testing.T) {
	feed := `<rss xmlns:n="https://nyaa.si/xmlns/nyaa"><channel><item><title>ABP-123 1080p</title><n:infoHash>` + otherFilterTestHash + `</n:infoHash><n:seeders>12</n:seeders><n:downloads>3000</n:downloads><n:trusted>Yes</n:trusted><n:size>5.6 GiB</n:size></item><item><title>ABP-123 duplicate</title><n:infoHash>` + otherFilterTestHash + `</n:infoHash></item><item><title>ABP-1230</title><n:infoHash>` + filterTestHash + `</n:infoHash></item><item><title>ABP-123</title><n:infoHash>` + filterTestHash + `</n:infoHash><n:seeders>-1</n:seeders></item></channel></rss>`
	results, err := parseRescueRSS([]byte(feed), "ABP-123", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Hash != otherFilterTestHash || results[0].Seeders == nil || *results[0].Seeders != 12 || results[1].Seeders != nil {
		t.Fatalf("bad RSS results: %+v", results)
	}
	for _, invalid := range []string{`<html><body>captcha</body></html>`, `<rss/>`, `<rss><channel><item><title>ABP-123</title><infoHash>invalid</infoHash></item></channel></rss>`} {
		if _, err := parseRescueRSS([]byte(invalid), "ABP-123", time.Now()); err == nil {
			t.Fatalf("accepted invalid feed: %s", invalid)
		}
	}
	if empty, err := parseRescueRSS([]byte(`<rss><channel/></rss>`), "ABP-123", time.Now()); err != nil || len(empty) != 0 {
		t.Fatal("valid empty RSS was treated as an error")
	}
}

func TestRescueSearchUsesExplicitProxyAndRejectsHTML(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() || r.URL.Host != "indexer.invalid" || r.URL.Query().Get("q") != "ABP-123" {
			t.Error("search did not use explicit proxy")
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<rss><channel/></rss>`)
	}))
	defer proxy.Close()
	s := newTorrentSearcher(torrentRescueOptions{ProxyURL: proxy.URL, RSSURL: "http://indexer.invalid/", SearchTimeout: time.Second})
	if _, err := s.search(context.Background(), "ABP-123"); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html>challenge</html>`)
	}))
	defer bad.Close()
	s = newTorrentSearcher(torrentRescueOptions{RSSURL: bad.URL, SearchTimeout: time.Second})
	if _, err := s.search(context.Background(), "ABP-123"); err == nil {
		t.Fatal("challenge page treated as empty search")
	}
}

func TestRescueObservationKeepsFreshDownloadEvidence(t *testing.T) {
	now := time.Now()
	torrent := qbitTorrent{Hash: filterTestHash, AddedOn: now.Add(-20 * 24 * time.Hour).Unix(), Completed: 100, Size: 1000, Progress: 0.1, State: "stalledDL"}
	old := rescueObservation{AddedOn: torrent.AddedOn, Completed: 100, LastProgress: now.Add(-25 * time.Hour), ObservedAt: now.Add(-time.Minute)}
	observation := observeRescue(old, torrent, now, 30*time.Minute)
	if !observation.LastProgress.Equal(old.LastProgress) {
		t.Fatal("unchanged completed bytes reset download evidence")
	}
	for _, kind := range []string{"first", "gap", "new instance", "progress", "recheck"} {
		t.Run(kind, func(t *testing.T) {
			previous, updated := old, torrent
			switch kind {
			case "first":
				previous = rescueObservation{}
			case "gap":
				previous.ObservedAt = now.Add(-2 * time.Hour)
			case "new instance":
				updated.AddedOn++
			case "progress":
				updated.Completed++
			case "recheck":
				updated.Completed--
			}
			if !observeRescue(previous, updated, now, 30*time.Minute).LastProgress.Equal(now) {
				t.Fatal("uncertain continuity did not refresh runtime download evidence")
			}
		})
	}
}

func TestRescueEligibilityUsesAddedTimeWithoutObservationWait(t *testing.T) {
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	opts := torrentRescueOptions{MinAge: 10 * 24 * time.Hour}
	torrent := qbitTorrent{Hash: filterTestHash, AddedOn: now.Add(-20 * 24 * time.Hour).Unix(), Size: 1000, State: "stalledDL"}
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{{"old task on first poll", 20 * 24 * time.Hour, true}, {"age boundary", opts.MinAge, true}, {"too young", opts.MinAge - time.Second, false}, {"future added time", -time.Hour, false}} {
		t.Run(tc.name, func(t *testing.T) {
			current := torrent
			current.AddedOn = now.Add(-tc.age).Unix()
			if rescueEligible(current, now, opts) != tc.want {
				t.Fatalf("age %s admission differs from added time", tc.age)
			}
		})
	}
	for _, state := range []string{"pausedDL", "stoppedDL", "queuedDL", "error", "checkingDL", "downloading"} {
		current := torrent
		current.State = state
		if rescueEligible(current, now, opts) {
			t.Fatalf("state %s should not trigger replacement", state)
		}
	}
	for _, change := range []func(*qbitTorrent){func(t *qbitTorrent) { t.AddedOn = 0 }, func(t *qbitTorrent) { t.Progress = 1 }, func(t *qbitTorrent) { t.Size = 0 }, func(t *qbitTorrent) { t.Hash = "invalid" }} {
		current := torrent
		change(&current)
		if rescueEligible(current, now, opts) {
			t.Fatal("invalid or complete task was admitted")
		}
	}
}

type rescueTestAPI struct {
	fake         *filterTestServer
	loseAdd      bool
	addCalls     int
	network      string
	hook         bool
	deleteCalls  []rescueTestDelete
	loseDelete   bool
	ignoreDelete bool
	rejectDelete bool
}

type rescueTestDelete struct {
	Hash        string
	DeleteFiles bool
}

func newRescueTestRunner(t *testing.T) (*torrentRescueRunner, *rescueTestAPI, *time.Time) {
	t.Helper()
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_URL", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_USERNAME", "")
	t.Setenv("QBIT_UPLOAD_QBITTORRENT_PASSWORD", "")
	t.Setenv("QBIT_UPLOAD_SEARCH_PROXY", "")
	f, _ := newFilterTestServer(t, "v5.1.3")
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	f.torrents = []qbitTorrent{{Hash: filterTestHash, Name: "ABP-123", AddedOn: now.Add(-20 * 24 * time.Hour).Unix(), State: "stalledDL", Size: 200 * mb, Completed: 100 * mb, Progress: 0.5, SavePath: "/downloads/original"}}
	f.files[filterTestHash] = []qbitFile{{Index: 7, Name: "ABP-123.mp4", Size: 200 * mb, Priority: 1, Progress: 0.5}}
	api := &rescueTestAPI{fake: f, network: "connected", hook: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		endpoint := strings.TrimPrefix(req.URL.Path, "/qb/api/v2/")
		if endpoint == "torrents/delete" {
			f.mu.Lock()
			defer f.mu.Unlock()
			hash := req.FormValue("hashes")
			flag := req.FormValue("deleteFiles")
			if req.Method != http.MethodPost || !validTorrentHash(hash) || (flag != "true" && flag != "false") {
				t.Error("deletion not scoped to a single hash with an explicit file policy")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			api.deleteCalls = append(api.deleteCalls, rescueTestDelete{Hash: hash, DeleteFiles: flag == "true"})
			if api.rejectDelete {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if !api.ignoreDelete {
				for i, torrent := range f.torrents {
					if torrent.Hash == hash {
						f.torrents = append(f.torrents[:i], f.torrents[i+1:]...)
						break
					}
				}
				delete(f.files, hash)
			}
			if api.loseDelete {
				api.loseDelete = false
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		if endpoint == "torrents/categories" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"qbit-rescue":{}}`)
			return
		}
		if endpoint == "torrents/createTags" {
			return
		}
		if endpoint == "torrents/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `["qbit-rescue"]`)
			return
		}
		if endpoint == "app/preferences" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"autorun_enabled": api.hook, "autorun_program": "/config/qbit-rescue-hook.py %F %N %I"})
			return
		}
		if endpoint == "transfer/info" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"connection_status": api.network})
			return
		}
		if endpoint == "torrents/add" {
			f.mu.Lock()
			defer f.mu.Unlock()
			api.addCalls++
			if req.FormValue("stopCondition") != "MetadataReceived" || req.FormValue("tags") != rescueTag || req.FormValue("autoTMM") != "false" {
				t.Error("candidate not isolated or not stopped after metadata")
			}
			hash := strings.TrimPrefix(req.FormValue("urls"), "magnet:?xt=urn:btih:")
			f.torrents = append(f.torrents, qbitTorrent{Hash: hash, Name: "ABP-123", AddedOn: now.Unix(), State: "stoppedDL", Size: 200 * mb, SavePath: req.FormValue("savepath"), Tags: rescueTag})
			f.files[hash] = []qbitFile{{Index: 12, Name: "ABP-123.mp4", Size: 200 * mb, Priority: 1}, {Index: 8, Name: "advertisement.url", Size: 2 * mb, Priority: 1}}
			if api.loseAdd {
				api.loseAdd = false
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, "Ok.")
			return
		}
		f.serveHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	cfg := appConfig{TorrentFilter: torrentFilterConfig{BaseURL: server.URL + "/qb"}, TorrentRescue: torrentRescueConfig{StateDir: t.TempDir(), GuardDir: t.TempDir(), DownloadHostDir: t.TempDir(), DownloadPrefix: "/downloads", CandidatePath: "/downloads/rescue", PublishDir: filepath.Join(t.TempDir(), "incoming")}}
	opts, err := resolveTorrentRescueOptions(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	opts.MinFree = 0
	r, err := newTorrentRescueRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.store.close)
	r.now = func() time.Time { return now }
	r.diskFree = func(string) (uint64, error) { return 1 << 40, nil }
	r.verify = func(context.Context, []string) error { return nil }
	r.search = func(context.Context, string) ([]rescueCandidate, error) {
		return []rescueCandidate{{Hash: otherFilterTestHash, Title: "ABP-123", Magnet: "magnet:?xt=urn:btih:" + otherFilterTestHash, Score: 70, Size: 200 * mb}}, nil
	}
	r.store.state.Observations[filterTestHash] = rescueObservation{AddedOn: f.torrents[0].AddedOn, Completed: 100 * mb, LastProgress: now.Add(-25 * time.Hour), ObservedAt: now.Add(-time.Minute)}
	return r, api, &now
}

func TestRescueLostAddAndStartRepliesReadBackOnce(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	api.loseAdd = true
	if err := r.poll(context.Background()); err == nil {
		t.Fatal("lost add reply should be reported")
	}
	*now = now.Add(time.Minute)
	api.fake.loseStartReply = true
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	if api.addCalls != 1 || g.Downloads[otherFilterTestHash].Stage != "running" {
		t.Fatalf("adding was duplicated or start readback failed: %+v", g)
	}
	if got := countFilterCalls(api.fake, "POST torrents/start"); got != 1 {
		t.Fatalf("start calls=%d", got)
	}
	*now = now.Add(time.Minute)
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countFilterCalls(api.fake, "POST torrents/start") != 1 {
		t.Fatal("repeated an acknowledged start")
	}
}

func TestRescueDryRunAndHookPrecondition(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	r.opts.DryRun = true
	r.store.dryRun = true
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 0 || countFilterCalls(api.fake, "POST torrents/start") != 0 {
		t.Fatal("dry-run mutated qBittorrent")
	}
	entries, _ := os.ReadDir(r.opts.StateDir)
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("dry-run wrote state")
	}
	entries, _ = os.ReadDir(r.opts.GuardDir)
	if len(entries) != 0 {
		t.Fatal("dry-run wrote guards")
	}
	r.opts.DryRun = false
	r.store.dryRun = false
	api.hook = false
	if err := r.poll(context.Background()); err == nil {
		t.Fatal("managed candidates without completion gate")
	}
	if api.addCalls != 0 {
		t.Fatal("hook failure still added candidates")
	}
}

func TestRescueRespectsManualPauseAndIncarnation(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	api.fake.mu.Lock()
	api.fake.torrents[1].State = "stoppedDL"
	api.fake.mu.Unlock()
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	d := r.store.state.Groups["ABP-123"].Downloads[otherFilterTestHash]
	if d.Stage != "manual-stopped" {
		t.Fatal("manual pause not respected")
	}
	api.fake.mu.Lock()
	api.fake.torrents[1].AddedOn++
	api.fake.mu.Unlock()
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if d.Stage != "removed" || countFilterCalls(api.fake, "POST torrents/start") != 1 {
		t.Fatal("a readded instance was managed or resumed")
	}
}

func TestRescueScoresActuallyRetirePoorTrials(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 28; i++ {
		*now = now.Add(15 * time.Minute)
		if err := r.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	d := r.store.state.Groups["ABP-123"].Downloads[otherFilterTestHash]
	if d.Stage != "failed" || d.RuntimeScore >= r.opts.MinRuntimeScore || !d.CooldownUntil.After(*now) {
		t.Fatalf("poor trial not retired: %+v", d)
	}
	if countFilterCalls(api.fake, "POST torrents/stop") != 1 {
		t.Fatal("retired trial was not stopped")
	}
}

func TestRescueGlobalAndPerCodeLimits(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	r.opts.MaxPerCode = 1
	r.opts.MaxActive = 1
	r.search = func(context.Context, string) ([]rescueCandidate, error) {
		return []rescueCandidate{{Hash: otherFilterTestHash, Magnet: "magnet:?xt=urn:btih:" + otherFilterTestHash, Score: 70, Size: 200 * mb}, {Hash: strings.Repeat("1", 40), Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("1", 40), Score: 60, Size: 200 * mb}}, nil
	}
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 1 {
		t.Fatalf("concurrency not bounded: %d", api.addCalls)
	}
}

func TestRescuePublicationCrashRecoveryAndNoDuplicateAfterConsumption(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	ctx := context.Background()
	r.opts.MinVideoSize = 1
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	d := g.Downloads[filterTestHash]
	api.fake.mu.Lock()
	api.fake.torrents[0].Progress = 1
	api.fake.torrents[0].AmountLeft = 0
	api.fake.torrents[0].State = "uploading"
	api.fake.files[filterTestHash] = []qbitFile{{Index: 7, Name: "ABP-123.mp4", Size: 4, Priority: 1, Progress: 1}}
	original := api.fake.torrents[0]
	api.fake.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(r.opts.DownloadHostDir, "original"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.opts.DownloadHostDir, "original", "ABP-123.mp4"), []byte("film"), 0644); err != nil {
		t.Fatal(err)
	}
	d.Files = []qbitFile{{Name: "ABP-123.mp4", Size: 4, Priority: 1, Progress: 1}}
	g.Winner = filterTestHash
	g.Stage = "publishing"
	g.PublishPath = filepath.Join(r.opts.PublishDir, "rescue-ABP-123-"+filterTestHash[:12])
	if err := r.store.save(); err != nil {
		t.Fatal(err)
	}
	// Simulate crash after rename but before the published state was committed.
	if err := os.MkdirAll(g.PublishPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeRescueJSON(filepath.Join(g.PublishPath, ".qbit-rescue.json"), rescueManifest{Code: g.Code, Hash: g.Winner}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.PublishPath, "ABP-123.mp4"), []byte("film"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := torrentRescueConfig{Enabled: true, StateDir: r.opts.StateDir}
	if ready, err := rescuePublicationReady(g.PublishPath, cfg); err != nil || ready {
		t.Fatal("watcher accepted an uncommitted publication")
	}
	if err := r.publishWinner(ctx, g, map[string]qbitTorrent{filterTestHash: original}); err != nil {
		t.Fatal(err)
	}
	if ready, err := rescuePublicationReady(g.PublishPath, cfg); err != nil || !ready {
		t.Fatal("committed publication not accepted")
	}
	if err := os.RemoveAll(g.PublishPath); err != nil {
		t.Fatal(err)
	}
	r.store.close()
	restarted, err := newTorrentRescueRunner(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.store.close()
	restarted.now = r.now
	restarted.search = r.search
	restarted.verify = r.verify
	if err := restarted.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.PublishPath); !os.IsNotExist(err) {
		t.Fatal("consumed publication was copied a second time")
	}
}

func TestRescueRejectsTraversalWrongMovieAndSymlink(t *testing.T) {
	r, _, _ := newRescueTestRunner(t)
	for _, name := range []string{"../../outside.mp4", "/outside.mp4", `..\outside.mp4`} {
		if _, err := r.localDownloadPath("/downloads/base", name); err == nil {
			t.Fatalf("accepted path %q", name)
		}
	}
	if _, err := selectRescueVideos([]qbitFile{{Name: "ABP-124.mp4", Size: 200 * mb, Priority: 1}}, "ABP-123", 100*mb); err == nil {
		t.Fatal("accepted wrong movie")
	}
	outside := filepath.Join(t.TempDir(), "other.mp4")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.opts.DownloadHostDir, "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	if _, err := regularRescueFile(link, r.opts.DownloadHostDir); err == nil {
		t.Fatal("accepted escaped symlink")
	}
}

func TestRescueConfigValidationAndDefaults(t *testing.T) {
	t.Setenv("QBIT_UPLOAD_SEARCH_PROXY", "")
	cfg := appConfig{}
	opts, err := resolveTorrentRescueOptions(cfg, true)
	if err != nil || opts.MaxActive != 6 || opts.MinAge != 240*time.Hour || opts.MinRuntimeScore != 40 || opts.PreferredVideoSize != 4<<30 || opts.IdealVideoSize != 20<<30 {
		t.Fatalf("invalid defaults: %+v %v", opts, err)
	}
	for _, edit := range []func(*torrentRescueConfig){func(c *torrentRescueConfig) { c.NoProgress = "-1h" }, func(c *torrentRescueConfig) { c.ProxyURL = "http://user:secret@example.com" }, func(c *torrentRescueConfig) { c.MaxActive = -1 }, func(c *torrentRescueConfig) { c.RSSURL = "https://example.com/?token=secret" }, func(c *torrentRescueConfig) { c.MinRuntimeScore = 101 }, func(c *torrentRescueConfig) { c.PreferredVideoGB = -1 }, func(c *torrentRescueConfig) { c.IdealVideoGB = 2 }} {
		current := cfg
		edit(&current.TorrentRescue)
		if _, err := resolveTorrentRescueOptions(current, true); err == nil {
			t.Fatal("accepted invalid rescue config")
		}
	}
}

func TestRescueStorageBudgetAndSourceFailure(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	r.opts.StorageBudget = 1
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 0 {
		t.Fatal("storage budget did not bound trial additions")
	}
	r.opts.StorageBudget = 200 << 30
	diskFree := r.diskFree
	r.diskFree = func(string) (uint64, error) { return 0, nil }
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 0 {
		t.Fatal("insufficient disk space did not bound trial additions")
	}
	r.diskFree = diskFree
	r.search = func(context.Context, string) ([]rescueCandidate, error) { return nil, fmt.Errorf("source unavailable") }
	r.store.state.Groups["ABP-123"].NextSearch = time.Time{}
	if err := r.poll(context.Background()); err == nil {
		t.Fatal("source failure treated as an empty success")
	}
	if api.addCalls != 0 {
		t.Fatal("source failure started a queued trial")
	}
}

func TestRescueFirstObservationAndLegacyCopyGate(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	r.store.state.Observations = map[string]rescueObservation{}
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 1 {
		t.Fatal("an old stalled task did not start rescue on its first observation")
	}
	legacy := filepath.Join(t.TempDir(), "ABP-123.mp4")
	if err := os.WriteFile(legacy, []byte("copy"), 0644); err != nil {
		t.Fatal(err)
	}
	if ready, err := rescuePublicationReady(legacy, torrentRescueConfig{Enabled: true, StateDir: r.opts.StateDir}); err != nil || ready {
		t.Fatal("legacy copy raced the managed publication")
	}
	unmanaged := filepath.Join(t.TempDir(), "ABP-124.mp4")
	if err := os.WriteFile(unmanaged, []byte("copy"), 0644); err != nil {
		t.Fatal(err)
	}
	if ready, err := rescuePublicationReady(unmanaged, torrentRescueConfig{Enabled: true, StateDir: r.opts.StateDir}); err != nil || !ready {
		t.Fatal("unmanaged normal file was blocked")
	}
}

func TestRescueSearchesOldestAddedTaskFirst(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	r.opts.SearchBatch = 1
	older := strings.Repeat("1", 40)
	api.fake.mu.Lock()
	api.fake.torrents = append(api.fake.torrents, qbitTorrent{Hash: older, Name: "ZZZ-789", AddedOn: now.Add(-21 * 24 * time.Hour).Unix(), State: "stalledDL", Size: 200 * mb, SavePath: "/downloads/older"})
	api.fake.files[older] = []qbitFile{{Index: 0, Name: "ZZZ-789.mp4", Size: 200 * mb, Priority: 1}}
	api.fake.mu.Unlock()
	var searched []string
	r.search = func(_ context.Context, code string) ([]rescueCandidate, error) {
		searched = append(searched, code)
		return nil, nil
	}
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 || searched[0] != "ZZZ-789" || len(r.store.state.Groups) != 2 {
		t.Fatalf("first search did not follow added time: %v", searched)
	}
}

func TestRescueTitleLabelsDoNotRestrictCandidates(t *testing.T) {
	for _, name := range []string{"ABP-123-C", "ABP-123-1080p", "ABP-123-4K UNCENSORED"} {
		t.Run(name, func(t *testing.T) {
			r, api, _ := newRescueTestRunner(t)
			r.store.state.Observations = map[string]rescueObservation{}
			api.fake.mu.Lock()
			api.fake.torrents[0].Name = name
			api.fake.mu.Unlock()
			if err := r.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if api.addCalls != 1 || len(r.store.state.Groups["ABP-123"].Labels) != 0 {
				t.Fatal("title labels restricted a candidate of the same film")
			}
		})
	}
}

func TestRescueRemovesLegacyLabelRequirementsAndRefreshesSearch(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	r.search = func(context.Context, string) ([]rescueCandidate, error) { return nil, nil }
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	g.Labels = []string{"chinese-subtitles", "4k", "uncensored"}
	if !g.NextSearch.After(*now) {
		t.Fatal("search cooldown not established")
	}
	r.search = func(context.Context, string) ([]rescueCandidate, error) {
		return []rescueCandidate{{Hash: otherFilterTestHash, Magnet: "magnet:?xt=urn:btih:" + otherFilterTestHash, Size: 200 * mb, Score: 70}}, nil
	}
	*now = now.Add(time.Minute)
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.addCalls != 1 || len(g.Labels) != 0 {
		t.Fatal("legacy labels or search cooldown still blocked rescue")
	}
}

func TestRescueTitleLabelsDoNotChangeCandidateScore(t *testing.T) {
	r, api, _ := newRescueTestRunner(t)
	r.opts.MaxPerCode, r.opts.MaxActive = 1, 1
	api.fake.mu.Lock()
	api.fake.torrents[0].Name = "ABP-123-C"
	api.fake.mu.Unlock()
	subtitled := strings.Repeat("1", 40)
	r.search = func(context.Context, string) ([]rescueCandidate, error) {
		return []rescueCandidate{{Hash: otherFilterTestHash, Magnet: "magnet:?xt=urn:btih:" + otherFilterTestHash, Size: 200 * mb, Score: 70}, {Hash: subtitled, Magnet: "magnet:?xt=urn:btih:" + subtitled, Size: 200 * mb, Score: 69, Labels: []string{"chinese-subtitles"}}}, nil
	}
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	if api.addCalls != 1 || g.Downloads[otherFilterTestHash].Stage != "adding" || g.Downloads[subtitled].Candidate.Score != 69 {
		t.Fatal("subtitle or version label changed the candidate ranking")
	}
}

func TestRescueRealVideoValidation(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	r, _, _ := newRescueTestRunner(t)
	r.opts.FFmpeg = ffmpeg
	r.opts.FFprobe = ffprobe
	file := filepath.Join(r.opts.DownloadHostDir, "ABP-123.mp4")
	output, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=1280x720:r=1:d=65", "-c:v", "mpeg4", file).CombinedOutput()
	if err != nil {
		t.Fatalf("generate fixture: %v %s", err, output)
	}
	if err := r.verifyVideos(context.Background(), []string{file}); err != nil {
		t.Fatal(err)
	}
	r.opts.MinHeight = 2160
	if err := r.verifyVideos(context.Background(), []string{file}); err == nil {
		t.Fatal("actual height did not enforce the configured quality minimum")
	}
	r.opts.MinHeight = 720
	if err := os.WriteFile(file, []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := r.verifyVideos(context.Background(), []string{file}); err == nil {
		t.Fatal("corrupted video accepted")
	}
}
