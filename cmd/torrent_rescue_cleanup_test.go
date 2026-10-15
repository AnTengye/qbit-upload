package cmd

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func newPublishedRescueTest(t *testing.T) (*torrentRescueRunner, *rescueTestAPI, *rescueGroup, string) {
	t.Helper()
	r, api, _ := newRescueTestRunner(t)
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	g.Stage, g.Winner = "published", filterTestHash
	g.PublishPath = filepath.Join(r.opts.PublishDir, "rescue-ABP-123-"+filterTestHash[:12])
	d := g.Downloads[filterTestHash]
	d.Stage = "winner"
	d.Files = []qbitFile{{Index: 7, Name: "ABP-123.mp4", Size: 4, Priority: 1, Progress: 1}}
	api.fake.mu.Lock()
	api.fake.torrents[0].Progress, api.fake.torrents[0].AmountLeft = 1, 0
	api.fake.torrents[0].State = "stoppedUP"
	api.fake.files[filterTestHash] = d.Files
	g.Downloads[otherFilterTestHash].AddedOn = api.fake.torrents[1].AddedOn
	g.Downloads[otherFilterTestHash].Stage = "metadata"
	api.fake.mu.Unlock()
	for _, dir := range []string{filepath.Join(r.opts.DownloadHostDir, "original"), g.PublishPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ABP-123.mp4"), []byte("film"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRescueJSON(filepath.Join(g.PublishPath, ".qbit-rescue.json"), rescueManifest{Code: g.Code, Hash: g.Winner}, 0644); err != nil {
		t.Fatal(err)
	}
	loserDir := filepath.Join(r.opts.DownloadHostDir, "rescue", g.Code, otherFilterTestHash)
	if err := os.MkdirAll(loserDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loserDir, "ABP-123.mp4.!qB"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.store.guard(r.opts.GuardDir, g, g.Downloads[otherFilterTestHash]); err != nil {
		t.Fatal(err)
	}
	if err := r.store.save(); err != nil {
		t.Fatal(err)
	}
	return r, api, g, loserDir
}

func assertRescueWinnerPreserved(t *testing.T, r *torrentRescueRunner, api *rescueTestAPI, g *rescueGroup) {
	t.Helper()
	if g.Stage != "published" || len(g.Downloads) != 1 || g.Downloads[g.Winner] == nil || g.Downloads[g.Winner].Stage != "winner" {
		t.Fatal("completed group did not converge to its single winner")
	}
	api.fake.mu.Lock()
	defer api.fake.mu.Unlock()
	if len(api.fake.torrents) != 1 || api.fake.torrents[0].Hash != g.Winner {
		t.Fatal("winner removed or losing torrent retained")
	}
	for _, filename := range []string{filepath.Join(r.opts.DownloadHostDir, "original", "ABP-123.mp4"), filepath.Join(g.PublishPath, "ABP-123.mp4")} {
		if data, err := os.ReadFile(filename); err != nil || string(data) != "film" {
			t.Fatal("winner's data or published copy was modified")
		}
	}
	if _, err := os.Stat(filepath.Join(r.opts.GuardDir, g.Winner+".json")); err != nil {
		t.Fatal("winner completion guard was removed")
	}
}

func TestRescueCleanupPublishedGroupRetainsOnlyWinner(t *testing.T) {
	r, api, g, loserDir := newPublishedRescueTest(t)
	queued := fmt.Sprintf("%040x", 99)
	g.Downloads[queued] = &rescueDownload{Candidate: rescueCandidate{Hash: queued}, Stage: "queued"}
	r.store.state.Observations[otherFilterTestHash] = rescueObservation{AddedOn: g.Downloads[otherFilterTestHash].AddedOn}
	if err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRescueWinnerPreserved(t, r, api, g)
	if len(api.deleteCalls) != 1 || api.deleteCalls[0] != (rescueTestDelete{Hash: otherFilterTestHash, DeleteFiles: true}) {
		t.Fatal("unfinished deletion did not target the owned candidate and its data")
	}
	for _, p := range []string{loserDir, filepath.Join(r.opts.GuardDir, otherFilterTestHash+".json")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("loser data or guard retained: %s", p)
		}
	}
	if _, exists := r.store.state.Observations[otherFilterTestHash]; exists {
		t.Fatal("loser observation retained")
	}
	state, err := os.ReadFile(filepath.Join(r.opts.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.store.close()
	restored, err := openRescueStore(r.opts.StateDir, r.opts.Qbit.BaseURL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if len(restored.state.Groups[g.Code].Downloads) != 1 || len(state) == 0 {
		t.Fatal("single winner state was not durable")
	}
}

func TestRescueCleanupDeleteReadbackAndRestart(t *testing.T) {
	for _, mode := range []string{"lost reply", "rejected", "delayed"} {
		t.Run(mode, func(t *testing.T) {
			r, api, g, loserDir := newPublishedRescueTest(t)
			api.loseDelete, api.rejectDelete, api.ignoreDelete = mode == "lost reply", mode == "rejected", mode == "delayed"
			err := r.poll(context.Background())
			if mode == "lost reply" {
				if err != nil {
					t.Fatal(err)
				}
				assertRescueWinnerPreserved(t, r, api, g)
			} else {
				if err == nil || g.Downloads[otherFilterTestHash].Stage != "deleting" || g.Downloads[otherFilterTestHash].Cleanup == nil {
					t.Fatal("unconfirmed deletion discarded its persisted intent")
				}
				if _, err := os.Stat(loserDir); err != nil {
					t.Fatal("data deleted before torrent removal was confirmed")
				}
			}
			r.store.close()
			restarted, err := newTorrentRescueRunner(r.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.store.close()
			restarted.now, restarted.search, restarted.verify = r.now, r.search, r.verify
			api.rejectDelete, api.ignoreDelete = false, false
			if err := restarted.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertRescueWinnerPreserved(t, restarted, api, restarted.store.state.Groups[g.Code])
			wantCalls := 2
			if mode == "lost reply" {
				wantCalls = 1
			}
			if len(api.deleteCalls) != wantCalls {
				t.Fatal("lost reply duplicated deletion or restart did not retry")
			}
		})
	}
}

func TestRescueCleanupDoesNotDeleteReplacementOrUnownedTorrent(t *testing.T) {
	for _, change := range []string{"readded", "untagged", "moved", "queued"} {
		t.Run(change, func(t *testing.T) {
			r, api, g, loserDir := newPublishedRescueTest(t)
			api.fake.mu.Lock()
			switch change {
			case "readded":
				api.fake.torrents[1].AddedOn++
			case "untagged":
				api.fake.torrents[1].Tags = ""
			case "moved":
				api.fake.torrents[1].SavePath = "/downloads/saved"
			case "queued":
				g.Downloads[otherFilterTestHash].Stage = "queued"
				g.Downloads[otherFilterTestHash].AddedOn = 0
			}
			api.fake.mu.Unlock()
			if err := r.cleanupPublishedGroup(context.Background(), g); err != nil {
				t.Fatal(err)
			}
			if len(api.deleteCalls) != 0 || len(g.Downloads) != 1 {
				t.Fatal("changed instance was deleted or obsolete record retained")
			}
			if _, err := os.Stat(loserDir); err != nil {
				t.Fatal("unowned data deleted")
			}
		})
	}
}

func TestRescueCleanupPreservesCompletedDuplicateAndSharedData(t *testing.T) {
	for _, mode := range []string{"completed", "overlap", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			r, api, g, loserDir := newPublishedRescueTest(t)
			api.fake.mu.Lock()
			switch mode {
			case "completed":
				api.fake.torrents[1].Progress, api.fake.torrents[1].AmountLeft = 1, 0
			case "overlap":
				api.fake.torrents[0].SavePath = api.fake.torrents[1].SavePath
			case "symlink":
				if err := os.Symlink(filepath.Join(r.opts.DownloadHostDir, "original", "ABP-123.mp4"), filepath.Join(loserDir, "ABP-123.mp4")); err != nil {
					api.fake.mu.Unlock()
					t.Skip(err)
				}
			}
			api.fake.mu.Unlock()
			if err := r.cleanupPublishedGroup(context.Background(), g); err != nil {
				t.Fatal(err)
			}
			if len(api.deleteCalls) != 1 || api.deleteCalls[0].DeleteFiles || len(g.Downloads) != 1 {
				t.Fatal("completed, shared or unsafe data was eligible for deletion")
			}
			if _, err := os.Stat(loserDir); err != nil {
				t.Fatal("protected duplicate data deleted")
			}
		})
	}
}

func TestRescueCleanupRecoversGuardedOrphanAndProtectsOpenGroups(t *testing.T) {
	r, api, g, loserDir := newPublishedRescueTest(t)
	delete(g.Downloads, otherFilterTestHash)
	openHash := fmt.Sprintf("%040x", 88)
	openGroup := &rescueGroup{Code: "ABP-124", Stage: "open", Downloads: map[string]*rescueDownload{}}
	openDownload := &rescueDownload{Candidate: rescueCandidate{Hash: openHash}, AddedOn: r.now().Unix(), Stage: "metadata"}
	openGroup.Downloads[openHash] = openDownload
	r.store.state.Groups[openGroup.Code] = openGroup
	api.fake.mu.Lock()
	api.fake.torrents = append(api.fake.torrents, qbitTorrent{Hash: openHash, AddedOn: openDownload.AddedOn, Tags: rescueTag, SavePath: path.Join(r.opts.CandidatePath, openGroup.Code, openHash), State: "stoppedDL"})
	api.fake.mu.Unlock()
	if err := r.cleanupPublishedGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if len(g.Downloads) != 1 || len(api.deleteCalls) != 1 || api.deleteCalls[0].Hash != otherFilterTestHash || len(openGroup.Downloads) != 1 {
		t.Fatal("guarded orphan was missed or an unfinished group was cleaned")
	}
	if _, err := os.Stat(loserDir); !os.IsNotExist(err) {
		t.Fatal("orphan partial data retained")
	}
}

func TestRescueCleanupDryRunAndPublicationGate(t *testing.T) {
	r, api, g, loserDir := newPublishedRescueTest(t)
	before, err := os.ReadFile(filepath.Join(r.opts.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.opts.DryRun = true
	if err := r.cleanupPublishedGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	r.opts.DryRun = false
	for _, stage := range []string{"open", "publishing"} {
		g.Stage = stage
		if err := r.cleanupPublishedGroup(context.Background(), g); err == nil {
			t.Fatal("unpublished group was eligible for cleanup")
		}
	}
	after, err := os.ReadFile(filepath.Join(r.opts.StateDir, "state.json"))
	if err != nil || string(after) != string(before) || len(api.deleteCalls) != 0 || len(g.Downloads) != 2 {
		t.Fatal("dry run or publication gate changed state or qBittorrent")
	}
	if _, err := os.Stat(loserDir); err != nil {
		t.Fatal("dry run deleted data")
	}
}

func TestRescueCleanupOriginalLeavesSharedDownloadDirectory(t *testing.T) {
	r, api, g, winnerDir := newPublishedRescueTest(t)
	g.Winner = otherFilterTestHash
	g.PublishPath = filepath.Join(r.opts.PublishDir, "rescue-ABP-123-"+g.Winner[:12])
	g.Downloads[g.Winner].Stage = "winner"
	g.Downloads[g.Winner].Files = []qbitFile{{Index: 12, Name: "ABP-123.mp4", Size: 4, Priority: 1, Progress: 1}}
	g.Downloads[filterTestHash].Stage = "original"
	api.fake.mu.Lock()
	api.fake.torrents[0].Progress, api.fake.torrents[0].AmountLeft = 0.5, 2
	api.fake.torrents[0].SavePath = "/downloads"
	api.fake.torrents[1].Progress, api.fake.torrents[1].AmountLeft = 1, 0
	api.fake.torrents[1].State = "stoppedUP"
	api.fake.mu.Unlock()
	partial := filepath.Join(r.opts.DownloadHostDir, "ABP-123.mp4.!qB")
	unrelated := filepath.Join(r.opts.DownloadHostDir, "keep.txt")
	for _, filename := range []string{partial, unrelated, filepath.Join(winnerDir, "ABP-123.mp4")} {
		if err := os.WriteFile(filename, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.cleanupPublishedGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if len(api.deleteCalls) != 1 || api.deleteCalls[0] != (rescueTestDelete{Hash: filterTestHash, DeleteFiles: true}) || len(g.Downloads) != 1 {
		t.Fatal("unfinished original was not removed after its replacement completed")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("original partial file retained")
	}
	for _, filename := range []string{unrelated, filepath.Join(winnerDir, "ABP-123.mp4")} {
		if data, err := os.ReadFile(filename); err != nil || string(data) != "data" {
			t.Fatal("shared download root or winner data was removed")
		}
	}
}

func TestRescueCleanupRecoversAbsentTorrentWithoutReissuingDelete(t *testing.T) {
	r, api, g, loserDir := newPublishedRescueTest(t)
	d := g.Downloads[otherFilterTestHash]
	d.Stage = "deleting"
	d.Cleanup = &rescueCleanup{SavePath: path.Join(r.opts.CandidatePath, g.Code, otherFilterTestHash), DeleteFiles: true, Files: []string{"ABP-123.mp4"}}
	api.fake.mu.Lock()
	api.fake.torrents = api.fake.torrents[:1]
	delete(api.fake.files, otherFilterTestHash)
	api.fake.mu.Unlock()
	if err := r.store.save(); err != nil {
		t.Fatal(err)
	}
	r.store.close()
	restarted, err := newTorrentRescueRunner(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.store.close()
	if err := restarted.cleanupPublishedGroup(context.Background(), restarted.store.state.Groups[g.Code]); err != nil {
		t.Fatal(err)
	}
	assertRescueWinnerPreserved(t, restarted, api, restarted.store.state.Groups[g.Code])
	if len(api.deleteCalls) != 0 {
		t.Fatal("already removed torrent deletion was reissued")
	}
	if _, err := os.Stat(loserDir); !os.IsNotExist(err) {
		t.Fatal("private data cleanup was not resumed after crash")
	}
}
