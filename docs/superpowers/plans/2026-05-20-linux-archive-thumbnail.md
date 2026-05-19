# Linux Archive Thumbnail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Linux-aware 7z discovery, tgz fallback archives, and ffmpeg-based video contact sheet generation.

**Architecture:** Split the current `cmd/root.go` responsibilities into small helpers while preserving the existing Cobra entrypoint. Archive behavior moves behind an archive plan and archive creation helpers. Thumbnail behavior gets its own option validation, output naming, and ffmpeg/ffprobe command builders.

**Tech Stack:** Go standard library, Cobra, YAML config, external `7z`/`7zz`, `ffmpeg`, and `ffprobe`.

---

## File Structure

- Modify `cmd/root.go`: flags, config structs, option resolution, run orchestration, archive and thumbnail integration.
- Create `cmd/archive.go`: archive planning, embedded 7z discovery, 7z invocation wrapper, tgz creation.
- Create `cmd/archive_test.go`: discovery priority, tgz contents, archive fallback tests.
- Create `cmd/memory_linux.go`: Linux `/proc/meminfo` memory implementation.
- Create `cmd/memory_linux_test.go`: `MemAvailable` parser tests.
- Create `cmd/thumbnail.go`: thumbnail option defaults, naming, ffprobe/ffmpeg command construction and execution.
- Create `cmd/thumbnail_test.go`: defaults, validation, naming collision, command construction.
- Modify `cmd/sys_other.go`: restrict fallback memory implementation to non-Windows and non-Linux.
- Modify `qbit-upload.example.yaml`: add archive and thumbnail config sections.
- Modify `README.md`: document Linux 7z embedding, tgz fallback, and thumbnails.

## Task 1: Linux Memory Parsing

**Files:**
- Create: `cmd/memory_linux.go`
- Create: `cmd/memory_linux_test.go`
- Modify: `cmd/sys_other.go`

- [ ] **Step 1: Write the failing parser test**

Create `cmd/memory_linux_test.go` with:

```go
//go:build linux

package cmd

import (
	"strings"
	"testing"
)

func TestParseMemAvailableMB(t *testing.T) {
	data := strings.NewReader("MemTotal:       16384256 kB\nMemFree:         1024000 kB\nMemAvailable:    3145728 kB\n")

	got, err := parseMemAvailableMB(data)
	if err != nil {
		t.Fatalf("parseMemAvailableMB returned error: %v", err)
	}
	if got != 3072 {
		t.Fatalf("parseMemAvailableMB = %d, want 3072", got)
	}
}

func TestParseMemAvailableMBMissing(t *testing.T) {
	_, err := parseMemAvailableMB(strings.NewReader("MemTotal: 1 kB\n"))
	if err == nil {
		t.Fatal("parseMemAvailableMB returned nil error for missing MemAvailable")
	}
}
```

- [ ] **Step 2: Run the Linux-tagged test to verify it fails**

Run: `go test ./cmd -run TestParseMemAvailableMB`

Expected on Windows: build skips Linux file and reports no tests matching, so also run this on Linux in CI after implementation. Locally, proceed with compile coverage via `go test ./...`.

- [ ] **Step 3: Implement Linux parser and memory getter**

Create `cmd/memory_linux.go`:

```go
//go:build linux

package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func getAvailableMemoryMB() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	available, err := parseMemAvailableMB(f)
	if err != nil {
		return 0
	}
	return available
}

func parseMemAvailableMB(r io.Reader) (uint64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("invalid MemAvailable line: %q", line)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemAvailable: %w", err)
		}
		return kb / 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemAvailable not found")
}
```

Modify `cmd/sys_other.go` build tag to:

```go
//go:build !windows && !linux
```

- [ ] **Step 4: Run tests**

Run: `go test ./...`

Expected: all packages pass.

## Task 2: Archive Planning and tgz Fallback

**Files:**
- Create: `cmd/archive.go`
- Create: `cmd/archive_test.go`
- Modify: `cmd/root.go`

- [ ] **Step 1: Write failing archive tests**

Create `cmd/archive_test.go` with tests for:

