package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type rescueObservation struct {
	AddedOn      int64     `json:"added_on"`
	Completed    int64     `json:"completed"`
	LastProgress time.Time `json:"last_progress"`
	ObservedAt   time.Time `json:"observed_at"`
}
type rescueDownload struct {
	Candidate     rescueCandidate `json:"candidate"`
	AddedOn       int64           `json:"added_on"`
	Original      bool            `json:"original"`
	Stage         string          `json:"stage"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     time.Time       `json:"started_at"`
	LastProgress  time.Time       `json:"last_progress"`
	LastCompleted int64           `json:"last_completed"`
	CooldownUntil time.Time       `json:"cooldown_until"`
	RuntimeScore  int             `json:"runtime_score"`
	AverageSpeed  float64         `json:"average_completed_bytes_per_second"`
	LastObserved  time.Time       `json:"last_observed"`
	Reason        string          `json:"reason,omitempty"`
	Files         []qbitFile      `json:"files,omitempty"`
	MetadataBytes int64           `json:"metadata_total_bytes,omitempty"`
	FilmBytes     int64           `json:"verified_film_bytes,omitempty"`
	Cleanup       *rescueCleanup  `json:"cleanup,omitempty"`
}

// Persist deletion intent before calling qBittorrent. An absent torrent on
// restart can then finish the same scoped file cleanup without reissuing delete.
type rescueCleanup struct {
	SavePath    string   `json:"save_path"`
	DeleteFiles bool     `json:"delete_files"`
	Files       []string `json:"files,omitempty"`
}
type rescueGroup struct {
	Code        string                     `json:"code"`
	Labels      []string                   `json:"required_labels,omitempty"`
	Stage       string                     `json:"stage"`
	NextSearch  time.Time                  `json:"next_search"`
	Downloads   map[string]*rescueDownload `json:"downloads"`
	Winner      string                     `json:"winner,omitempty"`
	PublishPath string                     `json:"publish_path,omitempty"`
	LastError   string                     `json:"last_error,omitempty"`
}
type rescueState struct {
	Version      int                          `json:"version"`
	Server       string                       `json:"server"`
	Observations map[string]rescueObservation `json:"observations"`
	Groups       map[string]*rescueGroup      `json:"groups"`
}
type rescueStore struct {
	dir     string
	state   rescueState
	release func()
	dryRun  bool
}

func openRescueStore(dir, server string, dryRun bool) (*rescueStore, error) {
	s := &rescueStore{dir: dir, dryRun: dryRun, state: rescueState{Version: 1, Server: server, Observations: map[string]rescueObservation{}, Groups: map[string]*rescueGroup{}}}
	if !dryRun {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		release, err := lockJobStore(filepath.Join(dir, ".lock"))
		if err != nil {
			return nil, err
		}
		s.release = release
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		s.close()
		return nil, err
	}
	if err = json.Unmarshal(data, &s.state); err != nil || s.state.Version != 1 || s.state.Server != server || s.state.Observations == nil || s.state.Groups == nil {
		s.close()
		return nil, fmt.Errorf("补救状态损坏或属于其他 qBittorrent 实例")
	}
	for code, g := range s.state.Groups {
		if g == nil || g.Code != code || !validRescueCode(code) || g.Downloads == nil || !validRescueGroupStage(g.Stage) {
			s.close()
			return nil, fmt.Errorf("补救组记录无效")
		}
		if g.Winner != "" && !validTorrentHash(g.Winner) {
			s.close()
			return nil, fmt.Errorf("补救成功记录无效")
		}
		for h, d := range g.Downloads {
			if d == nil || !validTorrentHash(h) || d.Candidate.Hash != h || !validRescueDownloadStage(d.Stage) {
				s.close()
				return nil, fmt.Errorf("补救下载记录无效")
			}
		}
	}
	return s, nil
}
func validRescueGroupStage(s string) bool {
	return s == "open" || s == "publishing" || s == "published"
}
func validRescueDownloadStage(s string) bool {
	switch s {
	case "original", "queued", "adding", "metadata", "ready", "start-requested", "running", "quality-stopping", "stopping", "failed", "manual-stopped", "removed", "winner", "deleting":
		return true
	}
	return false
}
func (s *rescueStore) close() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}
func (s *rescueStore) save() error {
	if s.dryRun {
		return nil
	}
	return writeRescueJSON(filepath.Join(s.dir, "state.json"), s.state, 0600)
}

func writeRescueJSON(filename string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".rescue-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp.Name(), filename); err != nil {
		return err
	}
	return syncJobDirectory(filepath.Dir(filename))
}

type rescueGuard struct {
	Original bool   `json:"original"`
	Hash     string `json:"hash"`
	AddedOn  int64  `json:"added_on"`
	Code     string `json:"code"`
	Tag      string `json:"tag"`
}

func (s *rescueStore) guard(dir string, g *rescueGroup, d *rescueDownload) error {
	if s.dryRun {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return writeRescueJSON(filepath.Join(dir, d.Candidate.Hash+".json"), rescueGuard{Hash: d.Candidate.Hash, AddedOn: d.AddedOn, Code: g.Code, Tag: rescueTag, Original: d.Original}, 0644)
}
