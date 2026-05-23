package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func runWatch(cmd *cobra.Command) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logFilePath, err := initLogging(cfg)
	if err != nil {
		return err
	}
	stepLog("开始监听任务")
	stepLog("日志文件: %s", logFilePath)

	opts, err := resolveOptions(cmd, cfg)
	if err != nil {
		return err
	}
	watchOpts, err := resolveWatchOptions(cfg.Watch)
	if err != nil {
		return err
	}
	watchOpts.Enabled = true
	if len(watchOpts.Dirs) == 0 {
		return fmt.Errorf("监听目录不能为空：请在配置 watch.dirs 中指定")
	}

	absDirs := make([]string, 0, len(watchOpts.Dirs))
	for _, dir := range watchOpts.Dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("解析监听目录失败 %s: %w", dir, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("读取监听目录失败 %s: %w", abs, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("监听路径不是目录: %s", abs)
		}
		absDirs = append(absDirs, abs)
		stepLog("监听目录: %s", abs)
	}

	processed := make(map[string]struct{})
	for {
		for _, dir := range absDirs {
			candidates, err := scanWatchCandidates(dir, opts.MinSizeMB*mb)
			if err != nil {
				stepLog("WARN: 扫描监听目录失败 %s: %v", dir, err)
				continue
			}
			for _, candidate := range candidates {
				target, err := watchedProcessingPath(dir, candidate)
				if err != nil {
					stepLog("WARN: 解析监听目标失败 %s: %v", candidate, err)
					continue
				}
				if _, ok := processed[target]; ok {
					continue
				}
				if !isStableFile(candidate, watchOpts.StableDelay) {
					continue
				}
				stepLog("发现稳定视频，开始处理: %s", target)
				if err := processSource(target, opts); err != nil {
					stepLog("ERROR: 自动处理失败 %s: %v", target, err)
					continue
				}
				processed[target] = struct{}{}
			}
		}
		time.Sleep(watchOpts.PollInterval)
	}
}

func scanWatchCandidates(watchDir string, minSizeBytes int64) ([]string, error) {
	var candidates []string
	err := filepath.WalkDir(watchDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() < minSizeBytes {
			return nil
		}
		ok, err := isVideo(path)
		if err != nil || !ok {
			return nil
		}
		candidates = append(candidates, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

func watchedProcessingPath(watchDir, candidate string) (string, error) {
	absWatch, err := filepath.Abs(watchDir)
	if err != nil {
		return "", err
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absWatch, absCandidate)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("候选路径不在监听目录内: %s", absCandidate)
	}

	parts := splitPathParts(rel)
	if len(parts) <= 1 {
		return absCandidate, nil
	}
	return filepath.Join(absWatch, parts[0]), nil
}

func splitPathParts(path string) []string {
	clean := filepath.Clean(path)
	if clean == "." {
		return nil
	}
	return strings.Split(clean, string(os.PathSeparator))
}

func isStableFile(path string, delay time.Duration) bool {
	first, err := os.Stat(path)
	if err != nil || first.IsDir() {
		return false
	}
	time.Sleep(delay)
	second, err := os.Stat(path)
	if err != nil || second.IsDir() {
		return false
	}
	return first.Size() == second.Size() && first.ModTime().Equal(second.ModTime())
}
