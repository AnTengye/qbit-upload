package cmd

import (
	"context"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"
)

func TestRescueRejectsCollectionsAndCountsOnlyOneFilm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []qbitFile
		want  int64
	}{
		{"one film with attachments", []qbitFile{{Name: "ABP-123.mp4", Size: 20 << 30, Priority: 1}, {Name: "poster.jpg", Size: 10 << 30, Priority: 1}, {Name: "advertisement.mp4", Size: 2 * mb, Priority: 1}}, 20 << 30},
		{"continuous parts", []qbitFile{{Name: "ABP-123-CD1.mp4", Size: 10 << 30, Priority: 1}, {Name: "ABP-123-CD2.mp4", Size: 10 << 30, Priority: 1}}, 20 << 30},
		{"foreign movie even deselected", []qbitFile{{Name: "ABP-123.mp4", Size: 1 << 30, Priority: 1}, {Name: "ABP-124.mp4", Size: 99 << 30, Priority: 0}}, 0},
		{"two complete versions", []qbitFile{{Name: "ABP-123.mp4", Size: 1 << 30, Priority: 1}, {Name: "ABP-123-1080p.mkv", Size: 1 << 30, Priority: 1}}, 0},
		{"missing part", []qbitFile{{Name: "ABP-123-CD1.mp4", Size: 1 << 30, Priority: 1}, {Name: "ABP-123-CD3.mp4", Size: 1 << 30, Priority: 1}}, 0},
		{"unselected part", []qbitFile{{Name: "ABP-123-CD1.mp4", Size: 1 << 30, Priority: 1}, {Name: "ABP-123-CD2.mp4", Size: 1 << 30, Priority: 0}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := selectRescueVideos(tc.files, "ABP-123", 100*mb)
			if tc.want == 0 {
				if err == nil {
					t.Fatal("accepted collection or incomplete parts")
				}
				return
			}
			if err != nil || rescueFilmBytes(selected) != tc.want {
				t.Fatalf("film size=%d, err=%v", rescueFilmBytes(selected), err)
			}
		})
	}
	var collection []qbitFile
	for i := 0; i < 100; i++ {
		collection = append(collection, qbitFile{Name: fmt.Sprintf("ABP-%03d.mp4", 123+i), Size: 1 << 30, Priority: 1})
	}
	if _, err := selectRescueVideos(collection, "ABP-123", 100*mb); err == nil {
		t.Fatal("100 GiB collection of 1 GiB films was accepted")
	}
}

func TestRescueMetadataRankingDoesNotTreatRSSTotalAsFilmSize(t *testing.T) {
	opts := torrentRescueOptions{PreferredVideoSize: 4 << 30, IdealVideoSize: 20 << 30}
	available := &rescueDownload{Candidate: rescueCandidate{Hash: otherFilterTestHash, Size: 63 * (1 << 30) / 10, Score: 94}, FilmBytes: 63 * (1 << 30) / 10}
	unknown := &rescueDownload{Candidate: rescueCandidate{Hash: filterTestHash, Size: 64 * (1 << 30) / 10, Score: 40}, FilmBytes: 64 * (1 << 30) / 10}
	if !rescueQualityLess(available, unknown, opts, true) {
		t.Fatal("unverified torrent total displaced the more reachable metadata source")
	}
	if !rescueQualityLess(unknown, available, opts, false) {
		t.Fatal("verified film quality was not used for download priority")
	}
}

func addQualityTestMetadata(r *torrentRescueRunner, api *rescueTestAPI, now time.Time, hash string, sourceSize, filmSize int64, score int) *rescueDownload {
	g := r.store.state.Groups["ABP-123"]
	d := &rescueDownload{Candidate: rescueCandidate{Hash: hash, Title: "ABP-123", Magnet: "magnet:?xt=urn:btih:" + hash, Size: sourceSize, Score: score}, AddedOn: now.Unix(), Stage: "metadata", CreatedAt: now}
	g.Downloads[hash] = d
	api.fake.mu.Lock()
	defer api.fake.mu.Unlock()
	api.fake.torrents = append(api.fake.torrents, qbitTorrent{Hash: hash, Name: "ABP-123", AddedOn: d.AddedOn, State: "stoppedDL", Size: filmSize + 2*mb, Tags: rescueTag, SavePath: path.Join(r.opts.CandidatePath, g.Code, hash)})
	api.fake.files[hash] = []qbitFile{{Index: 1, Name: "ABP-123.mp4", Size: filmSize, Priority: 1}, {Index: 2, Name: "advertisement.txt", Size: 2 * mb, Priority: 1}}
	return d
}

