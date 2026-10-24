# 地图下载器 Tiler

[English](README-EN.md)

按 GeoJSON 范围和缩放级别并发下载地图瓦片，支持本地文件、MBTiles（SQLite）、Compact v2 及 S3 输出。数据源需使用标准 Web Mercator XYZ/TMS 瓦片地址；URL 中使用 `{z}`、`{x}`、`{y}`，需要反转行号时使用 `{-y}`。可通过多个 `[[lrs]]` 为不同级别设置不同范围。

另有 `convert` 在本地/S3 瓦片目录、MBTiles、PMTiles、Compact v2 之间转换和追加，`upload` 将已有归档或 Compact 缓存上传到 S3/MinIO，`repair` 按失败清单补下载，均集成在同一个可执行文件中。最近一轮修复与验证见 [循环审计记录](docs/audit-2026-10-24.md)。

大范围栅格下载可用 `output.format = "compactv2"`：每个 bundle 固定容纳一个级别内最多 128×128 张瓦片，直接写入 bundle，不产生散图片。支持续传、追加、补洞；外部 MapProxy 可把整套目录作为同一图层读取。配置和真实 MapProxy 验证见 [Compact v2 说明](docs/compact-v2.md) 与 [MapProxy 配置示例](docs/mapproxy-compactv2.yaml)。

`lrs.geojson` 支持 FeatureCollection、单个 Feature、裸 Geometry，以及带 `geojson` 几何字段的 Nominatim 数组，无需手工包装为 FeatureCollection。

## 编译与运行

在仓库根目录执行，使用 Go 1.20.14 和 C 编译器（SQLite 依赖 CGO）；AWS SDK 已锁定为兼容 Go 1.20 的版本：

```sh
CGO_ENABLED=1 go build -o tiler .
./tiler -c conf.toml
# 也可直接运行整个包
go run . -c conf.toml
```

请编译整个包，不要用 `go run main.go`；依赖由 `go.mod` 锁定，不要单独 `go get` 升级上游 orb。瓦片覆盖的扩展函数已内置，无需 fork 的私有 API。修改 `conf.toml` 中的 `tm.url`、`tm.format` 和各 `[[lrs]]` 后再运行。相对路径以运行时的工作目录为基准。

面向 Windows 7 构建必须固定 Go 1.20.14，并使用 MSVCRT MinGW 工具链。Linux 交叉编译：

```sh
GO_CMD=/path/to/go1.20.14/bin/go MINGW_ROOT=/path/to/llvm-mingw-msvcrt bash scripts/build-windows.sh
```

Windows 本机 PowerShell（编译器架构须匹配 `-Arch`）：

```powershell
.\scripts\build-windows.ps1 -Arch amd64 -Compiler gcc
# 32 位改为 -Arch 386，并提供对应的 C 编译器
```

脚本生成 `dist/tiler_windows_amd64.exe` 或 `dist/tiler_windows_386.exe`，包含 CGO SQLite 支持。CI 也固定工具链构建两种架构。当前完成的是以 Win7 为目标的构建与检查，仍需真实 Win7 环境运行验证。将程序与 `conf.toml`、`geojson` 目录放在一起，在该目录打开终端运行 `tiler_windows_amd64.exe -c conf.toml`（打包版本中名为 `tiler.exe`）。

## S3 直存

保留原有 `tm`、`lrs`、`task` 配置，将输出改为：

```toml
[output]
format = "s3"
resume = false
tile_path = "{z}/{x}/{y}.{format}"

[output.s3]
bucket = "my-map-tiles"
prefix = "basemap"
region = "ap-northeast-1"
endpoint = ""
access_key_id = ""
secret_access_key = ""
session_token = ""
force_path_style = false
timeout_seconds = 60
```

Bucket 需预先存在，运行身份需有目标对象的 `s3:PutObject` 权限。可在 `[output.s3]` 中明文填写 `access_key_id` 和 `secret_access_key`，临时凭证还需 `session_token`。配置中的凭证优先于环境变量、profile 和 IAM 角色，未填写的 token 不会继承环境变量。三个字段都留空时使用 AWS 默认凭证链：环境变量 `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`、可选的 `AWS_SESSION_TOKEN`，共享配置/`AWS_PROFILE`，或 IAM 角色。只填写一项密钥或只填写 token 会报配置错误。直存、`upload` 和涉及 S3 的 `convert` 共用这些设置；`region` 留空时使用 `AWS_REGION`、`AWS_DEFAULT_REGION` 或 AWS profile。

