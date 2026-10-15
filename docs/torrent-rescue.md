# 停滞种子自动补救

`qbit-upload watch` 在 `torrent_rescue.enabled: true` 时增加独立的补救循环。旧配置默认关闭。沿用 `torrent_filter` 的 WebUI 地址和凭据，即使大小过滤关闭也可以配置这组连接参数。检索通过 Sukebei Nyaa RSS 独立执行，不依赖 qBittorrent 搜索插件；本版内置此主源，JavDB/BTDig 尚未接入在线检索。

## 判定与下载

每 15 分钟检查 qBittorrent。默认根据任务的 `added_on` 判断年龄：添加达到 240 小时、尚未完成且处于 `stalledDL` 的任务，在首次检查就进入补救，无需先累计 24 小时观察。按原任务的添加时间从早到晚调度，优先处理等待最久的番号。旧配置中的 `no_progress` 保留读取兼容，不再影响补救准入。暂停、排队、校验、磁盘错误及正在下载的任务不作为新补救目标。

仍记录 `completed` 字节数用于候选试下载的实测评分。首次观察、服务/API 观察中断、校验导致完成数据回退及删除后重加会刷新候选进展记录，避免把观察中断误判为试下载失败；上传会刷新的 `last_activity` 不参与判定。

从名称提取规范番号，严格匹配 RSS 标题，按 infohash 去重，排除已经存在的任务。候选只按影片本身的质量选择：字幕、标题中的 4K/1080p 声明、无码等版本标签不作准入条件或加分项；实际画面高度在完成后用 ffprobe 核验。旧配置的 `preserve_labels` 保留读取兼容但不再生效。升级时自动清除旧补救组的标签限制，并重新搜索因此被排除的候选。

初筛每次最多收集 10 个候选，参考站点做种数、历史完成数、trusted、大小和 remake 标记。未知或负数统计保留为 `null`，不当作零或可信做种数。搜索页总大小只用于安排元数据检查顺序，不作为正片质量证据。每个番号最多 3 个获取元数据或试下载的活动任务，全局最多 6 个；每轮最多检索 2 个番号，每个番号每天最多一次成功检索。网络/HTTP/验证页/无效 RSS 是来源错误，与有效空结果分别记录。

新候选使用 `qbit-rescue` 标签、分类及独立 `candidate_save_path/番号/hash` 目录；不传入 `filter-small` 标签。添加时获取元数据后停止，检查番号、视频大小、文件索引和选择状态，排除附件并回读，再进入 `ready` 等待质量调度。因此既有大小过滤器不会抢占补救任务的启动阶段。其他番号或无法核实归属的大视频会使整条磁力失败，即使它们已经被取消选择；sample/trailer 不作为正片。多个视频只接受同一番号、从 1 开始连续编号且全部选择的分段，最多 8 段；多份完整版本、缺段或大量视频均拒绝。100 个不同影片、每个 1 GiB 的合集不会按 100 GiB 算成高质量影片。

质量优先级按元数据核实的正片大小分三档：默认 16–24 GiB（约 20 GiB）优先，其次 4 GiB 以上，最后小于 4 GiB 的后备。合法分段只累计这一部影片的各段，图片、文本、广告和其他附件不计入正片大小。同档优先接近 20 GiB 的版本，再比较来源初筛分数；同档可并行试下载。`preferred_video_size_gb: 4` 和 `ideal_video_size_gb: 20` 可修改上述基准。体积是质量偏好指标，无法证明码率、画质或内容身份。

更高档候选仍在等待元数据或试下载时，小版本保持停止并留作后备；更高档候选被淘汰后自动启用后备。后来发现更高档候选时，暂停当前较低档试下载，保留已有数据并腾出检查名额。低档版本先完成也会等待更高档候选的结果。获取元数据的等待受 1 小时超时限制，试下载沿用下述实测淘汰规则。

试下载的实测评分为：分片可获得程度 35、近期有效增长 30、完成字节增量估算的平均速度/剩余时间 15、实际连接的做种者 10、已通过正片准入 10。分片可获得程度不足完整覆盖时只给 10 分；完全不可见不给分。6 小时无增长且分数低于 40 时暂停并补位；无增长达到 24 小时也会暂停。排队、校验、网络断开及观察中断不会直接算作连续停滞。

尚未交付的补救组中，失败记录保存 7 天冷却时间；已有的失败任务保持暂停和留存，不会被自动重新加入或恢复。未确认添加的请求最多等待元数据超时窗口，随后释放试下载名额。留存的候选文件计入 200 GiB 预算，额度耗尽后保持等待。未知大小按 32 GiB 预留，另保留 20 GiB 可用磁盘空间。

## 完成回调与唯一交付

启用前将 [qbit-rescue-hook.py](../scripts/qbit-rescue-hook.py) 安装到 qBittorrent 容器可读的 `/config/qbit-rescue-hook.py`。容器需要 Python 3，并允许回调通过本机 WebUI 读取该任务。完成时设置：

