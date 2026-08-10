# 番号预处理与影片上报

本文档描述 `qbit-upload` 内置的番号预处理规则，以及影片打包完成后的 Avister Film 异步上报行为。

## 设计目标

- 从带域名、发布组、画质、字幕、时间戳和分段信息的文件名中提取稳定番号。
- 把“核心番号”与分段、画质等元数据分开，避免同一影片生成多条记录。
- 只有成功打包的影片才上报；上报失败不改变归档结果。
- 规则可单元测试、可增量扩展，不依赖某一个超大正则表达式。

实现参考了 [MetaTube SDK Go](https://github.com/metatube-community/metatube-sdk-go)、[JavSP](https://github.com/Yuukiy/JavSP)、[Javinizer](https://github.com/javinizer/Javinizer)、[Javinizer Go](https://github.com/javinizer/javinizer-go) 和 [Movie Data Capture](https://github.com/mvdctop/Movie_Data_Capture) 的公开规则与测试思路。当前代码为本项目独立实现，没有直接引入这些项目的代码或运行时依赖。

## 处理流水线

番号预处理按以下顺序执行：

1. 优先检查文件名；移除最后一个扩展名并转为大写。
2. 屏蔽 `4123.com`、`hhd800.com` 等域名，避免把域名中的字母和数字误识别成番号。
3. 并行收集标准番号及特殊片商格式候选。
4. 按格式可靠度评分，拒绝 `VIDEO-1080`、`H264-1080`、`SAMPLE-1080` 等已知噪声前缀。
5. 生成核心番号；分段、字幕、画质和时间戳保留为结构化元数据，不拼入核心番号。
6. 文件名没有候选时，才从最近一级父目录向上回退。例如 `ABC-123/video.mp4` 使用 `ABC-123`。

上报客户端在发送请求前会再次调用同一预处理函数，确保任何调用路径都不会绕过规范化。

## 支持的核心格式

| 类型 | 输入示例 | 核心番号 |
| --- | --- | --- |
| 标准连字符或下划线 | `cawd-999`、`ATIDD_004` | `CAWD-999`、`ATIDD-004` |
| 紧凑格式 | `WASS644` | `WASS-644` |
| 长前缀 | `MURIKURI-007` | `MURIKURI-007` |
| 语义后缀 | `ABC-123E`、`ABC-123Z` | `ABC-123E`、`ABC-123Z` |
| FC2 | `FC2 PPV 1234567`、`fc2-1234567` | `FC2-1234567` |
| HEYZO | `heyzo_hd_1234` | `HEYZO-1234` |
| HEYDOUGA | `heydouga-4030-123` | `HEYDOUGA-4030-123` |
| 平台型番号 | `getchu_123456`、`mywife-1234` | `GETCHU-123456`、`MYWIFE-1234` |
| DMM CID | `h_123abcd456` | `H_123ABCD456` |
| 数字开头片商 | `133ARA-030`、`18ntrd052` | `133ARA-030`、`18NTRD052` |
| 字母序列号 | `MARRA-A030` | `MARRA-A030` |
| 无码片商型 | `H4610-TK1003`、`XXX-AV-1789` | `H4610-TK1003`、`XXX-AV-1789` |
| 特殊数字前缀 | `T28-1234`、`R18_1234` | `T28-1234`、`R18-1234` |
| 单字母系列 | `N1234`、`K1234` | `N1234`、`K1234` |
| 日期型番号 | `041216_550` | `041216-550` |

平台型规则目前覆盖 `GETCHU`、`GYUTTO`、`GCOLLE`、`PCOLLE` 和 `MYWIFE`。无码片商型规则还覆盖 `H4610`、`H0930`、`C0930`；另有 `MKD`、`MKBD`、`MK3D2DBD`、`S2M`、`S2MBD` 系列的特殊规则。

## 分段与标签

以下内容不会进入上报番号：

| 文件名后缀 | 结构化结果 |
| --- | --- |
| `-A`、`A` | `part=1` |
| `-B`、`B` | `part=2` |
| `-1`、`-2`、`-CD1`、`-PART2` | 对应 `part` |
| `CH`、`C`、`UC` | `chinese-subtitles` 标签 |
| `U`、`UC` | `uncensored` 标签 |
| `[4K]`、`2160P`、`1080P`、`720P` | 对应画质标签 |
| `20260706_112405` | `timestamp` 标签 |

`E` 和 `Z` 被视为番号本身的语义后缀并保留；`A/B` 才作为分段处理。例如 `ABC-123E` 与 `ABC-123-A` 分别得到 `ABC-123E` 和 `ABC-123, part=1`。

## 已确认样例

| 文件名 | 核心番号 | 附加信息 |
| --- | --- | --- |
| `4123.com@CAWD-999.mp4` | `CAWD-999` | - |
| `4221.com@DLD-419.7z` | `DLD-419` | - |
| `hhd800.com@ATIDD-004.7z` | `ATIDD-004` | - |
| `hhd800.com@HODV-22069.7z` | `HODV-22069` | - |
| `hhd800.com@MIRD-275-A.7z` | `MIRD-275` | `part=1` |
| `hhd800.com@MIRD-275-B.7z` | `MIRD-275` | `part=2` |
| `hhsdf0.com@MIRD-275-2.7z` | `MIRD-275` | `part=2` |
| `hhsdf0.com@MIRD-275-1.7z` | `MIRD-275` | `part=1` |
| `hhsdf0.com@MNGS-071_20260706_112405.7z` | `MNGS-071` | `timestamp` |
| `hhsdf0.com@MURIKURI-007.7z` | `MURIKURI-007` | - |
| `HNDF-051.7z` | `HNDF-051` | - |
| `SONE-483.[4k]@RUNBKK.7z` | `SONE-483` | `4k` |
| `WASS-644ch.7z` | `WASS-644` | `chinese-subtitles` |

纯时间戳、纯发布组名称和仅含清晰度的名称不会被识别。例如 `RUNBKK.7z`、`20260706_112405.mp4` 和 `video1080p.mp4` 均无结果。无法可靠识别时，程序记录警告并跳过上报，不会把整个文件名当作番号。

## 默认上报行为

普通归档和 `watch` 模式默认开启上报；`thumbnail` 子命令不打包，因此不会上报。

默认接口：

```text
POST http://89.116.88.182:8081/api/external/films
```

请求使用 `multipart/form-data`：

- `code`：预处理后的核心番号。
- `previewFile`：该影片生成的 JPEG 缩略图长图。
- `Key`：独立 API Key 请求头。

一部影片有多个分段时，程序会等待本次任务中该核心番号的所有待处理文件都成功归档，再加入上报队列。不同番号可以在后续归档继续执行时并行上报，后台请求并发上限为 `4`。同一任务内的相同核心番号只请求一次；程序退出或删除源文件前会等待已经进入队列的请求结束。

上报是尽力行为：

- `201 Created`：上报成功。
- `409 Conflict`：番号已经存在，按幂等成功记录。
- `502 Bad Gateway`：影片记录可能已经创建而图片上传失败，不自动重试，避免再次创建。
- 其他网络错误或非预期状态：记录警告，不回滚、不删除已完成归档，也不自动重试。
- 缩略图不存在、读取失败、类型不支持或超过接口的 `8 MiB` 限制：记录警告并只上报番号。
- 番号无法识别：不发送请求。
- API Key 未配置：默认行为仍为“已开启”，但会明确警告并跳过请求，归档流程继续。

`--dry-run` 会显示预计上报的核心番号和预览图路径，但不会发起网络请求。

## 配置

推荐通过环境变量提供 Key，避免密钥写入仓库或出现在进程参数中：

PowerShell：

```powershell
$env:QBIT_UPLOAD_REPORT_API_KEY = "replace-with-api-key"
qbit-upload --config qbit-upload.yaml <source-dir>
```

Linux/macOS：

```bash
export QBIT_UPLOAD_REPORT_API_KEY='replace-with-api-key'
qbit-upload --config qbit-upload.yaml <source-dir>
```

配置文件：

```yaml
report:
  enabled: true
  url: http://89.116.88.182:8081/api/external/films
  timeout: 30s
  # 不推荐明文保存；仅在环境变量不可用时设置。
  # api_key: "replace-with-api-key"
```

API Key 的优先级从高到低为：

1. `--report-api-key`
2. `report.api_key`
3. `QBIT_UPLOAD_REPORT_API_KEY`
4. `AVISTER_FILM_EXTERNAL_API_KEY`

完整命令行参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--report` | `true` | 是否在影片打包完成后上报 |
| `--report-url` | Avister Film TOK 地址 | 覆盖接口地址 |
| `--report-api-key` | 空 | 覆盖 API Key；不推荐用于长期运行 |
| `--report-timeout` | `30s` | 单次请求超时 |

需要临时关闭时：

```powershell
qbit-upload --report=false <source-dir>
```

或在配置中设置：

```yaml
report:
  enabled: false
```

## 扩展规则

新增格式时，应同步增加表驱动单元测试。优先增加独立的高置信格式规则或噪声词，不要放宽通用规则到会匹配画质、域名或时间戳。现有回归测试位于：

- `cmd/catalog_number_test.go`
- `cmd/report_test.go`
