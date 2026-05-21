package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const mb = 1024 * 1024

var (
	destDir          string
	password         string
	sevenZip         string
	minSizeMB        int64
	reserveMemoryMB  int64
	allowTgzFallback bool
	embedded7zDir    string
	splitArchive     bool
	thumbnailEnabled bool
	ffmpegPath       string
	ffprobePath      string
	thumbnailColumns int
	thumbnailRows    int
	thumbnailWidth   int
	dryRun           bool
	config           string
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

	flags := cmd.PersistentFlags()
	flags.StringVarP(&destDir, "dest", "d", "", "压缩包输出目录（可选，默认程序所在目录）")
	flags.StringVarP(&password, "password", "p", "", "7z 加密密码（可从配置文件读取）")
	flags.StringVar(&sevenZip, "7z", "7z", "7z 可执行文件路径")
	flags.Int64Var(&minSizeMB, "min-size-mb", 50, "最小视频大小（MB）")
	flags.Int64Var(&reserveMemoryMB, "reserve-memory-mb", 2048, "压缩时系统至少保留的可用物理内存(MB)")
	flags.BoolVar(&allowTgzFallback, "allow-tgz-fallback", true, "7z 不可用或失败时生成未加密 .tgz")
	flags.StringVar(&embedded7zDir, "embedded-7z-dir", "tools", "内嵌 7z 工具目录（相对程序目录）")
	flags.BoolVar(&splitArchive, "split", false, "每个视频文件单独生成一个压缩包")
	flags.BoolVar(&thumbnailEnabled, "thumbnail", true, "生成视频缩略图长图")
	flags.StringVar(&ffmpegPath, "ffmpeg", "ffmpeg", "ffmpeg 可执行文件路径")
	flags.StringVar(&ffprobePath, "ffprobe", "ffprobe", "ffprobe 可执行文件路径")
	flags.IntVar(&thumbnailColumns, "thumbnail-columns", 4, "缩略图列数")
	flags.IntVar(&thumbnailRows, "thumbnail-rows", 15, "缩略图行数")
	flags.IntVar(&thumbnailWidth, "thumbnail-width", 320, "单张缩略图宽度")
	flags.BoolVar(&dryRun, "dry-run", false, "仅打印将执行的操作，不实际压缩/移动/删除")
	flags.StringVar(&config, "config", "", "配置文件路径（支持 .yaml/.yml/.json）")

	cmd.AddCommand(newThumbnailCmd())

	return cmd
}

func newThumbnailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "thumbnail <source-dir>",
		Short: "仅生成视频缩略图长图，不压缩或删除源目录",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThumbnailOnly(cmd, args[0])
		},
	}
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

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	execDir := filepath.Dir(execPath)
	stepLog("发现 7z 可执行文件")
	sevenZipPath, sevenZipErr := discoverSevenZip(opts.SevenZip, execDir, opts.Archive.Embedded7zDir, nil)
	plannedFormat := archiveFormat7z
	if sevenZipErr != nil {
		if !opts.Archive.AllowTgzFallback {
			return sevenZipErr
		}
		plannedFormat = archiveFormatTgz
		stepLog("INFO: 7z 不可用，将使用未加密 tgz 兜底: %v", sevenZipErr)
	}

	archiveOutputs, err := planArchiveOutputs(filepath.Base(absSource), absDest, videoFiles, plannedFormat, opts.Archive.Split)
	if err != nil {
		return err
	}
	stepLog("检查目标压缩包是否冲突: %d 个", len(archiveOutputs))
	for _, out := range archiveOutputs {
		if err := ensureOutputDoesNotExist(out.Path); err != nil {
			return err
		}
	}

	if opts.DryRun {
		fmt.Printf("[dry-run] 源目录: %s\n", absSource)
		fmt.Printf("[dry-run] 将压缩视频数量: %d\n", len(videoFiles))
		for _, f := range videoFiles {
			fmt.Printf("[dry-run]   - %s\n", f)
		}
		fmt.Printf("[dry-run] 目标压缩包数量: %d\n", len(archiveOutputs))
		for _, out := range archiveOutputs {
			fmt.Printf("[dry-run]   - %s\n", out.Path)
		}
		if plannedFormat == archiveFormat7z {
			fmt.Printf("[dry-run] 压缩格式: 7z (%s)\n", sevenZipPath)
		} else {
			fmt.Printf("[dry-run] 压缩格式: tgz（未加密兜底）\n")
		}
		if opts.Thumbnail.Enabled {
			thumbs, err := planThumbnailOutputs(filepath.Base(absSource), absDest, videoFiles)
			if err != nil {
				return err
			}
			fmt.Printf("[dry-run] 将生成缩略图数量: %d\n", len(thumbs))
			for _, thumb := range thumbs {
				fmt.Printf("[dry-run]   - %s\n", thumb.OutputPath)
			}
		}
		fmt.Printf("[dry-run] 将删除目录: %s\n", absSource)
		stepLog("dry-run 完成")
		return nil
	}

	if opts.Thumbnail.Enabled {
		stepLog("开始生成视频缩略图")
		if err := generateThumbnails(absSource, filepath.Base(absSource), absDest, videoFiles, opts.Thumbnail); err != nil {
			return err
		}
	}

	finalArchives := make([]string, 0, len(archiveOutputs))
	for i, out := range archiveOutputs {
		format := plannedFormat
		finalArchive := out.Path
		tempArchive := filepath.Join(os.TempDir(), fmt.Sprintf("%s_%d_%d.%s", filepath.Base(absSource), time.Now().UnixNano(), i, format))
		stepLog("临时压缩包路径: %s", tempArchive)
		moved := false
		defer func() {
			if !moved {
				_ = os.Remove(tempArchive)
			}
		}()

		if format == archiveFormat7z {
			if strings.TrimSpace(opts.Password) == "" {
				return fmt.Errorf("密码不能为空：使用 7z 时请通过 --password 或配置文件提供")
			}
			stepLog("开始调用 7z 压缩（将实时输出 7z 日志）")
			if err := compressWith7z(absSource, tempArchive, out.Files, sevenZipPath, opts.Password, opts.ReserveMemoryMB); err != nil {
				if !opts.Archive.AllowTgzFallback {
					return err
				}
				stepLog("INFO: 7z 压缩失败，将使用未加密 tgz 兜底: %v", err)
				_ = os.Remove(tempArchive)
				format = archiveFormatTgz
				finalArchive = archivePathWithFormat(out.Path, format)
				if err := ensureOutputDoesNotExist(finalArchive); err != nil {
					return err
				}
				tempArchive = filepath.Join(os.TempDir(), fmt.Sprintf("%s_%d_%d.%s", filepath.Base(absSource), time.Now().UnixNano(), i, format))
				stepLog("临时压缩包路径: %s", tempArchive)
				if err := createTgzArchive(absSource, tempArchive, out.Files); err != nil {
					return err
				}
			}
		} else {
			stepLog("开始创建未加密 tgz 兜底压缩包")
			if err := createTgzArchive(absSource, tempArchive, out.Files); err != nil {
				return err
			}
		}
		stepLog("压缩完成: %s", format)

		stepLog("移动压缩包到目标目录")
		if err := moveFile(tempArchive, finalArchive); err != nil {
			return fmt.Errorf("移动压缩包失败: %w", err)
		}
		moved = true
		finalArchives = append(finalArchives, finalArchive)
	}

	stepLog("删除源目录")
	if err := os.RemoveAll(absSource); err != nil {
		return fmt.Errorf("删除源目录失败: %w", err)
	}

	stepLog("任务完成")
	fmt.Printf("已完成: %d 个视频 -> %d 个压缩包\n", len(videoFiles), len(finalArchives))
	for _, finalArchive := range finalArchives {
		fmt.Printf("  - %s\n", finalArchive)
	}
	return nil
}

func runThumbnailOnly(cmd *cobra.Command, sourceDir string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	logFilePath, err := initLogging(cfg)
	if err != nil {
		return err
	}

	stepLog("开始执行缩略图任务")
	stepLog("日志文件: %s", logFilePath)

	opts, err := resolveOptions(cmd, cfg)
	if err != nil {
		return err
	}
	if !isFlagChanged(cmd, "thumbnail") {
		opts.Thumbnail.Enabled = true
	}

	absSource, absDest, videoFiles, err := prepareVideoInputs(sourceDir, opts)
	if err != nil {
		return err
	}

	if opts.DryRun {
		fmt.Printf("[dry-run] 源目录: %s\n", absSource)
		thumbs, err := planThumbnailOutputs(filepath.Base(absSource), absDest, videoFiles)
		if err != nil {
			return err
		}
		fmt.Printf("[dry-run] 将生成缩略图数量: %d\n", len(thumbs))
		for _, thumb := range thumbs {
			fmt.Printf("[dry-run]   - %s\n", thumb.OutputPath)
		}
		stepLog("缩略图 dry-run 完成")
		return nil
	}

	if !opts.Thumbnail.Enabled {
		stepLog("缩略图已禁用，跳过")
		return nil
	}
	if err := generateThumbnails(absSource, filepath.Base(absSource), absDest, videoFiles, opts.Thumbnail); err != nil {
		return err
	}

	stepLog("缩略图任务完成")
	fmt.Printf("缩略图已完成: %d 个视频 -> %s\n", len(videoFiles), absDest)
	return nil
}

