# qbit-upload

`qbit-upload` is a small Go CLI for scanning video files, creating an encrypted 7z archive, and moving the archive to a target directory.

## Features

- Filter video files by extension + MIME detection.
- Ignore files smaller than a configurable minimum size.
- Encrypt archive content and file list with 7z password mode (`-p` + `-mhe=on`).
- Linux amd64/arm64 can use an embedded `7z`/`7zz` under `tools/<goos>-<goarch>/`.
- Fall back to an unencrypted `.tgz` archive when 7z is unavailable or fails.
- Generate ffmpeg thumbnail contact sheets for matched videos.
- Optional dry-run mode.
- Per-run timestamped logs written to a log directory.

## Requirements

- Go 1.23+
- 7-Zip available in PATH or configured with `--7z` / config `seven_zip`.
- ffmpeg and ffprobe available in PATH, or configured with `--ffmpeg` / `--ffprobe`, when thumbnails are enabled.

## Quick Start

```powershell
go run . --dry-run <source-dir>
```

Use explicit config:

```powershell
go run . --config qbit-upload.example.yaml <source-dir>
```

Build binary:

```powershell
go build ./...
```

Run tests:

```powershell
go test ./...
```

## Config

Copy `qbit-upload.example.yaml` and adjust values for your environment.

Supported config names for auto-discovery (same directory as executable):

- `qbit-upload.yaml`
- `qbit-upload.yml`
- `qbit-upload.json`

CLI flags override config values.

### Linux 7z and tgz fallback

When `seven_zip` / `--7z` is not set, the CLI looks for embedded tools before checking `PATH`:

- `tools/linux-amd64/7z` or `tools/linux-amd64/7zz`
- `tools/linux-arm64/7z` or `tools/linux-arm64/7zz`
- `7z` or `7zz` next to the executable
- `7z` or `7zz` in `PATH`

If 7z is unavailable or compression fails and `archive.allow_tgz_fallback` is `true`, the CLI writes an unencrypted `.tgz` archive and records the fallback in the run log at INFO level.

### Thumbnails

Thumbnails are enabled by default. For each matched video, the CLI writes a JPEG contact sheet next to the archive in the destination directory. The default layout is `4` columns by `15` rows, and the image label includes file name, file size, and duration.

Disable thumbnails with:

```powershell
go run . --thumbnail=false <source-dir>
```

Or in config:

```yaml
thumbnail:
  enabled: false
```

## Batch Script

`batch-run.bat` can be used on Windows for scheduled/batch execution.

## CI / Auto Build

GitHub Actions workflow `.github/workflows/build.yml` provides:

- CI build and test on `push` and `pull_request`.
- Cross-platform binaries (Windows/Linux/macOS) as workflow artifacts.
- Release assets upload when pushing a tag like `v1.0.0`.

Tag release example:

```powershell
git tag v1.0.0
git push origin v1.0.0
```
