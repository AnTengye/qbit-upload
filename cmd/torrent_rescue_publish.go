package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type rescueManifest struct {
	Code string `json:"code"`
	Hash string `json:"hash"`
}

func (r *torrentRescueRunner) localDownloadPath(savePath, name string) (string, error) {
	prefix := path.Clean(r.opts.DownloadPrefix)
	full := path.Join(savePath, name)
	if path.IsAbs(name) || strings.Contains(name, `\`) || !strings.HasPrefix(full, prefix+"/") {
		return "", fmt.Errorf("下载文件路径超出映射范围")
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".." {
			return "", fmt.Errorf("下载文件路径含父目录跳转")
		}
	}
	return filepath.Join(r.opts.DownloadHostDir, filepath.FromSlash(strings.TrimPrefix(full, prefix+"/"))), nil
}
func regularRescueFile(filename, root string) (os.FileInfo, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolvedFile, err := filepath.EvalSymlinks(filename)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("文件符号链接超出下载目录")
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("视频不是普通文件")
	}
	return info, nil
}
func (r *torrentRescueRunner) verifyVideos(ctx context.Context, files []string) error {
	minHeight := r.opts.MinHeight
	for _, filename := range files {
		if _, err := regularRescueFile(filename, r.opts.DownloadHostDir); err != nil {
			return fmt.Errorf("检查完整视频失败 %s: %w", filename, err)
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
		cmd := exec.CommandContext(probeCtx, r.opts.FFprobe, "-v", "error", "-show_entries", "format=duration:stream=codec_type,width,height", "-of", "json", filename)
		output, err := cmd.Output()
		cancel()
		if err != nil {
			return fmt.Errorf("ffprobe 检查视频失败 %s", filename)
		}
		var media struct {
			Format struct {
				Duration string `json:"duration"`
			} `json:"format"`
			Streams []struct {
				Type   string `json:"codec_type"`
				Height int    `json:"height"`
			} `json:"streams"`
		}
		if json.Unmarshal(output, &media) != nil {
			return fmt.Errorf("视频信息无效")
		}
		duration, err := strconv.ParseFloat(media.Format.Duration, 64)
		if err != nil || duration < float64(r.opts.MinDuration)/float64(time.Second) {
			return fmt.Errorf("视频时长不足或未知")
		}
		video := false
		for _, s := range media.Streams {
			if s.Type == "video" && s.Height >= minHeight {
				video = true
			}
		}
		if !video {
			return fmt.Errorf("视频画面尺寸不满足要求")
		}
		for _, fraction := range []float64{0.1, 0.5, 0.9} {
			decodeCtx, cancel := context.WithTimeout(ctx, time.Minute)
			err = exec.CommandContext(decodeCtx, r.opts.FFmpeg, "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-ss", fmt.Sprintf("%.3f", duration*fraction), "-i", filename, "-map", "0:v:0", "-frames:v", "1", "-f", "null", "-").Run()
			cancel()
			if err != nil {
				return fmt.Errorf("视频抽样解码失败 %s", filename)
			}
		}
	}
	return nil
}
func (r *torrentRescueRunner) publishWinner(ctx context.Context, g *rescueGroup, live map[string]qbitTorrent) error {
	if r.opts.DryRun {
		return nil
	}
	d := g.Downloads[g.Winner]
	if d == nil || len(d.Files) == 0 {
		return fmt.Errorf("成功候选缺少持久化文件列表")
	}
	t, exists := live[g.Winner]
	if !exists || t.AddedOn != d.AddedOn {
		return fmt.Errorf("成功候选已被删除或重新添加")
	}
	target := filepath.Join(r.opts.PublishDir, "rescue-"+g.Code+"-"+g.Winner[:12])
	if g.PublishPath != target {
		return fmt.Errorf("补救交付路径与配置不符")
	}
	manifest := rescueManifest{Code: g.Code, Hash: g.Winner}
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("交付目标已存在且不是目录")
		}
		data, err := os.ReadFile(filepath.Join(target, ".qbit-rescue.json"))
		var existing rescueManifest
		if err != nil || json.Unmarshal(data, &existing) != nil || existing != manifest {
			return fmt.Errorf("交付目标已存在，身份无法核实")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		var needed uint64
		for _, f := range d.Files {
			needed += uint64(f.Size)
		}
		free, err := r.diskFree(filepath.Dir(r.opts.PublishDir))
		if err != nil {
			return err
		}
		if free < r.opts.MinFree+needed {
			return fmt.Errorf("交付副本的磁盘空间不足")
		}
		if err = os.MkdirAll(r.opts.PublishDir, 0755); err != nil {
			return err
		}
		// A sibling staging directory is outside the watched tree. Rename exposes
		// all files together; the watcher also requires the durable published state.
		stage, err := os.MkdirTemp(filepath.Dir(r.opts.PublishDir), ".qbit-rescue-stage-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		for _, f := range d.Files {
			source, err := r.localDownloadPath(t.SavePath, f.Name)
			if err != nil {
				return err
			}
			info, err := regularRescueFile(source, r.opts.DownloadHostDir)
			if err != nil {
				return err
			}
			if info.Size() != f.Size {
				return fmt.Errorf("完整视频大小与元数据不符")
			}
			destination := filepath.Join(stage, filepath.FromSlash(f.Name))
			if err = os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				return err
			}
			if err = copyRescueVideo(ctx, source, destination); err != nil {
				return err
			}
		}
		if err = writeRescueJSON(filepath.Join(stage, ".qbit-rescue.json"), manifest, 0644); err != nil {
			return err
		}
		if err = filepath.WalkDir(stage, func(p string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return syncJobDirectory(p)
			}
			return nil
		}); err != nil {
			return err
		}
		if err = os.Chmod(stage, 0755); err != nil {
			return err
		}
		if err = os.Rename(stage, target); err != nil {
			return err
		}
		if err = syncJobDirectory(r.opts.PublishDir); err != nil {
			return err
		}
	}
	for _, f := range d.Files {
		info, err := regularRescueFile(filepath.Join(target, filepath.FromSlash(f.Name)), target)
		if err != nil || info.Size() != f.Size {
			return fmt.Errorf("交付目标视频缺失或大小不符")
		}
	}
	d.Stage = "winner"
	g.Stage = "published"
	g.LastError = ""
	if err := r.store.save(); err != nil {
		return err
	}
	stepLog("补救成功交付 %s hash=%s -> %s", g.Code, g.Winner, target)
	return nil
}

func copyRescueVideo(ctx context.Context, source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("创建交付视频失败: %w", err)
	}
	buffer := make([]byte, 1024*1024)
	for {
		if ctx.Err() != nil {
			out.Close()
			return ctx.Err()
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			if _, err = out.Write(buffer[:n]); err != nil {
				out.Close()
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			out.Close()
			return readErr
		}
	}
	if err = out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func rescuePublicationReady(target string, cfg torrentRescueConfig) (bool, error) {
	if !cfg.Enabled {
		return true, nil
	}
	var data []byte
	info, err := os.Stat(target)
	if err == nil && info.IsDir() {
		data, err = os.ReadFile(filepath.Join(target, ".qbit-rescue.json"))
	} else if err == nil {
		err = os.ErrNotExist
	}
	if os.IsNotExist(err) {
		// The old completion script may already have begun copying when a group
		// was claimed. Keep such copies for inspection; only the manifest-backed
		// winner is allowed into packaging for a managed catalog number.
		parsed, ok := preprocessCatalogNumber(target)
		if !ok {
			return true, nil
		}
		state, err := readRescuePublicationState(cfg.StateDir)
		if os.IsNotExist(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		_, managed := state.Groups[parsed.Code]
		return !managed, nil
	}
	if err != nil {
		return false, err
	}
	var manifest rescueManifest
	if json.Unmarshal(data, &manifest) != nil || !validRescueCode(manifest.Code) || !validTorrentHash(manifest.Hash) {
		return false, fmt.Errorf("补救交付清单无效")
	}
	state, err := readRescuePublicationState(cfg.StateDir)
	if err != nil {
		return false, err
	}
	g := state.Groups[manifest.Code]
	return g != nil && g.Stage == "published" && g.Winner == manifest.Hash && filepath.Clean(g.PublishPath) == filepath.Clean(target), nil
}

func readRescuePublicationState(stateDir string) (rescueState, error) {
	var state rescueState
	if stateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return state, err
		}
		stateDir = filepath.Join(base, "qbit-upload", "torrent-rescue")
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return state, err
	}
	if json.Unmarshal(data, &state) != nil {
		return state, fmt.Errorf("补救交付状态无效")
	}
	return state, nil
}