func prepareVideoInputs(sourceDir string, opts runOptions) (absSource, absDest string, videoFiles []string, err error) {
	stepLog("解析源目录: %s", sourceDir)
	absSource, err = filepath.Abs(sourceDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("解析源目录失败: %w", err)
	}

	stepLog("检查源目录是否存在")
	info, err := os.Stat(absSource)
	if err != nil {
		return "", "", nil, fmt.Errorf("读取源目录失败: %w", err)
	}
	if !info.IsDir() {
		return "", "", nil, fmt.Errorf("不是目录: %s", absSource)
	}

	stepLog("解析目标目录: %s", opts.DestDir)
	absDest, err = filepath.Abs(opts.DestDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("解析目标目录失败: %w", err)
	}
	stepLog("确保目标目录存在")
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return "", "", nil, fmt.Errorf("创建目标目录失败: %w", err)
	}

	stepLog("扫描视频文件（最小大小: %dMB）", opts.MinSizeMB)
	minSizeBytes := opts.MinSizeMB * mb
	videoFiles, err = collectEligibleVideos(absSource, minSizeBytes)
	if err != nil {
		return "", "", nil, err
	}
	if len(videoFiles) == 0 {
		return "", "", nil, fmt.Errorf("目录中没有大于等于 %dMB 的视频文件", opts.MinSizeMB)
	}
	stepLog("扫描完成，命中视频文件: %d 个", len(videoFiles))
	return absSource, absDest, videoFiles, nil
}

type appConfig struct {
	DestDir         string          `json:"dest_dir" yaml:"dest_dir"`
	Password        string          `json:"password" yaml:"password"`
	SevenZip        string          `json:"seven_zip" yaml:"seven_zip"`
	MinSizeMB       int64           `json:"min_size_mb" yaml:"min_size_mb"`
	ReserveMemoryMB int64           `json:"reserve_memory_mb" yaml:"reserve_memory_mb"`
	Archive         archiveConfig   `json:"archive" yaml:"archive"`
	Thumbnail       thumbnailConfig `json:"thumbnail" yaml:"thumbnail"`
	Log             logConfig       `json:"log" yaml:"log"`
}

type archiveConfig struct {
	AllowTgzFallback *bool  `json:"allow_tgz_fallback" yaml:"allow_tgz_fallback"`
	Embedded7zDir    string `json:"embedded_7z_dir" yaml:"embedded_7z_dir"`
	Split            *bool  `json:"split" yaml:"split"`
}

type logConfig struct {
	Path       string `json:"path" yaml:"path"`
	AlsoStdout *bool  `json:"also_stdout" yaml:"also_stdout"`
}

type runOptions struct {
	DestDir         string
	Password        string
	SevenZip        string
	MinSizeMB       int64
	ReserveMemoryMB int64
	Archive         archiveOptions
	Thumbnail       thumbnailOptions
	DryRun          bool
}

type archiveOptions struct {
	AllowTgzFallback bool
	Embedded7zDir    string
	Split            bool
}

