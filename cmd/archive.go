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

	names := []string{executableName("7z"), executableName("7zz")}
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
	for _, name := range []string{"7z", "7zz"} {
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

	args := []string{
		"a",
		"-t7z",
		"-mx=9",
		fmt.Sprintf("-md=%dm", dictSize),
		fmt.Sprintf("-mmt=%d", threads),
		"-mhe=on",
		"-p" + archivePassword,
		outArchive,
	}
	args = append(args, files...)

	cmd := exec.Command(sevenZipPath, args...)
	cmd.Dir = sourceDir
	var buf bytes.Buffer
	mw := io.MultiWriter(runtimeLogOutput, &buf)
	cmd.Stdout = mw
	cmd.Stderr = mw

	stepLog("执行命令: %s %s", sevenZipPath, strings.Join(args, " "))
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("7z 压缩失败: %w\n%s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}
