package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type torrentFilterJob struct {
	Version      int        `json:"version"`
	Hash         string     `json:"hash"`
	AddedOn      int64      `json:"added_on"`
	Server       string     `json:"server"`
	Tag          string     `json:"tag"`
	Stage        string     `json:"stage"`
	MinSizeBytes int64      `json:"min_size_bytes"`
	Files        []qbitFile `json:"files,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	NextRetry    time.Time  `json:"next_retry,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type torrentFilterStore struct {
	dir     string
	release func()
	jobs    map[string]torrentFilterJob
}

func openTorrentFilterStore(dir string) (*torrentFilterStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建种子过滤状态目录失败: %w", err)
	}
	release, err := lockJobStore(filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, err
	}
	s := &torrentFilterStore{dir: dir, release: release, jobs: make(map[string]torrentFilterJob)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		release()
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			release()
			return nil, err
		}
		var job torrentFilterJob
		if err := json.Unmarshal(data, &job); err != nil {
			release()
			return nil, fmt.Errorf("种子过滤记录损坏 %s: %w", entry.Name(), err)
		}
		if job.Version != 1 || !validTorrentHash(job.Hash) || job.Hash+".json" != entry.Name() ||
			job.AddedOn <= 0 || job.Server == "" || job.Tag == "" || !validFilterStage(job.Stage) ||
			(job.Stage != "waiting" && (job.MinSizeBytes <= 0 || len(job.Files) == 0)) {
			release()
			return nil, fmt.Errorf("种子过滤记录字段无效: %s", entry.Name())
		}
		s.jobs[job.Hash] = job
	}
	return s, nil
}

func validFilterStage(stage string) bool {
	switch stage {
	case "waiting", "filtering", "verified", "start-requested", "done":
		return true
	default:
		return false
	}
}

func (s *torrentFilterStore) close() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

func (s *torrentFilterStore) save(job *torrentFilterJob) error {
	job.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".filter-*.tmp")
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
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, job.Hash+".json")); err != nil {
		return err
	}
	if err := syncJobDirectory(s.dir); err != nil {
		return err
	}
	s.jobs[job.Hash] = *job
	return nil
}