对象键为 `prefix` 加 `output.tile_path`，默认得到 `basemap/{z}/{x}/{y}.{format}`；例如将模板改为 `{z}/{y}/{x}.{format}` 即可交换目录顺序。空 prefix 直接从模板开始。S3 模式不创建本地瓦片副本；`output.directory` 只用于默认失败清单位置，`output.name` 不影响对象键。`resume = false` 时同 key 会被覆盖；开启恢复时通过 HEAD 跳过非空对象，另需 `s3:GetObject` 权限；AWS S3 还需 `s3:ListBucket` 才能将缺失对象识别为 404（否则可能返回 403 并停止恢复，参见 [HeadObject 权限说明](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)）。

MinIO 等兼容服务可设置 `endpoint = "http://localhost:9000"`、`force_path_style = true`，并提供对应 region 与凭证。下载和上传共享 `task.workers` 并发限制；上传使用 SDK 重试，`output.s3.timeout_seconds` 是单个上传的总超时（含重试），必须为正数。失败会使任务返回错误。

生成 MBTiles 时，瓦片在内存中处理后直接写入 SQLite，不会另存散落的 PNG/JPG 文件。SQLite 运行时可能短暂生成 `-journal` 事务日志，正常完成后关闭数据库。

文件下载会在瓦片根目录生成 `metadata.json`，记录格式、层级、范围和 `tile_path`，供后续转换识别；它不作为下载完成凭证。MBTiles 元数据仍在数据库中。逐瓦片直存 S3 不额外生成该文件；`convert` 导出 S3 瓦片目录时会写元数据。

## 路径模板与追加地区

本地文件和逐瓦片 S3 输出共用 `output.tile_path`：

```toml
[output]
format = "file"
directory = "output"
name = "basemap"
tile_path = "{z}/{y}/{x}.{format}"
append = true
```

模板支持 `{z}`、`{x}`、`{y}`、`{-y}`、`{format}`；`{-y}` 为 `2^z-1-y`。必须包含 z、x 及 y 或 -y，可添加固定子目录、文件名前缀或省略扩展名，例如 `tiles/{z}/{x}_{y}`。路径必须是相对路径，数字坐标之间须有非数字分隔符；`{x}{y}` 会产生坐标歧义，因而被拒绝。MBTiles/PMTiles/Compact v2 的内部存储遵循各自规范，不使用目录模板。

`append = true` 同时启用按瓦片恢复。下载完 A 地区后，保持同一数据源、格式、路径模板和输出目标，把 `[[lrs]]` 范围换成 B 地区再运行：已有非空瓦片保留，新瓦片写入同一数据集。本地文件/MBTiles/Compact v2 必须设置固定 `output.name`，其元数据范围与层级会合并；S3 使用同一 bucket/prefix 追加对象。更换模板应使用新目录或新 prefix，不能沿用旧布局续传。

下载可直接输出文件、MBTiles、Compact v2 或 S3。要累加 PMTiles，可先持续追加 MBTiles 工作库再转换，或用下述 `convert -append` 合并新地区。

## 离线转换、归档上传与补洞

```sh
# XYZ 目录内的路径为 z/x/y.png（也支持 jpg、pbf、webp）
./tiler convert -input output/tiles -output basemap.mbtiles
./tiler convert -input basemap.mbtiles -output basemap.pmtiles
# 将目录里的图像实际转为 JPEG 后写入 MBTiles
./tiler convert -input output/tiles -output basemap-jpg.mbtiles -format jpg
# PMTiles 解包为本地瓦片，并指定输出顺序
./tiler convert -input basemap.pmtiles -output output/zyx -output-type file -tile-path '{z}/{y}/{x}.{format}'
# 从 S3 已有瓦片构建本地归档；从本地归档导出到 S3 瓦片目录
./tiler convert -c conf.toml -input s3://my-map-tiles/basemap/ -output basemap.mbtiles
./tiler convert -c conf.toml -input basemap.pmtiles -output s3://my-map-tiles/zyx/ -output-type file -tile-path '{z}/{y}/{x}.{format}'
# B 地区追加到已有 A 地区归档，重合坐标保留已有瓦片
./tiler convert -input region-b.mbtiles -output basemap.pmtiles -append
# 读取 conf.toml 的 output.s3，上传已有归档；参数必须放在路径前
./tiler upload -c conf.toml -timeout 2h basemap.mbtiles basemap.pmtiles
# 仅重试清单内未解决的瓦片，写回原来的文件、MBTiles 或 S3 目标
./tiler repair -c conf.toml -failures output/basemap.mbtiles.failures.json
```