```go
func TestCreateTgzIncludesOnlyRequestedFiles(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.mp4"), "aaa")
	mustWriteFile(t, filepath.Join(root, "nested", "b.mkv"), "bbb")
	mustWriteFile(t, filepath.Join(root, "skip.txt"), "skip")

	out := filepath.Join(t.TempDir(), "out.tgz")
	if err := createTgzArchive(root, out, []string{"a.mp4", filepath.Join("nested", "b.mkv")}); err != nil {
		t.Fatalf("createTgzArchive returned error: %v", err)
	}

	got := readTgzNames(t, out)
	want := []string{"a.mp4", "nested/b.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tgz entries = %#v, want %#v", got, want)
	}
}

func TestDiscoverSevenZipPrefersEmbeddedCandidate(t *testing.T) {
	execDir := t.TempDir()
	embedded := filepath.Join(execDir, "tools", runtime.GOOS+"-"+runtime.GOARCH, executableName("7z"))
	mustWriteExecutable(t, embedded)

	got, err := discoverSevenZip("", execDir, "tools", func(name string) (string, error) {
		return filepath.Join(t.TempDir(), name), nil
	})
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != embedded {
		t.Fatalf("discoverSevenZip = %q, want %q", got, embedded)
	}
}

func TestDiscoverSevenZipUsesExplicitPath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), executableName("7z"))
	mustWriteExecutable(t, explicit)

	got, err := discoverSevenZip(explicit, t.TempDir(), "tools", nil)
	if err != nil {
		t.Fatalf("discoverSevenZip returned error: %v", err)
	}
	if got != explicit {
		t.Fatalf("discoverSevenZip = %q, want explicit path", got)
	}
}
```

Include helper functions in the same test file for writing files, marking executables with `0o755`, and reading `.tgz` names with `gzip.NewReader` and `tar.NewReader`.

- [ ] **Step 2: Run archive tests to verify they fail**

Run: `go test ./cmd -run "TestCreateTgz|TestDiscoverSevenZip"`

Expected: compile failure for missing functions.

- [ ] **Step 3: Implement archive helpers**

Create `cmd/archive.go` with:

- `type archiveFormat string` constants `archiveFormat7z` and `archiveFormatTgz`.
- `type archiveResult struct { Format archiveFormat; Path string; UsedFallback bool }`.
- `func discoverSevenZip(explicitPath, execDir, embeddedDir string, lookPath func(string) (string, error)) (string, error)`.
- `func createTgzArchive(sourceDir, outArchive string, files []string) error`.
- Move the existing `compressWith7z` function from `cmd/root.go` into `archive.go` unchanged except for package-local helper usage.

`createTgzArchive` must:

- Create the output file with `os.Create`.
- Wrap `gzip.NewWriter`.
- Add each requested file using forward-slash tar names via `filepath.ToSlash(rel)`.
- Reject relative paths that escape the source directory.
- Stream file content with `io.Copy`.

- [ ] **Step 4: Wire fallback into root flow**

Modify `cmd/root.go`:

- Add `Archive archiveConfig` to `appConfig`.
- Add `Archive archiveOptions` fields to `runOptions`.
- Resolve defaults: `AllowTgzFallback=true`, `Embedded7zDir="tools"`.
- Determine final archive extension after archive planning: `.7z` when 7z succeeds, `.tgz` when fallback is used.
- In non-dry-run, try 7z first. If discovery or compression fails and fallback is allowed, log INFO and call `createTgzArchive`.
- If 7z is used, require a non-empty password. If fallback is used due to no 7z, allow empty password.

- [ ] **Step 5: Run tests**

Run: `go test ./...`

Expected: all packages pass.

## Task 3: Thumbnail Options, Naming, and Commands

**Files:**
- Create: `cmd/thumbnail.go`
- Create: `cmd/thumbnail_test.go`
- Modify: `cmd/root.go`

- [ ] **Step 1: Write failing thumbnail tests**

Create `cmd/thumbnail_test.go` with:

