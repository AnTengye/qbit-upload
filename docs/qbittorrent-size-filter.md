# qBittorrent 下载前按大小过滤

在现有 `qbit-upload watch` 服务中启用 `torrent_filter.enabled`，同一个进程会同时运行下载过滤和文件监控、打包、上传。过滤使用单独的轮询循环，打包上传耗时较长时仍能继续筛选种子，两部分共用配置文件和日志。它只管理带有 `torrent_filter.tag` 标签的任务，等待 qBittorrent 获取元数据，将小于配置阈值的文件设置为“不下载”，回读文件优先级核验成功后启动下载。浏览器提交成功后可以关闭。

过滤只根据文件大小，不判断扩展名、文件名或正片身份。等于阈值的文件保留；原本取消下载的大文件仍然保持取消。不会删除已经下载的附件。

## 配置

在现有配置中添加：

```yaml
torrent_filter:
  enabled: true
  base_url: http://127.0.0.1:8080
  username: your-webui-user
  min_size_mb: 100
  tag: filter-small
  poll_interval: 5s
  retry_interval: 30s
  request_timeout: 10s
  state_dir: /var/lib/qbit-upload/torrent-filter
```

`min_size_mb` 的单位是 MiB，100 对应 `104857600` 字节。该配置独立于顶层 `min_size_mb`，后者仍然用于本地视频打包。

`enabled` 默认是 `false`，保留旧配置的行为。启用后，地址、凭据或状态目录配置错误会阻止服务启动；运行中的接口暂时不可用、等待元数据或单个种子筛选失败会记录并重试，文件监控继续运行。

密码优先从 `QBIT_UPLOAD_QBITTORRENT_PASSWORD` 获取；`QBIT_UPLOAD_QBITTORRENT_URL` 和 `QBIT_UPLOAD_QBITTORRENT_USERNAME` 也可以覆盖配置。用户名和密码必须同时提供；如果 qBittorrent 对本机请求启用了免认证，可以同时留空。配置地址支持反向代理路径，例如 `http://localhost/qb`。

后台服务支持 qBittorrent 4.5+ 和 5.x，会根据版本选择 `pause/resume` 或 `stop/start` 接口。

## 添加任务的入口

浏览器的“一键 NAS”入口需要在 **同一次添加请求** 中携带专用标签和停止参数。

添加磁力链接时，通过 `POST /api/v2/torrents/add` 提交以下表单字段：

| 字段 | qBittorrent 4.5+ | qBittorrent 5.x |
| --- | --- | --- |
| `urls` | 磁力链接 | 磁力链接 |
| `tags` | `filter-small` | `filter-small` |
| `stopCondition` | `MetadataReceived` | `MetadataReceived` |
| 开始获取元数据 | `paused=false` | `stopped=false` |

标签必须与配置一致。已有业务标签可以一起传入，以逗号分隔。必须明确允许获取元数据，并设置收到元数据后停止；不要先正常下载、随后才加标签。NAS 服务不会暂停正在获取元数据的任务。

如果上传 `.torrent` 文件，文件列表已存在：以 multipart 表单的 `torrents` 字段提交文件，带上标签，并使用 `paused=true`（4.x）或 `stopped=true`（5.x）直接停止添加。此时不要依赖“收到元数据后停止”，因为元数据已经存在。

若添加入口漏设停止条件，服务会在取得文件列表后先停止任务，再修改优先级；轮询发现前可能已下载少量内容。因此正确传入添加参数仍然是下载前过滤的必要条件。

## 运行与检查

正常常驻运行统一使用：

```sh
qbit-upload watch --config /etc/qbit-upload.yaml
```

`filter-torrents` 保留作为手动扫描和诊断入口，显式运行此命令时不受 `enabled` 控制。使用 `--once --dry-run` 可在服务运行时只查看计划；会修改任务的独立命令与统一服务不能同时占用同一个状态目录。

```sh
# 只查看计划，不修改任务或写入过滤状态。
qbit-upload filter-torrents --config /etc/qbit-upload.yaml --once --dry-run

# 扫描一次，等待元数据的任务留待下次处理。
qbit-upload filter-torrents --config /etc/qbit-upload.yaml --once

# 仅在需要单独诊断过滤流程时常驻运行。
qbit-upload filter-torrents --config /etc/qbit-upload.yaml
```

