# Linux Archive Fallback and Video Thumbnail Design

## Goal

Extend `qbit-upload` so it can run well on Linux amd64/arm64, prefer 7z compression when available, fall back to an unencrypted `.tgz` archive when needed, and generate per-video thumbnail contact sheets in the destination directory.

## Current Context

The project is a small Go CLI. `cmd/root.go` owns flag parsing, config loading, video scanning, 7z execution, logging, and file movement. Windows has a real `getAvailableMemoryMB()` implementation in `cmd/sys_windows.go`; non-Windows currently returns `0`, causing 7z parameter selection to use CPU-only defaults. Existing tests cover `calcSevenZipParams`.

## Archive Behavior

The archive step should support three paths:

1. Use explicit 7z path from `--7z` or config `seven_zip` when provided.
2. If no explicit path is provided, discover an embedded 7z near the executable before checking `PATH`.
3. If 7z is unavailable or 7z compression fails, create an unencrypted `.tgz` fallback and log that fallback at INFO level.

Embedded discovery should be simple and portable. On Linux amd64 and arm64, check candidates under the executable directory such as `tools/linux-amd64/7z`, `tools/linux-amd64/7zz`, `tools/linux-arm64/7z`, `tools/linux-arm64/7zz`, then same-directory `7z`/`7zz`, then `PATH`. The config can override the embedded tool directory name with `archive.embedded_7z_dir`, defaulting to `tools`.

The existing 7z CPU and memory budget behavior remains. Linux should get a real memory implementation by parsing `/proc/meminfo` and using `MemAvailable`. The existing `calcSevenZipParams` function remains the shared resource decision point.

The `.tgz` fallback should be streaming and low-memory: Go standard library `archive/tar` plus `compress/gzip`, adding only the eligible video files using their relative paths. It does not encrypt content. The current password requirement should be relaxed only enough to allow dry-run and tgz fallback decisions, while normal 7z archive creation still requires a password.

## Thumbnail Behavior

Thumbnail generation is enabled by default. It runs after archive creation succeeds and before deleting the source directory. For each eligible video, the CLI creates one JPEG contact sheet in the destination directory. The file name uses the source directory base name and a stable suffix, for example:

`<source-base>-<video-base>-thumbnail.jpg`

If two videos have the same base name, append a numeric suffix to avoid collisions. Existing destination files should cause a clear error rather than being overwritten.

Each contact sheet defaults to `4` columns and `15` rows, for `60` sampled frames arranged as a vertical sheet. The image includes extra information: file name, file size, and duration. File size comes from Go. Duration comes from `ffprobe`. Frame extraction and tiling use `ffmpeg`.

Configuration should allow:

- Enable or disable thumbnail generation.
- Configure `ffmpeg` and `ffprobe` paths.
- Configure columns and rows.
- Configure thumbnail tile width.

CLI flags should mirror the practical controls:

- `--thumbnail`
- `--ffmpeg`
- `--ffprobe`
- `--thumbnail-columns`
- `--thumbnail-rows`
- `--thumbnail-width`

If `ffmpeg` or `ffprobe` is missing while thumbnail generation is enabled, the command should fail with an actionable error. If metadata text rendering fails because of ffmpeg filter/font differences, the implementation may fall back to a plain tiled image and record the downgrade at INFO level.

## Configuration Shape

Keep existing top-level fields for backward compatibility, and add nested groups for new behavior:

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

Default `archive.allow_tgz_fallback` is `true`, matching the requested fallback behavior.

## Error Handling and Logging

Errors should include the failing path or operation. Archive fallback should be logged as INFO with enough detail to explain whether 7z was missing or failed. Thumbnail command output should be captured and included in errors. Dry-run should print planned archive format and thumbnail outputs without invoking external tools or modifying files.

## Testing

Use Go's built-in `testing` package.

Tests should cover:

- Linux `/proc/meminfo` parsing for `MemAvailable`.
- Archive tool discovery priority with embedded candidates and PATH lookup injection.
- `.tgz` fallback archive contents.
- Thumbnail option defaults and validation.
- Thumbnail output naming collision behavior.
- Config parsing for new nested fields while preserving existing fields.

Full verification command: `go test ./...`.