```go
func TestResolveThumbnailDefaults(t *testing.T) {
	opts, err := resolveThumbnailOptions(thumbnailConfig{})
	if err != nil {
		t.Fatalf("resolveThumbnailOptions returned error: %v", err)
	}
	if !opts.Enabled || opts.FFmpeg != "ffmpeg" || opts.FFprobe != "ffprobe" {
		t.Fatalf("unexpected tool defaults: %#v", opts)
	}
	if opts.Columns != 4 || opts.Rows != 15 || opts.Width != 320 {
		t.Fatalf("unexpected grid defaults: %#v", opts)
	}
}

func TestResolveThumbnailRejectsInvalidGrid(t *testing.T) {
	_, err := resolveThumbnailOptions(thumbnailConfig{Enabled: boolPtr(true), Columns: -1, Rows: 15, Width: 320})
	if err == nil {
		t.Fatal("resolveThumbnailOptions returned nil error for invalid columns")
	}
}

func TestThumbnailOutputPathsAvoidDuplicateVideoBaseNames(t *testing.T) {
	dest := t.TempDir()
	files := []string{"movie.mp4", filepath.Join("nested", "movie.mkv")}
	got, err := planThumbnailOutputs("source", dest, files)
	if err != nil {
		t.Fatalf("planThumbnailOutputs returned error: %v", err)
	}
	if filepath.Base(got[0].OutputPath) != "source-movie-thumbnail.jpg" {
		t.Fatalf("first output = %s", got[0].OutputPath)
	}
	if filepath.Base(got[1].OutputPath) != "source-movie-2-thumbnail.jpg" {
		t.Fatalf("second output = %s", got[1].OutputPath)
	}
}

func TestBuildFFmpegThumbnailArgs(t *testing.T) {
	args := buildFFmpegThumbnailArgs("in.mp4", "out.jpg", thumbnailOptions{Columns: 4, Rows: 15, Width: 320}, "name.mp4", "1.5 GiB", "01:02:03")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i in.mp4", "tile=4x15", "scale=320:-1", "out.jpg"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ffmpeg args %q missing %q", joined, want)
		}
	}
}
```

- [ ] **Step 2: Run thumbnail tests to verify they fail**

Run: `go test ./cmd -run "TestResolveThumbnail|TestThumbnailOutputPaths|TestBuildFFmpeg"`

Expected: compile failure for missing types/functions.

- [ ] **Step 3: Implement thumbnail helpers**

Create `cmd/thumbnail.go` with:

- `type thumbnailConfig struct { Enabled *bool; FFmpeg string; FFprobe string; Columns int; Rows int; Width int }`
- `type thumbnailOptions struct { Enabled bool; FFmpeg string; FFprobe string; Columns int; Rows int; Width int }`
- `type thumbnailOutput struct { InputRel string; InputPath string; OutputPath string }`
- `resolveThumbnailOptions(cfg thumbnailConfig) (thumbnailOptions, error)`.
- `planThumbnailOutputs(sourceBase, destDir string, files []string) ([]thumbnailOutput, error)`.
- `buildFFprobeDurationArgs(input string) []string`.
- `buildFFmpegThumbnailArgs(input, output string, opts thumbnailOptions, displayName, sizeText, durationText string) []string`.
- `generateThumbnails(sourceDir, sourceBase, destDir string, files []string, opts thumbnailOptions) error`.

`generateThumbnails` should:

- Return immediately when disabled.
- Verify no output path already exists.
- Run `ffprobe` for duration.
- Run `ffmpeg` to generate the contact sheet.
- Capture stdout/stderr and include them in errors.

- [ ] **Step 4: Wire flags and config into root flow**

Modify `cmd/root.go`:

- Add thumbnail global flag variables.
- Register flags: `--thumbnail`, `--ffmpeg`, `--ffprobe`, `--thumbnail-columns`, `--thumbnail-rows`, `--thumbnail-width`.
- Add `Thumbnail thumbnailConfig` to `appConfig`.
- Add `Thumbnail thumbnailOptions` to `runOptions`.
- Merge CLI overrides after config.
- In dry-run, print planned thumbnail outputs when enabled.
- After archive move succeeds and before deleting source, call `generateThumbnails`.

- [ ] **Step 5: Run tests**

Run: `go test ./...`

Expected: all packages pass.

## Task 4: Docs and Example Config

**Files:**
- Modify: `qbit-upload.example.yaml`
- Modify: `README.md`

- [ ] **Step 1: Update example config**

Add:

```yaml
archive:
  allow_tgz_fallback: true
  embedded_7z_dir: tools

thumbnail:
  enabled: true
  ffmpeg: ffmpeg
  ffprobe: ffprobe
  columns: 4
  rows: 15
  width: 320
```

- [ ] **Step 2: Update README**

Document:

- Linux amd64/arm64 can ship embedded `tools/<goos>-<goarch>/7z` or `7zz`.
- `.tgz` fallback is unencrypted and logged as INFO.
- Thumbnail generation requires `ffmpeg` and `ffprobe`.
- Default contact sheet is `4` columns by `15` rows and includes file name, size, duration.
- Disable thumbnails with config or `--thumbnail=false`.

- [ ] **Step 3: Run final verification**

Run: `gofmt -w .`

Run: `go test ./...`

Expected: all packages pass.