```text
python3 /config/qbit-rescue-hook.py "%F" "%N" "%I"
```

原来的 `/config/qbit-move.sh` 保留。未管理的任务仍由 `/bin/bash /config/qbit-move.sh` 处理；管理记录按 `hash + added_on` 判定，删除后重加的任务不继承旧记录。回调读取失败时保留完成数据，供 host worker 恢复。只有带补救标签的待确认候选才使用 `added_on=0` 的添加意图记录。

补救 worker 先确认目标文件全部完成，检查普通文件、路径边界和元数据大小，再使用 ffprobe 检查画面尺寸/时长，并在 10%、50%、90% 三处进行视频帧解码。该检查不是全片逐帧解码，也不能证明内容身份或内嵌字幕语义。

优先质量档中第一个通过检查的原任务或候选成为唯一成功结果；较低档的完成副本先保留，等待更高档试下载被淘汰后才交付。worker 在监听目录旁建立暂存目录，复制所选视频及相对路径，fsync 后原子发布；监听器还要看到持久化的 `published` 状态才能处理。重启可恢复发布到一半的状态；归档删除交付副本后不会再次复制。

交付提交成功后，自动删除同组其他未完成的 qBittorrent 任务及其下载数据，包括未完成的原任务；候选的独立目录和残留 `.!qB` 文件也会清理。删除后回读确认任务已消失，再移除其候选记录、进展观察和完成回调 guard，最终只保留一条 `winner` 记录。尚未加入的候选记录也会移除。已有 `published` 组在升级或重启后的首次检查中补做清理。

每次仅操作一个 infohash，并检查 `added_on`、候选标签和独立保存路径；身份或归属已改变的任务只移除旧补救记录。清理意图在删除前持久化，响应丢失、延迟删除或重启均通过回读继续恢复。另一个已经完整下载的副本只移除任务和候选记录，其文件保留；与成功版本或其他任务共享文件、含符号链接等无法安全清理的数据也保留。不会递归删除原任务共享的下载根目录。成功版本的任务、原下载文件和 guard 保留。带原 guard 且身份匹配的历史候选也会纳入清理；未完成的其他补救组继续正常试下载。

已纳入补救的番号由这一入口统一交付；同番号的遗留普通复制项留存并跳过自动归档，避免回调接管瞬间的竞争导致重复处理。若需另行归档同番号的新版本，应先明确解除该补救组的管理；不要直接编辑正在运行的状态文件。

## 配置与命令

从 `qbit-upload.example.yaml` 的 `torrent_rescue` 段开始配置，NAS 主机目录需根据真实 Docker 挂载填写。例如：

```yaml
torrent_filter:
  base_url: http://127.0.0.1:8080
torrent_rescue:
  enabled: true
  proxy_url: http://127.0.0.1:7890
  state_dir: /var/lib/qbit-upload/torrent-rescue
  guard_dir: /srv/qbittorrent/config/qbit-rescue/guards
  download_path_prefix: /downloads
  download_host_dir: /srv/qbittorrent/downloads
  candidate_save_path: /downloads/qbit-rescue
  publish_dir: /srv/qbittorrent/config/tt
```

`guard_dir` 必须对应容器中的 `/config/qbit-rescue/guards`，发布目录必须是现有 `watch.dirs` 中的一项。候选目录必须位于下载映射内。状态目录加进程锁并原子写入；改变 qBittorrent 实例时使用新的状态目录。代理仅作用于 RSS HTTP 客户端；`QBIT_UPLOAD_SEARCH_PROXY` 可覆盖 `proxy_url`。URL 不接受内嵌凭据，避免日志泄露。

```sh
# 独立检索，输出可复核的磁力和评分证据，不创建下载。
qbit-upload search-torrents NAMH-070 --config /etc/qbit-upload.yaml

# 服务运行期间可以读回计划，既不写状态，也不修改任务。
qbit-upload rescue-torrents --once --dry-run --config /etc/qbit-upload.yaml

# 常驻功能随 watch 启停。
qbit-upload watch --config /etc/qbit-upload.yaml

# 优先调度某个符合添加年龄和停滞状态要求的番号。
# 会创建试下载；与统一服务互斥，先停止服务，执行后再恢复。
qbit-upload rescue-torrents --once --force-code NAMH-070 --config /etc/qbit-upload.yaml
```

`--force-code` 作为优先调度参数保留，仍要求原任务达到年龄阈值且为 `stalledDL`，不会恢复用户暂停的任务；其他符合条件的旧任务也会进入补救。请求响应丢失后回读已添加/已启动状态，避免重复添加或盲目重启。任务的 added_on、保存路径或所有权标签发生变化时退出自动管理。

测试：`go test -race ./...`，`go vet ./...`，`python3 -m unittest discover -s scripts -p '*_test.py'`。真实媒体校验测试在 ffmpeg/ffprobe 可用时执行，否则跳过；HTTP 模拟测试不代表 NAS 已下载完真实资源。
