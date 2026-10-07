# 绿联网盘上传和持久化重试

在 NAS 上设置 `upload.enabled: true` 启用新流程。没有设置此项的旧配置保留原来的只打包模式。

## 顺序

1. 保存任务记录，生成缩略图和加密压缩包。
2. 将完整压缩包移动到 `archive.dest_dir`（默认 `dest_dir`）。跨文件系统先复制到临时文件，再原子重命名。
3. 调用原生 `/ugreen/v3/netDisk/server/file/upload`，复用当前 NAS 用户已绑定的百度网盘账号。
4. 等待原生任务状态为 5、全部字节和文件传输完成、零错误，并核对网盘文件大小。上传请求被接收不等于上传完成。
5. 本批次压缩包全部上传完成后，调用 `POST /api/external/films` 上报规范化番号及 `status: 5`（已归档）。有预览图使用 multipart，没有图片使用 JSON。
6. 本批次所有上报收到 `200/201/204` 后，先保存确认，再删除本地压缩包和任务私有缩略图。若 `archive.delete_source: true`，此后删除本次处理且未发生变化的源视频；其他文件保留。

无法识别全部番号、禁用上报、缺少 API Key、上传或上报失败均保留本地压缩包。`409` 不能证明状态已更新为 5，因此新流程将其作为失败，保留文件并重试。

## NAS 配置

保留原有目录、压缩密码和上报设置，添加：

```yaml
upload:
  enabled: true
  base_url: https://127.0.0.1:9443
  username: wenadmin
  certificate_sha256: "your-nas-certificate-sha256"
  remote_dir: /绿联网盘
  # 可省略，默认输出目录下的 .qbit-upload-jobs。
  state_dir: /volume1/docker/qbit-upload-state
  task_db: /volume1/@appstore/com.ugreen.netdisk/db/net_disk.db
  sqlite: sqlite3
  timeout: 24h
  poll_interval: 5s
  retry_interval: 12h
```

建议设置 `archive.allow_tgz_fallback: false`，避免 7z 失败后上传未加密的 tgz。`archive.delete_source: false` 可以保留原视频，本地压缩包仍在上报成功后删除。

NAS 密码通过 `QBIT_UPLOAD_NAS_PASSWORD` 提供，影片 API Key 使用 `QBIT_UPLOAD_REPORT_API_KEY` 或原来的配置。任务记录不保存密码和 API Key；百度 OAuth token 由原生网盘工具维护，程序仅在内存使用。

自签名证书必须配置实际 SHA256 指纹，不允许任意自签名证书。NAS 上查询：

```bash
openssl s_client -connect 127.0.0.1:9443 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

通过 `sqlite3 -readonly` 查询上传数据库，不修改原生数据库。服务用户必须能够读取该文件，默认 root 服务满足此条件。上传路径必须是 NAS 本机的真实文件路径，绑定的 NAS 用户也需要能读取该路径。Docker 部署还需能访问 NAS HTTPS、真实路径及原生数据库。

## 恢复

每次失败记录阶段、原生任务 ID、错误和失败后 12 小时的重试时间。`watch` 每次扫描前检查到期任务；打包、上传或扫描的耗时可能推迟执行。`watch` 和普通命令启动时立即恢复未完成任务，不等下一次重试时间。

```bash
qbit-upload watch --config /etc/qbit-upload.yaml
# 或只立即恢复一次，可用于手动恢复、外部定时器
qbit-upload retry --config /etc/qbit-upload.yaml
```

队列默认在输出目录的 `.qbit-upload-jobs`，每任务一份原子写入的 JSON。完成记录保留，用于阻止保留源文件时重复处理。不要删除未完成记录，否则会失去上传及上报确认。操作系统锁阻止多个进程同时修改同一队列，进程退出自动释放锁。

提交上传前保存意图和同路径旧任务 ID。请求中断后先核对原生任务，已经存在的任务继续等待，不重复提交。明确失败的任务在后续重试中重新提交。只有云端同名文件但缺少本次成功记录时，不覆盖云端，也不删除本地文件。

每次上报重试使用相同 `Idempotency-Key`。服务端需要支持该 key 返回原先的成功结果，或对已有番号更新状态并返回成功；否则“服务端处理完成但响应中断”的请求可能在重试时返回 409，客户端会继续保留压缩包。

## status 约定

此仓库是客户端，不包含 `/api/external/films` 服务端。客户端对接以下约定：JSON/multipart 的 `status` 可选，缺省或 `null` 默认 1；1 默认、2 入库、3 丢弃、4 无资源、5 已归档；其余返回 400。上传归档流程固定传整数 5。

```json
{"code":"ABC-123","status":5}
```

有图片时 multipart 为 `code`、`status=5`、`previewFile`，API Key 仍为 `Key` 请求头。客户端也会在请求前拒绝非 1–5 的状态。

## systemd 密码环境文件

保留原来的 API Key 环境文件，另外添加只允许 root 读取、权限为 `600` 的 `/etc/qbit-upload-nas.env`，格式为 `QBIT_UPLOAD_NAS_PASSWORD=...`。不要将其提交到仓库或把密码放进命令参数。服务 drop-in：

```ini
[Service]
EnvironmentFile=/etc/qbit-upload-nas.env
```

原生接口已按 UGOS Pro 1.17.0.0095 / 网盘工具 1.17.0.2829 验证。绿联升级造成接口或数据库格式变化时，程序失败并保留文件，需重新适配。
