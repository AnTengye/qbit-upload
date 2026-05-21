package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type archiveFormat string

const (
	archiveFormat7z  archiveFormat = "7z"
	archiveFormatTgz archiveFormat = "tgz"
)

type archiveResult struct {
	Format       archiveFormat
	Path         string
	UsedFallback bool
}

type archiveOutput struct {
	Path  string
	Files []string
}

func discoverSevenZip(explicitPath, execDir, embeddedDir string, lookPath func(string) (string, error)) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if strings.TrimSpace(explicitPath) != "" {
		if !strings.ContainsAny(explicitPath, `/\`) {
			found, err := lookPath(explicitPath)
			if err != nil {
				return "", fmt.Errorf("7z executable not found in PATH: %w", err)
			}
			return found, nil
		}
		if isExecutableFile(explicitPath) {
			return explicitPath, nil
		}
		return "", fmt.Errorf("7z executable not found or not executable: %s", explicitPath)
	}
	if strings.TrimSpace(embeddedDir) == "" {
		embeddedDir = "tools"
	}

	names := preferredSevenZipNames()
	for _, name := range names {
		candidates := []string{
			filepath.Join(execDir, embeddedDir, runtime.GOOS+"-"+runtime.GOARCH, name),
			filepath.Join(execDir, name),
		}
		for _, candidate := range candidates {
			if isExecutableFile(candidate) {
				return candidate, nil
			}
		}
	}

	var lastErr error
	for _, name := range preferredSevenZipNames() {
		found, err := lookPath(name)
		if err == nil && strings.TrimSpace(found) != "" {
			return found, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return "", fmt.Errorf("7z/7zz not found: %w", lastErr)
	}
	return "", fmt.Errorf("7z/7zz not found")
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func preferredSevenZipNames() []string {
	if runtime.GOOS == "windows" {
		return []string{executableName("7z"), executableName("7zz")}
	}
	return []string{executableName("7zz"), executableName("7z")}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}

func createTgzArchive(sourceDir, outArchive string, files []string) error {
	out, err := os.Create(outArchive)
	if err != nil {
		return fmt.Errorf("创建 tgz 文件失败: %w", err)
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	for _, rel := range files {
		cleanRel := filepath.Clean(rel)
		if filepath.IsAbs(cleanRel) || cleanRel == "." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) || cleanRel == ".." {
			return fmt.Errorf("非法归档路径: %s", rel)
		}

		src := filepath.Join(sourceDir, cleanRel)
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("读取待归档文件失败 %s: %w", src, err)
		}
		if info.IsDir() {
			return fmt.Errorf("待归档路径不是文件: %s", src)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("创建 tar 头失败 %s: %w", src, err)
		}
		hdr.Name = filepath.ToSlash(cleanRel)
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("写入 tar 头失败 %s: %w", src, err)
		}

		in, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("打开待归档文件失败 %s: %w", src, err)
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			return fmt.Errorf("写入 tgz 内容失败 %s: %w", src, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭待归档文件失败 %s: %w", src, closeErr)
		}
	}
	return nil
}

func archiveFileName(sourceBase string, format archiveFormat) string {
	return sourceBase + "." + string(format)
}

func archivePathWithFormat(path string, format archiveFormat) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "." + string(format)
}

func planArchiveOutputs(sourceBase, destDir string, files []string, format archiveFormat, split bool) ([]archiveOutput, error) {
	if !split {
		return []archiveOutput{{
			Path:  filepath.Join(destDir, archiveFileName(sourceBase, format)),
			Files: append([]string(nil), files...),
		}}, nil
	}

	outputs := make([]archiveOutput, 0, len(files))
	seen := make(map[string]int, len(files))
	for _, rel := range files {
		base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		if strings.TrimSpace(base) == "" {
			base = "video"
		}

		key := base + "." + string(format)
		seen[key]++
		nameBase := base
		if seen[key] > 1 {
			nameBase = fmt.Sprintf("%s-%d", base, seen[key])
		}

		outputs = append(outputs, archiveOutput{
			Path:  filepath.Join(destDir, archiveFileName(nameBase, format)),
			Files: []string{rel},
		})
	}
	return outputs, nil
}

func compressWith7z(sourceDir, outArchive string, files []string, sevenZipPath, archivePassword string, reserveMemoryMB int64) error {
	availMB := getAvailableMemoryMB()
	dictSize, threads := calcSevenZipParams(availMB, reserveMemoryMB, runtime.NumCPU())

	if availMB > 0 {
		var budgetMB int64
		if availMB > uint64(reserveMemoryMB) {
			budgetMB = int64(availMB) - reserveMemoryMB
		}
		stepLog("内存决策: 可用 %d MiB，保留 %d MiB，预算 %d MiB → dict=%dm threads=%d（预计用量 %.0f MiB）",
			availMB, reserveMemoryMB, budgetMB,
			dictSize, threads,
			float64(dictSize)*memCoeffPerThread*float64(threads))
	} else {
		stepLog("内存决策: 无法获取系统内存，使用默认参数 → dict=%dm threads=%d", dictSize, threads)
	}

	args := buildSevenZipArgs(outArchive, files, archivePassword, dictSize, threads)

	cmd := exec.Command(sevenZipPath, args...)
	cmd.Dir = sourceDir
	var buf bytes.Buffer
	progress := newProgressSnapshot(4096)
	mw := io.MultiWriter(runtimeLogOutput, &buf, progress)
	cmd.Stdout = mw
	cmd.Stderr = mw

	stepLog("执行命令: %s %s", sevenZipPath, strings.Join(args, " "))
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("启动 7z 失败: %w", err)
	}

	stopHeartbeat := startProgressHeartbeat("7z 压缩", progress, 10*time.Minute)
	err = cmd.Wait()
	stopHeartbeat()
	if err != nil {
		return fmt.Errorf("7z 压缩失败: %w\n%s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}

func buildSevenZipArgs(outArchive string, files []string, archivePassword string, dictSize, threads int) []string {
	args := []string{
		"a",
		"-t7z",
		"-mx=9",
		fmt.Sprintf("-md=%dm", dictSize),
		fmt.Sprintf("-mmt=%d", threads),
		"-mhe=on",
		"-bsp1",
		"-p" + archivePassword,
		outArchive,
	}
	args = append(args, files...)
	return args
}

func startProgressHeartbeat(label string, progress *progressSnapshot, interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				stepLog("INFO: %s仍在运行，最近输出: %s", label, progress.Latest())
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
	}
}
