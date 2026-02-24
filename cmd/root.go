package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const mb = 1024 * 1024

var (
	destDir   string
	password  string
	sevenZip  string
	minSizeMB int64
	dryRun    bool
	config    string
)

var runtimeLogOutput io.Writer = os.Stdout

func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "qbit-upload <source-dir>",
		Short: "过滤视频并用 7z 加密压缩后移动到目标目录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, args[0])
		},
	}

	cmd.Flags().StringVarP(&destDir, "dest", "d", "", "压缩包输出目录（可选，默认程序所在目录）")
	cmd.Flags().StringVarP(&password, "password", "p", "", "7z 加密密码（可从配置文件读取）")
	cmd.Flags().StringVar(&sevenZip, "7z", "7z", "7z 可执行文件路径")
	cmd.Flags().Int64Var(&minSizeMB, "min-size-mb", 50, "最小视频大小（MB）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "仅打印将执行的操作，不实际压缩/移动/删除")
	cmd.Flags().StringVar(&config, "config", "", "配置文件路径（支持 .yaml/.yml/.json）")

	return cmd
}

func run(cmd *cobra.Command, sourceDir string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logFilePath, err := initLogging(cfg)
	if err != nil {
		return err
	}

	stepLog("开始执行任务")
	stepLog("日志文件: %s", logFilePath)

	stepLog("加载参数与配置")
	opts, err := resolveOptions(cmd, cfg)
	if err != nil {
		return err
	}

	stepLog("解析源目录: %s", sourceDir)
	absSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("解析源目录失败: %w", err)
	}

	stepLog("检查源目录是否存在")
	info, err := os.Stat(absSource)
	if err != nil {
		return fmt.Errorf("读取源目录失败: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("不是目录: %s", absSource)
	}

	stepLog("解析目标目录: %s", opts.DestDir)
	absDest, err := filepath.Abs(opts.DestDir)
	if err != nil {
		return fmt.Errorf("解析目标目录失败: %w", err)
	}
	stepLog("确保目标目录存在")
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	stepLog("扫描视频文件（最小大小: %dMB）", opts.MinSizeMB)
	minSizeBytes := opts.MinSizeMB * mb
	videoFiles, err := collectEligibleVideos(absSource, minSizeBytes)
	if err != nil {
		return err
	}
	if len(videoFiles) == 0 {
		return fmt.Errorf("目录中没有大于等于 %dMB 的视频文件", opts.MinSizeMB)
	}
	stepLog("扫描完成，命中视频文件: %d 个", len(videoFiles))

	archiveName := filepath.Base(absSource) + ".7z"
	finalArchive := filepath.Join(absDest, archiveName)
	stepLog("检查目标压缩包是否冲突: %s", finalArchive)
	if _, err := os.Stat(finalArchive); err == nil {
		return fmt.Errorf("目标目录已存在同名压缩包: %s", finalArchive)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查目标压缩包失败: %w", err)
	}

	if opts.DryRun {
		fmt.Printf("[dry-run] 源目录: %s\n", absSource)
		fmt.Printf("[dry-run] 将压缩视频数量: %d\n", len(videoFiles))
		for _, f := range videoFiles {
			fmt.Printf("[dry-run]   - %s\n", f)
		}
		fmt.Printf("[dry-run] 目标压缩包: %s\n", finalArchive)
		fmt.Printf("[dry-run] 将删除目录: %s\n", absSource)
		stepLog("dry-run 完成")
		return nil
	}

	tempArchive := filepath.Join(os.TempDir(), fmt.Sprintf("%s_%d.7z", filepath.Base(absSource), time.Now().UnixNano()))
	stepLog("临时压缩包路径: %s", tempArchive)
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(tempArchive)
		}
	}()

	stepLog("开始调用 7z 压缩（将实时输出 7z 日志）")
	if err := compressWith7z(absSource, tempArchive, videoFiles, opts.SevenZip, opts.Password); err != nil {
		return err
	}
	stepLog("7z 压缩完成")

	stepLog("移动压缩包到目标目录")
	if err := moveFile(tempArchive, finalArchive); err != nil {
		return fmt.Errorf("移动压缩包失败: %w", err)
	}
	moved = true

	stepLog("删除源目录")
	if err := os.RemoveAll(absSource); err != nil {
		return fmt.Errorf("删除源目录失败: %w", err)
	}

	stepLog("任务完成")
	fmt.Printf("已完成: %d 个视频 -> %s\n", len(videoFiles), finalArchive)
	return nil
}

type appConfig struct {
	DestDir   string    `json:"dest_dir" yaml:"dest_dir"`
	Password  string    `json:"password" yaml:"password"`
	SevenZip  string    `json:"seven_zip" yaml:"seven_zip"`
	MinSizeMB int64     `json:"min_size_mb" yaml:"min_size_mb"`
	Log       logConfig `json:"log" yaml:"log"`
}

type logConfig struct {
	Path       string `json:"path" yaml:"path"`
	AlsoStdout *bool  `json:"also_stdout" yaml:"also_stdout"`
}

type runOptions struct {
	DestDir   string
	Password  string
	SevenZip  string
	MinSizeMB int64
	DryRun    bool
}

