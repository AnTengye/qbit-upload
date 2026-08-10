# qbit-upload

`qbit-upload` is a small Go CLI for scanning video files, creating an encrypted 7z archive, and moving the archive to a target directory.

## Features

- Filter video files by extension + MIME detection.
- Ignore files smaller than a configurable minimum size.
- Encrypt archive content and file list with 7z password mode (`-p` + `-mhe=on`).
- Optional split mode to create one archive per matched video file.
- Configurable archive temp directory.
- Watch mode for automatically processing completed large video copies.
- Linux amd64/arm64 can use an embedded official `7zz`/`7z` under `tools/<goos>-<goarch>/`.
- Linux systemd service installer for watch-mode autostart.
- Fall back to an unencrypted `.tgz` archive when 7z is unavailable or fails.
- Generate ffmpeg thumbnail contact sheets for matched videos.
- Normalize movie catalog numbers and asynchronously report each completed film with its preview image.
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

Create one archive per matched video:

```powershell
go run . --split --config qbit-upload.example.yaml <source-dir>
```

Watch configured folders and process completed large video copies:

```powershell
go run . watch --config qbit-upload.example.yaml
```

Generate thumbnails only, without archiving or deleting the source directory:

```powershell
go run . thumbnail --config qbit-upload.example.yaml <source-dir>
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

Catalog-number preprocessing and film reporting are enabled in the normal archive flow. Reporting defaults to the Avister Film external endpoint; configure the API key through `QBIT_UPLOAD_REPORT_API_KEY` and use `--report=false` only when reporting must be disabled. See [Catalog Number Preprocessing and Film Reporting](docs/catalog-number-reporting.md) for the complete rules, examples, configuration, and failure behavior.

### Linux 7z and tgz fallback

When `seven_zip` / `--7z` is not set, the CLI looks for embedded tools before checking `PATH`. On non-Windows systems it prefers `7zz`, because current official 7-Zip Linux releases use the new Linux console version and the older p7zip port is no longer recommended by 7-Zip upstream.

- `tools/linux-amd64/7zz` or `tools/linux-amd64/7z`
- `tools/linux-arm64/7zz` or `tools/linux-arm64/7z`
- `7zz` or `7z` next to the executable
- `7zz` or `7z` in `PATH`

If you distribute an embedded 7-Zip binary with this tool, include the 7-Zip license/source attribution required by 7-Zip's LGPL distribution guidance.

If 7z is unavailable or compression fails and `archive.allow_tgz_fallback` is `true`, the CLI writes an unencrypted `.tgz` archive and records the fallback in the run log at INFO level.

Set `archive.temp_dir` to control where temporary archives are written before being moved to `dest_dir`. Leave it empty to use the OS temp directory.

### Archive Split Mode

By default, all matched videos under the source directory are written into one archive named after the source directory, for example `MyFolder.7z`.

Use `--split` or config `archive.split: true` to create one archive per matched video. Archive names are based on each video file name without the video extension:

- `Movie.mp4` -> `Movie.7z`
- `nested/Movie.mkv` -> `Movie-2.7z` when another matched video already uses `Movie.7z`

When tgz fallback is used, the same naming rule is used with `.tgz`.

### Watch Mode

Use `watch.dirs` to list incoming folders. The watcher scans those folders repeatedly and waits until a matched video file remains stable for `watch.stable_delay` before processing it.

```yaml
watch:
  enabled: true
  dirs:
    - D:/downloads/incoming
  stable_delay: 30s
  poll_interval: 5s
```

If a large video is copied directly into the top level of a watched folder, the archive name is based on the video file name:

- `D:/downloads/incoming/Movie.mp4` -> `Movie.7z`

If a large video is found inside a child folder, the child folder is processed as the source directory:

- `D:/downloads/incoming/Show/Season 1/Episode.mkv` -> source `Show`

After a successful run, the processed file or directory is deleted, matching the normal archive flow.

### Linux Service

On Linux systemd hosts, install and start a watch-mode service:

```bash
sudo qbit-upload --config /etc/qbit-upload.yaml install-service
```

The generated unit runs:

```bash
qbit-upload watch --config /etc/qbit-upload.yaml
```

Use `--name` to change the service name and `--user` to run as a specific Linux user.

### Thumbnails

Thumbnails are enabled by default. For each matched video, the CLI writes a JPEG contact sheet next to the archive in the destination directory. The default layout is `4` columns by `15` rows. Frames are sampled by seeking to evenly spaced points in the video, similar to media player thumbnail generation, instead of scanning the full stream with an fps filter.

In the normal archive flow, thumbnails are generated before compression starts. This makes missing `ffmpeg` / `ffprobe` fail early, before spending time creating an archive.

Disable thumbnails with:

```powershell
go run . --thumbnail=false <source-dir>
```

Or in config:

```yaml
thumbnail:
  enabled: false
```

If you see an error like `exec: "ffprobe": executable file not found in %PATH%`, install ffmpeg and add its `bin` directory to `PATH`, or configure absolute paths:

```yaml
thumbnail:
  ffmpeg: D:/tools/ffmpeg/bin/ffmpeg.exe
  ffprobe: D:/tools/ffmpeg/bin/ffprobe.exe
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
