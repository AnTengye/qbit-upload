# qbit-upload

`qbit-upload` is a small Go CLI for scanning video files, creating an encrypted 7z archive, and moving the archive to a target directory.

## Features

- Filter video files by extension + MIME detection.
- Ignore files smaller than a configurable minimum size.
- Encrypt archive content and file list with 7z password mode (`-p` + `-mhe=on`).
- Optional dry-run mode.
- Per-run timestamped logs written to a log directory.

## Requirements

- Go 1.23+
- 7-Zip available in PATH or configured with `--7z` / config `seven_zip`.

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