func resolveOptions(cmd *cobra.Command, cfg appConfig) (runOptions, error) {
	dest := cfg.DestDir
	if cmd.Flags().Changed("dest") {
		dest = destDir
	}
	if strings.TrimSpace(dest) == "" {
		execPath, err := os.Executable()
		if err != nil {
			return runOptions{}, fmt.Errorf("获取程序路径失败: %w", err)
		}
		dest = filepath.Dir(execPath)
	}

	pwd := cfg.Password
	if cmd.Flags().Changed("password") {
		pwd = password
	}

	seven := cfg.SevenZip
	if seven == "" {
		seven = "7z"
	}
	if cmd.Flags().Changed("7z") {
		seven = sevenZip
	}

	minMB := cfg.MinSizeMB
	if minMB <= 0 {
		minMB = 50
	}
	if cmd.Flags().Changed("min-size-mb") {
		minMB = minSizeMB
	}

	if !dryRun && strings.TrimSpace(pwd) == "" {
		return runOptions{}, fmt.Errorf("密码不能为空：请通过 --password 或配置文件提供")
	}
	if minMB <= 0 {
		return runOptions{}, fmt.Errorf("min-size-mb 必须大于 0")
	}

	return runOptions{
		DestDir:   dest,
		Password:  pwd,
		SevenZip:  seven,
		MinSizeMB: minMB,
		DryRun:    dryRun,
	}, nil
}

func loadConfig() (appConfig, error) {
	path := strings.TrimSpace(config)
	if path == "" {
		execPath, err := os.Executable()
		if err != nil {
			return appConfig{}, fmt.Errorf("获取程序路径失败: %w", err)
		}
		execDir := filepath.Dir(execPath)
		candidates := []string{
			filepath.Join(execDir, "qbit-upload.yaml"),
			filepath.Join(execDir, "qbit-upload.yml"),
			filepath.Join(execDir, "qbit-upload.json"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
		if path == "" {
			return appConfig{}, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return appConfig{}, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg appConfig
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return appConfig{}, fmt.Errorf("解析 YAML 配置失败: %w", err)
		}
	case ".json":
		if err := json.Unmarshal(data, &cfg); err != nil {
			return appConfig{}, fmt.Errorf("解析 JSON 配置失败: %w", err)
		}
	default:
		return appConfig{}, fmt.Errorf("不支持的配置文件格式: %s", ext)
	}

	return cfg, nil
}

func initLogging(cfg appConfig) (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取程序路径失败: %w", err)
	}
	execDir := filepath.Dir(execPath)

	logDir := strings.TrimSpace(cfg.Log.Path)
	if logDir == "" {
		logDir = filepath.Join(execDir, "logs")
	} else if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(execDir, logDir)
	}
	if ext := strings.ToLower(filepath.Ext(logDir)); ext == ".log" || ext == ".txt" {
		logDir = filepath.Dir(logDir)
	}

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("创建日志目录失败: %w", err)
	}
	logFileName := fmt.Sprintf("qbit-upload-%s.log", time.Now().Format("20060102-150405"))
	logPath := filepath.Join(logDir, logFileName)

	fileWriter, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("创建日志文件失败: %w", err)
	}

	alsoStdout := true
	if cfg.Log.AlsoStdout != nil {
		alsoStdout = *cfg.Log.AlsoStdout
	}

	if alsoStdout {
		runtimeLogOutput = io.MultiWriter(os.Stdout, fileWriter)
	} else {
		runtimeLogOutput = fileWriter
	}
	log.SetOutput(runtimeLogOutput)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	return logPath, nil
}

func collectEligibleVideos(root string, minSizeBytes int64) ([]string, error) {
	videoFiles := make([]string, 0, 16)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() < minSizeBytes {
			return nil
		}

		ok, err := isVideo(path)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		videoFiles = append(videoFiles, rel)
		stepLog("命中视频: %s (%.2f MB)", rel, float64(info.Size())/mb)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("扫描目录失败: %w", err)
	}
	return videoFiles, nil
}

func isVideo(path string) (bool, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := videoExts[ext]; ok {
		return true, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	header := make([]byte, 512)
	n, err := f.Read(header)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}

	contentType := http.DetectContentType(header[:n])
	return strings.HasPrefix(contentType, "video/"), nil
}

func compressWith7z(sourceDir, outArchive string, files []string, sevenZipPath, archivePassword string) error {
	args := []string{
		"a",
		"-t7z",
		"-mx=9",
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

func stepLog(format string, args ...any) {
	log.Printf("[step] "+format, args...)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		_ = in.Close()
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = in.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = in.Close()
		return err
	}

	// Windows 下删除源文件前必须先关闭句柄。
	if err := out.Close(); err != nil {
		_ = in.Close()
		return err
	}
	if err := in.Close(); err != nil {
		return err
	}

	for i := 0; i < 5; i++ {
		if err := os.Remove(src); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("复制完成但删除源临时文件失败: %s", src)
}

var videoExts = map[string]struct{}{
	".3gp":  {},
	".asf":  {},
	".avi":  {},
	".flv":  {},
	".m2ts": {},
	".m4v":  {},
	".mkv":  {},
	".mov":  {},
	".mp4":  {},
	".mpeg": {},
	".mpg":  {},
	".mts":  {},
	".rm":   {},
	".rmvb": {},
	".ts":   {},
	".vob":  {},
	".webm": {},
	".wmv":  {},
}
