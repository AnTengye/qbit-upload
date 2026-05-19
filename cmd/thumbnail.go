package cmd

import (
	"bytes"
	"fmt"
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

func buildFFmpegThumbnailArgs(input, output string, opts thumbnailOptions, displayName, sizeText, durationText string) []string {
	return buildFFmpegThumbnailArgsWithFPS(input, output, opts, displayName, sizeText, durationText, "1/10")
}

func buildFFmpegThumbnailArgsForDuration(input, output string, opts thumbnailOptions, displayName, sizeText, durationText string, duration time.Duration) []string {
	return buildFFmpegThumbnailArgsWithFPS(input, output, opts, displayName, sizeText, durationText, thumbnailFPS(opts, duration))
}

func buildFFmpegThumbnailArgsWithFPS(input, output string, opts thumbnailOptions, displayName, sizeText, durationText, fps string) []string {
	label := escapeDrawText(fmt.Sprintf("%s | %s | %s", displayName, sizeText, durationText))
	filter := fmt.Sprintf(
		"fps=%s,scale=%d:-1,tile=%dx%d,drawtext=text='%s':x=8:y=8:fontsize=24:fontcolor=white:box=1:boxcolor=black@0.65",
		fps,
		opts.Width,
		opts.Columns,
		opts.Rows,
		label,
	)
	return []string{
		"-y",
		"-i", input,
		"-frames:v", "1",
		"-vf", filter,
		output,
	}
}

func buildPlainFFmpegThumbnailArgs(input, output string, opts thumbnailOptions) []string {
	filter := fmt.Sprintf("fps=1/10,scale=%d:-1,tile=%dx%d", opts.Width, opts.Columns, opts.Rows)
	return []string{
		"-y",
		"-i", input,
		"-frames:v", "1",
		"-vf", filter,
		output,
	}
}

func buildPlainFFmpegThumbnailArgsForDuration(input, output string, opts thumbnailOptions, duration time.Duration) []string {
	filter := fmt.Sprintf("fps=%s,scale=%d:-1,tile=%dx%d", thumbnailFPS(opts, duration), opts.Width, opts.Columns, opts.Rows)
	return []string{
		"-y",
		"-i", input,
		"-frames:v", "1",
		"-vf", filter,
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
		info, err := os.Stat(input)
		if err != nil {
			return fmt.Errorf("读取视频文件失败 %s: %w", input, err)
		}
		duration, err := probeDuration(opts.FFprobe, input)
		if err != nil {
			return err
		}
		durationText := formatDuration(duration)
		args := buildFFmpegThumbnailArgsForDuration(input, out.OutputPath, opts, filepath.Base(out.InputRel), formatBytes(info.Size()), durationText, duration)
		if err := runLoggedCommand(opts.FFmpeg, args); err != nil {
			stepLog("INFO: 带文字缩略图生成失败，将生成无文字网格: %v", err)
			plainArgs := buildPlainFFmpegThumbnailArgsForDuration(input, out.OutputPath, opts, duration)
			if plainErr := runLoggedCommand(opts.FFmpeg, plainArgs); plainErr != nil {
				return fmt.Errorf("生成缩略图失败 %s: %w", input, plainErr)
			}
		}
		stepLog("缩略图生成完成: %s", out.OutputPath)
	}
	return nil
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

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

func thumbnailFPS(opts thumbnailOptions, duration time.Duration) string {
	if duration <= 0 || opts.Columns <= 0 || opts.Rows <= 0 {
		return "1/10"
	}
	frameCount := float64(opts.Columns * opts.Rows)
	fps := frameCount / duration.Seconds()
	if fps <= 0 {
		return "1/10"
	}
	return strconv.FormatFloat(fps, 'f', 6, 64)
}

func escapeDrawText(s string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		":", "\\:",
		"'", "\\'",
		"%", "\\%",
	)
	return replacer.Replace(s)
}
