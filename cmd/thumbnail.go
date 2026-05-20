package cmd

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type thumbnailConfig struct {
	Enabled *bool  `json:"enabled" yaml:"enabled"`
	FFmpeg  string `json:"ffmpeg" yaml:"ffmpeg"`
	FFprobe string `json:"ffprobe" yaml:"ffprobe"`
	Columns int    `json:"columns" yaml:"columns"`
	Rows    int    `json:"rows" yaml:"rows"`
	Width   int    `json:"width" yaml:"width"`
}

type thumbnailOptions struct {
	Enabled bool
	FFmpeg  string
	FFprobe string
	Columns int
	Rows    int
	Width   int
}

type thumbnailOutput struct {
	InputRel   string
	InputPath  string
	OutputPath string
}

func resolveThumbnailOptions(cfg thumbnailConfig) (thumbnailOptions, error) {
	opts := thumbnailOptions{
		Enabled: true,
		FFmpeg:  "ffmpeg",
		FFprobe: "ffprobe",
		Columns: 4,
		Rows:    15,
		Width:   320,
	}
	if cfg.Enabled != nil {
		opts.Enabled = *cfg.Enabled
	}
	if strings.TrimSpace(cfg.FFmpeg) != "" {
		opts.FFmpeg = cfg.FFmpeg
	}
	if strings.TrimSpace(cfg.FFprobe) != "" {
		opts.FFprobe = cfg.FFprobe
	}
	if cfg.Columns != 0 {
		opts.Columns = cfg.Columns
	}
	if cfg.Rows != 0 {
		opts.Rows = cfg.Rows
	}
	if cfg.Width != 0 {
		opts.Width = cfg.Width
	}
	if opts.Columns <= 0 {
		return thumbnailOptions{}, fmt.Errorf("thumbnail columns must be greater than 0")
	}
	if opts.Rows <= 0 {
		return thumbnailOptions{}, fmt.Errorf("thumbnail rows must be greater than 0")
	}
	if opts.Width <= 0 {
		return thumbnailOptions{}, fmt.Errorf("thumbnail width must be greater than 0")
	}
	if strings.TrimSpace(opts.FFmpeg) == "" {
		return thumbnailOptions{}, fmt.Errorf("ffmpeg path cannot be empty")
	}
	if strings.TrimSpace(opts.FFprobe) == "" {
		return thumbnailOptions{}, fmt.Errorf("ffprobe path cannot be empty")
	}
	return opts, nil
}

func planThumbnailOutputs(sourceBase, destDir string, files []string) ([]thumbnailOutput, error) {
	seen := make(map[string]int)
	outputs := make([]thumbnailOutput, 0, len(files))
	for _, rel := range files {
		base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		if strings.TrimSpace(base) == "" {
			base = "video"
		}
		key := sourceBase + "-" + base
		seen[key]++
		nameBase := key
		if seen[key] > 1 {
			nameBase = fmt.Sprintf("%s-%d", key, seen[key])
		}
		out := filepath.Join(destDir, nameBase+"-thumbnail.jpg")
		outputs = append(outputs, thumbnailOutput{
			InputRel:   rel,
			OutputPath: out,
		})
	}
	return outputs, nil
}

func buildFFprobeDurationArgs(input string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		input,
	}
}

func buildFFmpegFrameArgs(input, output string, at time.Duration, width int) []string {
	return []string{
		"-y",
		"-ss", formatFFmpegTimestamp(at),
		"-i", input,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-1", width),
		"-q:v", "3",
		output,
	}
}

