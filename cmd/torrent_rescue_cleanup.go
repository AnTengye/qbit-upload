package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func (r *torrentRescueRunner) cleanupPublishedGroup(ctx context.Context, g *rescueGroup) error {
	winner := g.Downloads[g.Winner]
	if g.Stage != "published" || winner == nil || winner.Stage != "winner" {
		return fmt.Errorf("清理需要已持久化交付的唯一成功记录")
	}
	if err := r.recoverPublishedCandidates(ctx, g); err != nil {
		return err
	}
	hashes := make([]string, 0, len(g.Downloads))
	for h := range g.Downloads {
		if h != g.Winner {
			hashes = append(hashes, h)
		}
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		d := g.Downloads[h]
		if r.opts.DryRun {
			stepLog("[dry-run] 清理已交付补救组 %s 的其他候选 hash=%s", g.Code, h)
			continue
		}
		if err := r.cleanupDownload(ctx, g, d); err != nil {
			return fmt.Errorf("清理候选 %s: %w", h, err)
		}
		if err := r.removeCleanupGuard(g, d); err != nil {
			return err
		}
		delete(g.Downloads, h)
		delete(r.store.state.Observations, h)
		if err := r.store.save(); err != nil {
			return err
		}
		stepLog("已清理补救候选 %s hash=%s，保留成功版本 %s", g.Code, h, g.Winner)
	}
	return nil
}

// Old deployments or an add reply arriving after cleanup may leave a managed
// torrent outside Downloads. Adopt it only with the original completion guard,
// its tag, exact private path, and matching added_on before deleting it.
func (r *torrentRescueRunner) recoverPublishedCandidates(ctx context.Context, g *rescueGroup) error {
	ts, err := r.client.torrents(ctx, nil)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if !validTorrentHash(t.Hash) || t.AddedOn <= 0 || t.Hash == g.Winner || g.Downloads[t.Hash] != nil {
			continue
		}
		d := &rescueDownload{Candidate: rescueCandidate{Hash: t.Hash}, AddedOn: t.AddedOn, Stage: "failed"}
		if !r.ownsCandidate(g, d, t) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.opts.GuardDir, t.Hash+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var guard rescueGuard
		if json.Unmarshal(data, &guard) != nil || guard.Original || guard.Hash != t.Hash || guard.Code != g.Code || guard.Tag != rescueTag || (guard.AddedOn != 0 && guard.AddedOn != t.AddedOn) {
			continue
		}
		if !r.opts.DryRun {
			g.Downloads[t.Hash] = d
			if err := r.store.save(); err != nil {
				return err
			}
		} else {
			stepLog("[dry-run] 清理有归属凭据的历史候选 %s hash=%s", g.Code, t.Hash)
		}
	}
	return nil
}

func (r *torrentRescueRunner) cleanupTorrent(ctx context.Context, hash string) (qbitTorrent, bool, error) {
	ts, err := r.client.torrents(ctx, url.Values{"hashes": {hash}})
	if err != nil {
		return qbitTorrent{}, false, err
	}
	if len(ts) == 0 {
		return qbitTorrent{}, false, nil
	}
	if len(ts) != 1 || ts[0].Hash != hash {
		return qbitTorrent{}, false, fmt.Errorf("清理回读未返回指定的唯一任务")
	}
	return ts[0], true, nil
}

func (r *torrentRescueRunner) cleanupDownload(ctx context.Context, g *rescueGroup, d *rescueDownload) error {
	h := d.Candidate.Hash
	t, exists, err := r.cleanupTorrent(ctx, h)
	if err != nil {
		return err
	}
	if exists {
		// A queued discovery, a removed task, or a newly added instance is not
		// ours to delete, even when its infohash matches an old candidate.
		if d.Stage == "queued" || d.Stage == "removed" || (d.AddedOn > 0 && t.AddedOn != d.AddedOn) ||
			(d.Original && d.AddedOn <= 0) || (!d.Original && !r.ownsCandidate(g, d, t)) ||
			(d.Cleanup != nil && path.Clean(t.SavePath) != path.Clean(d.Cleanup.SavePath)) {
			stepLog("候选身份或归属已变，仅移除旧补救记录 %s hash=%s", g.Code, h)
			return nil
		}
		if d.Cleanup == nil {
			intent := &rescueCleanup{SavePath: t.SavePath}
			// Completed duplicates keep their data. Only unfinished downloads
			// are eligible for data deletion after the winner was published.
			if t.Progress < 1 || t.AmountLeft > 0 {
				files, err := r.client.files(ctx, h)
				if err != nil {
					return err
				}
				for _, f := range files {
					intent.Files = append(intent.Files, f.Name)
				}
				intent.DeleteFiles, err = r.cleanupDataSafe(ctx, g, d, intent)
				if err != nil {
					return err
				}
			}
			d.AddedOn = t.AddedOn
			d.Cleanup = intent
			d.Stage = "deleting"
			if err := r.store.save(); err != nil {
				return err
			}
		}
		// Recheck immediately before the irreversible API call. Never retry a
		// deletion against a replacement torrent or newly completed duplicate.
		t, exists, err = r.cleanupTorrent(ctx, h)
		if err != nil {
			return err
		}
		if exists {
			if t.AddedOn != d.AddedOn || path.Clean(t.SavePath) != path.Clean(d.Cleanup.SavePath) || (!d.Original && !r.ownsCandidate(g, d, t)) {
				return nil
			}
			if d.Cleanup.DeleteFiles && t.Progress >= 1 && t.AmountLeft == 0 {
				d.Cleanup.DeleteFiles = false
				if err := r.store.save(); err != nil {
					return err
				}
			}
			if d.Cleanup.DeleteFiles {
				safe, err := r.cleanupDataSafe(ctx, g, d, d.Cleanup)
				if err != nil {
					return err
				}
				if !safe {
					d.Cleanup.DeleteFiles = false
					if err := r.store.save(); err != nil {
						return err
					}
				}
			}
			deleteErr := r.client.deleteTorrent(ctx, h, d.Cleanup.DeleteFiles)
			_, stillExists, checkErr := r.cleanupTorrent(ctx, h)
			if checkErr != nil {
				return checkErr
			}
			if stillExists {
				if deleteErr != nil {
					return deleteErr
				}
				return fmt.Errorf("删除尚未获回读确认，保留清理意图等待重试")
			}
		}
	}
	if d.Cleanup == nil || !d.Cleanup.DeleteFiles {
		return nil
	}
	// qBittorrent can leave private folders or .!qB files behind. After its
	// record is absent, finish only the persisted, independently scoped data.
	safe, err := r.cleanupDataSafe(ctx, g, d, d.Cleanup)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("残留数据与其他任务重叠或路径已变化，停止数据清理")
	}
	if !d.Original {
		root, err := r.localDownloadPath(path.Dir(d.Cleanup.SavePath), path.Base(d.Cleanup.SavePath))
		if err != nil {
			return err
		}
		return os.RemoveAll(root)
	}
	for _, name := range d.Cleanup.Files {
		p, err := r.localDownloadPath(d.Cleanup.SavePath, name)
		if err != nil {
			return err
		}
		for _, filename := range []string{p, p + ".!qB"} {
			info, err := os.Lstat(filename)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("原任务残留不是普通文件，停止清理")
			}
			if err := os.Remove(filename); err != nil {
				return err
			}
		}
	}
	return nil
}

func rescuePathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// Refuse symlink ancestors as well as symlink leaf files before data deletion.
// A missing suffix is normal for metadata-only or already removed downloads.
func rescueCleanupPathSafe(filename, root string) bool {
	rel, err := filepath.Rel(root, filename)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	p := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, component)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func (r *torrentRescueRunner) cleanupDataSafe(ctx context.Context, g *rescueGroup, d *rescueDownload, intent *rescueCleanup) (bool, error) {
	wanted := []string{}
	if !d.Original {
		if path.Clean(intent.SavePath) != path.Join(r.opts.CandidatePath, g.Code, d.Candidate.Hash) {
			return false, nil
		}
		root, err := r.localDownloadPath(path.Dir(intent.SavePath), path.Base(intent.SavePath))
		if err != nil {
			return false, nil
		}
		wanted = append(wanted, root)
	}
	for _, name := range intent.Files {
		p, err := r.localDownloadPath(intent.SavePath, name)
		if err != nil || !rescueCleanupPathSafe(p, r.opts.DownloadHostDir) || !rescueCleanupPathSafe(p+".!qB", r.opts.DownloadHostDir) {
			return false, nil
		}
		if d.Original {
			wanted = append(wanted, p, p+".!qB")
		}
	}
	for _, p := range wanted {
		if !rescueCleanupPathSafe(p, r.opts.DownloadHostDir) || rescuePathsOverlap(p, g.PublishPath) {
			return false, nil
		}
	}
	ts, err := r.client.torrents(ctx, nil)
	if err != nil {
		return false, err
	}
	// Protect the winner's private directory even if its qBittorrent entry was
	// removed after publication. An absent legacy original has no saved path,
	// so keep data rather than guess which shared directory contained it.
	winner := g.Downloads[g.Winner]
	winnerPresent := false
	for _, t := range ts {
		if t.Hash == g.Winner {
			winnerPresent = true
		}
	}
	if !winnerPresent && winner.Original {
		return false, nil
	}
	if !winner.Original {
		p, err := r.localDownloadPath(path.Join(r.opts.CandidatePath, g.Code), g.Winner)
		if err != nil {
			return false, nil
		}
		for _, want := range wanted {
			if rescuePathsOverlap(want, p) {
				return false, nil
			}
		}
	}
	for _, t := range ts {
		if t.Hash == d.Candidate.Hash {
			if t.AddedOn != d.AddedOn && d.AddedOn > 0 {
				return false, nil
			}
			continue
		}
		// Only inspect file metadata when save directories could overlap.
		save := filepath.Join(r.opts.DownloadHostDir, filepath.FromSlash(strings.TrimPrefix(path.Clean(t.SavePath), path.Clean(r.opts.DownloadPrefix)+"/")))
		if path.Clean(t.SavePath) == path.Clean(r.opts.DownloadPrefix) {
			save = r.opts.DownloadHostDir
		}
		couldOverlap := false
		for _, want := range wanted {
			couldOverlap = couldOverlap || rescuePathsOverlap(want, save)
		}
		if !couldOverlap {
			continue
		}
		files, err := r.client.files(ctx, t.Hash)
		if err != nil {
			return false, err
		}
		for _, f := range files {
			p, err := r.localDownloadPath(t.SavePath, f.Name)
			if err != nil {
				return false, nil
			}
			for _, want := range wanted {
				if rescuePathsOverlap(want, p) || rescuePathsOverlap(want, p+".!qB") {
					return false, nil
				}
			}
		}
	}
	return len(wanted) > 0, nil
}

func (r *torrentRescueRunner) removeCleanupGuard(g *rescueGroup, d *rescueDownload) error {
	p := filepath.Join(r.opts.GuardDir, d.Candidate.Hash+".json")
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var guard rescueGuard
	if err := json.Unmarshal(data, &guard); err != nil {
		return fmt.Errorf("清理完成回调记录失败: %w", err)
	}
	if guard.Code != g.Code || guard.Hash != d.Candidate.Hash || (guard.AddedOn != 0 && guard.AddedOn != d.AddedOn) {
		return nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncJobDirectory(r.opts.GuardDir)
}