func TestRescueQualityUsesFilmSizeAndEnablesFallbackAfterFailure(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	small := g.Downloads[otherFilterTestHash]
	small.Candidate.Size = 100 << 30 // Big attachments are not a 100 GiB film.
	api.fake.mu.Lock()
	api.fake.torrents[1].Size = 100 << 30
	api.fake.files[otherFilterTestHash][0].Size = 1 << 30
	api.fake.mu.Unlock()
	large := addQualityTestMetadata(r, api, *now, strings.Repeat("1", 40), 20<<30, 20<<30, 1)
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if small.Candidate.Size != 100<<30 || small.FilmBytes != 1<<30 || small.Stage != "ready" || large.Stage != "running" {
		t.Fatalf("size estimate won over actual film: small=%+v large=%+v", small, large)
	}
	if small.MetadataBytes != (1<<30)+2*mb {
		t.Fatalf("wanted size was treated as total metadata size: %d", small.MetadataBytes)
	}
	if countFilterCalls(api.fake, "POST torrents/start") != 1 {
		t.Fatal("small fallback started with preferred film")
	}
	for i := 0; i < 28; i++ {
		*now = now.Add(15 * time.Minute)
		if err := r.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if large.Stage != "failed" || small.Stage != "running" {
		t.Fatalf("failed preferred film did not release fallback: large=%s small=%s", large.Stage, small.Stage)
	}
}

func TestRescueSourceFailureDoesNotBlockPreparedDownload(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	d := g.Downloads[otherFilterTestHash]
	d.Stage = "ready"
	api.fake.mu.Lock()
	api.fake.torrents[1].State = "stoppedDL"
	api.fake.mu.Unlock()
	g.NextSearch = time.Time{}
	r.search = func(context.Context, string) ([]rescueCandidate, error) { return nil, fmt.Errorf("source unavailable") }
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err == nil {
		t.Fatal("source failure was hidden")
	}
	if d.Stage != "running" || api.addCalls != 1 || countFilterCalls(api.fake, "POST torrents/start") != 2 {
		t.Fatal("source failure blocked prepared fallback or added stale candidate")
	}
}

func TestRescueQualityPrefersNearTwentyWithParallelPeers(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	four := g.Downloads[otherFilterTestHash]
	four.Candidate.Size = 4 << 30
	four.Candidate.Score = 100
	api.fake.mu.Lock()
	api.fake.torrents[1].Size = 4 << 30
	api.fake.files[otherFilterTestHash][0].Size = 4 << 30
	api.fake.mu.Unlock()
	twenty := addQualityTestMetadata(r, api, *now, strings.Repeat("1", 40), 20<<30, 20<<30, 1)
	twentyTwo := addQualityTestMetadata(r, api, *now, strings.Repeat("2", 40), 22<<30, 22<<30, 2)
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if four.Stage != "ready" || twenty.Stage != "running" || twentyTwo.Stage != "running" || countFilterCalls(api.fake, "POST torrents/start") != 2 {
		t.Fatalf("preferred tier not selected: %s %s %s", four.Stage, twenty.Stage, twentyTwo.Stage)
	}
}

func TestRescueBetterDiscoveryFreesSlotsAndHoldsSmallCompletion(t *testing.T) {
	r, api, now := newRescueTestRunner(t)
	r.opts.MaxPerCode, r.opts.MaxActive = 1, 1
	ctx := context.Background()
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	g := r.store.state.Groups["ABP-123"]
	small := g.Downloads[otherFilterTestHash]
	api.fake.mu.Lock()
	api.fake.torrents[1].Progress = 1
	api.fake.torrents[1].AmountLeft = 0
	api.fake.torrents[1].State = "uploading"
	api.fake.files[otherFilterTestHash][0].Progress = 1
	api.fake.mu.Unlock()
	g.NextSearch = time.Time{}
	hash := strings.Repeat("1", 40)
	r.search = func(context.Context, string) ([]rescueCandidate, error) {
		return []rescueCandidate{{Hash: hash, Magnet: "magnet:?xt=urn:btih:" + hash, Size: 20 << 30, Score: 1}}, nil
	}
	verified := 0
	r.verify = func(context.Context, []string) error { verified++; return nil }
	*now = now.Add(time.Minute)
	if err := r.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if verified != 0 || g.Winner != "" || small.Stage != "ready" || api.addCalls != 2 {
		t.Fatalf("small completion won or blocked better metadata: verified=%d winner=%s stage=%s adds=%d", verified, g.Winner, small.Stage, api.addCalls)
	}
}