`--once` 在任务过滤或核验失败时返回错误；元数据还未到不属于失败。常驻运行会记录失败原因并按 `retry_interval` 重试，某个任务等待元数据不会阻塞其他任务。

## NAS systemd 服务

先将更新后的二进制放在稳定路径，例如 `/usr/local/bin/qbit-upload`。将凭据放进 root 可读的 `/etc/qbit-upload-qbittorrent.env`（权限 `0600`）：

```text
QBIT_UPLOAD_QBITTORRENT_USERNAME=your-webui-user
QBIT_UPLOAD_QBITTORRENT_PASSWORD=your-webui-password
```

安装统一服务（已有 `qbit-upload.service` 时更新程序、配置并重启即可）：

```sh
sudo /usr/local/bin/qbit-upload install-service \
  --config /etc/qbit-upload.yaml \
  --env-file /etc/qbit-upload-qbittorrent.env

systemctl status qbit-upload
journalctl -u qbit-upload -n 100
```

默认服务名为 `qbit-upload`，执行 `watch --config /etc/qbit-upload.yaml`。本机免认证时不需要 `--env-file`。`--name` 可以覆盖服务名；`--user` 可以指定服务用户，该用户必须能读取配置、凭据文件并写入状态目录。

如果之前部署了独立的 `qbit-upload-filter.service`，先将其 `torrent_filter` 配置合并进 `/etc/qbit-upload.yaml` 并设置 `enabled: true`，保留原 `state_dir` 和相关凭据。备份配置和程序，停止并禁用旧过滤服务，再重启统一服务：

```sh
sudo systemctl disable --now qbit-upload-filter
sudo systemctl restart qbit-upload
```

两部分随同一服务启停，过滤队列的记录继续复用，避免重复处理或自动恢复用户已经暂停的任务。

## 核验和恢复

状态按种子 hash 保存，并用 `added_on` 区分同一 hash 被删除后重新添加的任务。文件索引使用 API 返回的 `index`，不会把数组位置当作文件索引。状态文件通过临时文件、fsync 和原子替换写入，目录加进程锁，避免同一队列被两个服务同时处理。

流程为 `waiting → filtering → verified → start-requested → done`：

- 元数据未到：保留获取元数据的状态，继续处理其他任务。
- 筛选或核验失败：任务保持停止，记录原因并重试。
- 全部文件都小于阈值，或剩余大文件原本也未选中：保持停止。
- 服务在筛选、核验阶段重启：根据保存的文件选择继续核验和处理。
- 启动响应丢失：先回读运行状态；已运行就确认完成，不重复提交启动请求。
- 已提交启动意图但回读仍停止：保留停止并记录原因，需要在 qBittorrent 中手动启动。网络中断或进程退出时无法区分“启动请求没有执行”和“启动后被用户暂停”，因此服务不会盲目重复启动。
- 完成后用户手动暂停：服务不会重新启动该任务。

阈值在首次形成筛选计划时保存；修改配置影响尚未形成计划的新任务，不会重新调整已经处理过的任务。状态目录应保留在 NAS 持久存储上；改变 qBittorrent 地址或管理标签时使用新的状态目录。

可用“一个超过阈值的文件、一个小于阈值的文件”作为验证种子，观察小文件优先级为 0、大文件保留原优先级、任务进入下载或排队状态。元数据未完成时停止/重启过滤服务，再检查它是否继续等待并在元数据到达后完成筛选。

API 和添加参数依据官方 [4.5.0 实现](https://github.com/qbittorrent/qBittorrent/blob/release-4.5.0/src/webui/api/torrentscontroller.cpp)、[5.0.0 实现](https://github.com/qbittorrent/qBittorrent/blob/release-5.0.0/src/webui/api/torrentscontroller.cpp) 和 [WebUI API 文档](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29)。本地 HTTP 模拟测试不代表 NAS 已完成部署或浏览器入口已接入。