func generateThumbnails(sourceDir, sourceBase, destDir string, files []string, opts thumbnailOptions) error {
	if !opts.Enabled {
		return nil
	}
	outputs, err := planThumbnailOutputs(sourceBase, destDir, files)
	if err != nil {
		return err
	}
	for _, out := range outputs {
		if _, err := os.Stat(out.OutputPath); err == nil {
			return fmt.Errorf("缩略图已存在: %s", out.OutputPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("检查缩略图失败 %s: %w", out.OutputPath, err)
		}
	}

	for _, out := range outputs {
		input := filepath.Join(sourceDir, out.InputRel)
		duration, err := probeDuration(opts.FFprobe, input)
		if err != nil {
			return err
		}
		if err := generateSeekContactSheet(input, out.OutputPath, opts, duration); err != nil {
			return err
		}
		stepLog("缩略图生成完成: %s", out.OutputPath)
	}
	return nil
}

func generateSeekContactSheet(input, output string, opts thumbnailOptions, duration time.Duration) error {
	tempDir, err := os.MkdirTemp("", "qbit-upload-frames-*")
	if err != nil {
		return fmt.Errorf("创建缩略图临时目录失败: %w", err)
	}
	defer os.RemoveAll(tempDir)

	count := opts.Columns * opts.Rows
	framePaths := make([]string, 0, count)
	for i, at := range sampleTimes(count, duration) {
		framePath := filepath.Join(tempDir, fmt.Sprintf("frame-%03d.jpg", i))
		args := buildFFmpegFrameArgs(input, framePath, at, opts.Width)
		if err := runLoggedCommand(opts.FFmpeg, args); err != nil {
			return fmt.Errorf("抽取缩略图帧失败 %s at %s: %w", input, formatDuration(at), err)
		}
		framePaths = append(framePaths, framePath)
	}
	if err := stitchFrames(framePaths, output, opts.Columns, opts.Rows); err != nil {
		return fmt.Errorf("拼接缩略图失败 %s: %w", output, err)
	}
	return nil
}

func sampleTimes(count int, duration time.Duration) []time.Duration {
	if count <= 0 {
		return nil
	}
	if duration <= 0 {
		duration = time.Duration(count+1) * time.Second
	}
	step := duration / time.Duration(count+1)
	times := make([]time.Duration, 0, count)
	for i := 1; i <= count; i++ {
		times = append(times, step*time.Duration(i))
	}
	return times
}

func stitchFrames(framePaths []string, output string, columns, rows int) error {
	if columns <= 0 || rows <= 0 {
		return fmt.Errorf("invalid thumbnail grid: %dx%d", columns, rows)
	}
	images := make([]image.Image, 0, len(framePaths))
	cellW, cellH := 0, 0
	for _, path := range framePaths {
		img, err := decodeImage(path)
		if err != nil {
			return err
		}
		b := img.Bounds()
		if cellW == 0 || b.Dx() > cellW {
			cellW = b.Dx()
		}
		if cellH == 0 || b.Dy() > cellH {
			cellH = b.Dy()
		}
		images = append(images, img)
	}
	if cellW == 0 || cellH == 0 {
		return fmt.Errorf("no frames to stitch")
	}

	canvas := image.NewRGBA(image.Rect(0, 0, columns*cellW, rows*cellH))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	for i, img := range images {
		if i >= columns*rows {
			break
		}
		x := (i % columns) * cellW
		y := (i / columns) * cellH
		dst := image.Rect(x, y, x+img.Bounds().Dx(), y+img.Bounds().Dy())
		draw.Draw(canvas, dst, img, img.Bounds().Min, draw.Src)
	}

	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()
	return jpeg.Encode(out, canvas, &jpeg.Options{Quality: 90})
}

func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func probeDuration(ffprobePath, input string) (time.Duration, error) {
	args := buildFFprobeDurationArgs(input)
	cmd := exec.Command(ffprobePath, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe 获取时长失败 %s: %w\n%s", input, err, strings.TrimSpace(buf.String()))
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(buf.String()), 64)
	if err != nil {
		return 0, fmt.Errorf("解析 ffprobe 时长失败 %s: %w", input, err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func runLoggedCommand(path string, args []string) error {
	cmd := exec.Command(path, args...)
	var buf bytes.Buffer
	mw := io.MultiWriter(runtimeLogOutput, &buf)
	cmd.Stdout = mw
	cmd.Stderr = mw
	stepLog("执行命令: %s %s", path, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}

func formatDuration(d time.Duration) string {
	total := int64(d.Round(time.Second).Seconds())
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}

func formatFFmpegTimestamp(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}