func resolveOptions(cmd *cobra.Command, cfg appConfig) (runOptions, error) {
	dest := cfg.DestDir
	if isFlagChanged(cmd, "dest") {
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
	if isFlagChanged(cmd, "password") {
		pwd = password
	}

	seven := cfg.SevenZip
	if isFlagChanged(cmd, "7z") {
		seven = sevenZip
	}

	minMB := cfg.MinSizeMB
	if minMB <= 0 {
		minMB = 50
	}
	if isFlagChanged(cmd, "min-size-mb") {
		minMB = minSizeMB
	}

	reserveMB := cfg.ReserveMemoryMB
	if reserveMB <= 0 {
		reserveMB = 2048
	}
	if isFlagChanged(cmd, "reserve-memory-mb") {
		reserveMB = reserveMemoryMB
	}

	if minMB <= 0 {
		return runOptions{}, fmt.Errorf("min-size-mb 必须大于 0")
	}

	archive := archiveOptions{
		AllowTgzFallback: true,
		Embedded7zDir:    "tools",
	}
	if cfg.Archive.AllowTgzFallback != nil {
		archive.AllowTgzFallback = *cfg.Archive.AllowTgzFallback
	}
	if strings.TrimSpace(cfg.Archive.Embedded7zDir) != "" {
		archive.Embedded7zDir = cfg.Archive.Embedded7zDir
	}
	if isFlagChanged(cmd, "allow-tgz-fallback") {
		archive.AllowTgzFallback = allowTgzFallback
	}
	if isFlagChanged(cmd, "embedded-7z-dir") {
		archive.Embedded7zDir = embedded7zDir
	}
	if cfg.Archive.Split != nil {
		archive.Split = *cfg.Archive.Split
	}
	if isFlagChanged(cmd, "split") {
		archive.Split = splitArchive
	}

	thumbCfg := cfg.Thumbnail
	if isFlagChanged(cmd, "thumbnail") {
		thumbCfg.Enabled = &thumbnailEnabled
	}
	if isFlagChanged(cmd, "ffmpeg") {
		thumbCfg.FFmpeg = ffmpegPath
	}
	if isFlagChanged(cmd, "ffprobe") {
		thumbCfg.FFprobe = ffprobePath
	}
	if isFlagChanged(cmd, "thumbnail-columns") {
		thumbCfg.Columns = thumbnailColumns
	}
	if isFlagChanged(cmd, "thumbnail-rows") {
		thumbCfg.Rows = thumbnailRows
	}
	if isFlagChanged(cmd, "thumbnail-width") {
		thumbCfg.Width = thumbnailWidth
	}
	thumb, err := resolveThumbnailOptions(thumbCfg)
	if err != nil {
		return runOptions{}, err
	}

	return runOptions{
		DestDir:         dest,
		Password:        pwd,
		SevenZip:        seven,
		MinSizeMB:       minMB,
		ReserveMemoryMB: reserveMB,
		Archive:         archive,
		Thumbnail:       thumb,
		DryRun:          dryRun,
	}, nil
}

func isFlagChanged(cmd *cobra.Command, name string) bool {
	flag := cmd.Flag(name)
	return flag != nil && flag.Changed
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

// dictSizeLevels holds candidate LZMA2 dictionary sizes (MB) in descending order.
// Values are powers of two as recommended by 7-Zip documentation.
var dictSizeLevels = []int{256, 128, 64, 32, 16, 8, 4, 2, 1}

// memCoeffPerThread is the empirical multiplier for LZMA2 compressor memory usage.
// Each thread needs approximately dictSizeMB * memCoeffPerThread MB of RAM.
const memCoeffPerThread = 11.5

// calcSevenZipParams returns the best (dictSizeMB, threads) combination that fits
// within the available memory budget.
//
//   - availMB: free physical RAM reported by the OS (0 = unknown, skip budgeting)
//   - reserveMB: MB to keep reserved for the OS and other processes
//   - cpuCores: logical CPU count
//
// The algorithm picks the largest dict size from dictSizeLevels such that
//
//	dictSizeMB * memCoeffPerThread * threads <= budgetMB
//
// If no combination fits even with threads=1, it falls back to dictSizeMB=1, threads=1.
func calcSevenZipParams(availMB uint64, reserveMB int64, cpuCores int) (dictSizeMB, threads int) {
	// Default thread count: leave 2 cores free for the OS.
	maxThreads := cpuCores - 2
	if maxThreads < 1 {
		maxThreads = 1
	}

	// If we cannot determine available memory, use CPU-only defaults.
	if availMB == 0 {
		return 64, maxThreads
	}

	var budgetMB int
	if availMB > uint64(reserveMB) {
		budgetMB = int(availMB - uint64(reserveMB))
	}
	// budgetMB == 0 means we are already below the reserve; still attempt minimal compression.

	// Try each dict size from largest to smallest.
	for _, dict := range dictSizeLevels {
		// Start with the preferred thread count and reduce if needed.
		for t := maxThreads; t >= 1; t-- {
			required := float64(dict) * memCoeffPerThread * float64(t)
			if required <= float64(budgetMB) {
				return dict, t
			}
		}
	}

	// Absolute fallback: minimum possible resource usage.
	return 1, 1
}

func stepLog(format string, args ...any) {
	log.Printf("[step] "+format, args...)
}

func ensureOutputDoesNotExist(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("目标目录已存在同名文件: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查目标文件失败: %w", err)
	}
	return nil
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