`convert` 支持本地/S3 瓦片目录、MBTiles、PMTiles v3、Compact v2 之间双向转换；S3 归档地址可写成 `s3://bucket/maps/basemap.pmtiles`，瓦片前缀使用结尾 `/`。纯本地转换默认不读取 TOML，S3 转换用 `-c` 读取 endpoint、region 和凭证配置，bucket/对象路径由 URI 指定。目录元数据中的 `tile_path` 可自动识别，也可用 `-input-tile-path` 指定输入布局，`-tile-path` 指定输出布局。

MBTiles 支持 z0–30，PMTiles 支持 z0–26。默认拒绝已有目标，`-overwrite` 允许替换，`-append` 则保留旧坐标并合并新瓦片。MBTiles/PMTiles 追加先合并再完整重建并替换；已有 Compact 追加只合并新输入涉及的 bundle，S3 只下载相关包、上传改变的包，详见 [追加与中断处理](docs/compact-v2.md#已有数据互转追加和-s3)。本地目录替换会先完整暂存；Compact 追加、散瓦片目录追加和 S3 散瓦片在发布中断后可能已有部分结果完成，可重跑 `-append` 补齐。转换构建不保存跨进程断点。PBF 在归档中规范为一层 gzip，本地目录导出为原始 PBF；PBF 转 MBTiles/PMTiles 还需有效的 `vector_layers`，可从输入元数据或 `-metadata metadata.json` 提供。构建归档使用临时数据库/归档并清理，不会额外解包出零散图片；明确输出文件目录时才生成单张瓦片。

`upload` 还可接收显式 Compact v2 根目录（规则见专门说明）；也可接收多个 `.mbtiles`、`.pmtiles` 文件或目录，递归上传目录内的归档并保留相对路径。默认每个文件超时 1 小时，初始分段 8 MiB、每文件并发 3；上传完成才替换同 key 对象。失败后尝试中止当前分段上传并报告已完成数量，不提供跨进程的分段续传。上传 MBTiles 前须停止写入并完成 WAL checkpoint；存在非空 `-wal` 或活跃回滚事务日志时会拒绝上传。

下载失败时自动写出 JSON 清单，日志会显示路径；可用 `output.failure_file` 指定 `.json` 路径。首次成功的 MBTiles 下载不会额外创建失败清单。补洞沿用清单记录的原目标，必须保持地图名、格式、数据源、级别和目标配置一致；可更新 token 等认证参数。清单记录坐标、来源指纹和脱敏错误，不保存原始数据源 URL 或密钥。Ctrl+C 后尚未调度的范围不在失败清单内，恢复整个中断任务仍应使用 `output.resume = true`。

完整参数、元数据样例和默认清单位置见 [转换、上传与补洞说明](docs/archive-and-repair.md)。Windows 中将 `./tiler` 换为 `.\tiler.exe`；这些子命令不需要额外安装 Python、Node、`mc` 或 PMTiles CLI。

## 恢复、格式与任务控制

- `output.resume = true`：跳过已保存的非空瓦片。文件、MBTiles 和 Compact v2 默认使用稳定基名 `Name-zmin-max`；可用 `output.name` 指定基名。恢复时必须保持数据源与输出路径配置一致。旧版带随机 ID 的产物可把其完整基名填入 `output.name`（MBTiles 不含 `.mbtiles`），或先重命名为新的稳定名称。
- `output.append = true`：在同一目标新增地区，并合并本地目录/MBTiles/Compact v2 的范围和层级；文件、MBTiles 与 Compact v2 需固定 `output.name`。它包含 resume 行为，重合坐标保留已有非空瓦片。`output.tile_path` 同时用于文件/S3 写入、恢复和补洞，变更布局时应更换目标。
- `tm.format = "png"` / `"jpg"` 会实际转换图像编码。PBF 的 `output.gzip_pbf` 未配置时，本地文件默认原始数据，MBTiles/S3 默认 gzip；文件/S3 可显式覆盖；MBTiles 要求 gzip PBF，不能设为 false。S3 会按内容设置 Content-Encoding。
- `task.timeout_seconds = 30`、`task.retries = 3` 控制请求超时和失败重试；重试网络错误及 HTTP 408、429、5xx，遵循标准重定向及 HTTP(S) 代理环境变量。可按数据源设置 `task.user_agent`、`task.referer`。
- `./tiler -i -c conf.toml` 开启交互：输入 `p` / `r` / `q` 后按 Enter 暂停、继续、取消。暂停停止调度新瓦片，已发请求仍会完成。Ctrl+C 取消并保留已存瓦片，下次设置 `resume = true` 重启。
- 任务结束日志输出 `saved`、`skipped`、`failed`，分别表示实际保存成功、已存非空瓦片跳过和瓦片错误；计数可安全用于并发下载。文件恢复的跳过项不会等待 `task.timedelay`。

下载速度同时受数据源、网络、格式转换和磁盘影响。`task.workers` 控制并发，HTTP 空闲连接池随之调整；`task.timedelay` 控制请求启动间隔，示例的 `50` 毫秒最多允许每秒约 20 次启动。在数据源允许时可设为 `0`，再逐步调整并发；出现 429/503 时应降低并发或恢复间隔。

`task.savepipe` 是 MBTiles 写入队列容量，不是写线程数量。写入器将队列中已经到达的瓦片合并为一个事务，队列为空时立即写单片，不等待凑批；提交成功后才计入 `saved`。批量失败时先回滚，再逐片重试，仅记录最终仍失败的瓦片，避免一个坏瓦片连带影响同批其他瓦片。磁盘较慢时增大队列可提高批量写入机会，但也增加内存占用。矩形 GeoJSON（包括示例全球范围）直接计数和流式枚举；PBF 保持完整解压校验，并按目标设置重新编码，gzip 输出统一为单个成员以兼容浏览器读取。上述优化不会跳过瓦片校验或修改请求限速设置。

## 常见问题与能力范围

| 问题 | 说明 |
| --- | --- |
| 只到 11 级、范围不全？ | 实际下载级别和范围由每个 `[[lrs]].min/max/geojson` 决定；`tm.min/max` 元数据会自动取实际层级范围。参见 [#15](https://github.com/atlasdatatech/tiler/issues/15)。 |
| 能存 SQLite？ | 设置 `output.format = "mbtiles"`；MBTiles 本身就是 SQLite 数据库。参见 [#22](https://github.com/atlasdatatech/tiler/issues/22)。 |
| 轮廓裁切？ | GeoJSON 用于选择相交瓦片，边缘瓦片保留完整图像；未实现像素级轮廓裁切。参见 [#17](https://github.com/atlasdatatech/tiler/issues/17)。 |
| 百度、EPSG:4326？ | 暂不支持百度特殊切片规则或 EPSG:4326/天地图 `*_c` 网格；天地图请选择 `img_w`、`vec_w` 等 `*_w` 服务。参见 [#3](https://github.com/atlasdatatech/tiler/issues/3)。 |
| 下载历史年份、镶嵌大图？ | 没有内置年份选择或镶嵌功能；历史影像需数据源提供对应地址。参见 [#19](https://github.com/atlasdatatech/tiler/issues/19)、[#16](https://github.com/atlasdatatech/tiler/issues/16)。 |
| 浏览器可开、程序失败？ | 检查实际瓦片 URL、凭证、代理和 Referer。429/503 可降低 `workers`、增大 `timedelay`；工具重试不能保证修复失效地址或服务端限制。参见 [#18](https://github.com/atlasdatatech/tiler/issues/18)、[#14](https://github.com/atlasdatatech/tiler/issues/14)。 |

常见地址模板：Google `https://mt0.google.com/vt/lyrs=s&x={x}&y={y}&z={z}`；天地图 `https://t0.tianditu.gov.cn/DataServer?T=img_w&x={x}&y={y}&l={z}&tk=YOUR_KEY`；ArcGIS 通常为 `/MapServer/tile/{z}/{y}/{x}`。请使用自己有权限的服务与密钥，确认图层坐标系和实际响应格式。

全部已审阅 Issues 的处理范围见 [问题状态表](docs/issue-status.md)。

Fork 盘点、兼容性风险及本分支独立重写的采纳项见 [Fork 评估与采纳](docs/fork-review.md)。
