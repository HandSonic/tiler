# 地图下载器 Tiler

[English](README-EN.md)

按 GeoJSON 范围和缩放级别并发下载地图瓦片，支持本地文件、MBTiles（SQLite）及 S3 输出。数据源需使用标准 Web Mercator XYZ/TMS 瓦片地址；URL 中使用 `{z}`、`{x}`、`{y}`，需要反转行号时使用 `{-y}`。可通过多个 `[[lrs]]` 为不同级别设置不同范围。

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

[output.s3]
bucket = "my-map-tiles"
prefix = "basemap"
region = "ap-northeast-1"
endpoint = ""
force_path_style = false
timeout_seconds = 60
```

Bucket 需预先存在，运行身份需有目标对象的 `s3:PutObject` 权限。凭证使用 AWS 默认凭证链：环境变量 `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`、可选的 `AWS_SESSION_TOKEN`，共享配置/`AWS_PROFILE`，或 IAM 角色。无需将密钥写入 TOML；`region` 留空时使用 `AWS_REGION`、`AWS_DEFAULT_REGION` 或 AWS profile。

对象键为 `basemap/{z}/{x}/{y}.{format}`；空前缀时直接使用 `{z}/{x}/{y}.{format}`。S3 模式不创建本地副本，忽略 `output.directory` 和 `output.name`。`resume = false` 时同 key 会被覆盖；开启恢复时通过 HEAD 跳过非空对象，另需 `s3:GetObject` 权限；AWS S3 还需 `s3:ListBucket` 才能将缺失对象识别为 404（否则可能返回 403 并停止恢复，参见 [HeadObject 权限说明](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)）。

MinIO 等兼容服务可设置 `endpoint = "http://localhost:9000"`、`force_path_style = true`，并提供对应 region 与凭证。下载和上传共享 `task.workers` 并发限制；上传使用 SDK 重试，`output.s3.timeout_seconds` 是单个上传的总超时（含重试），必须为正数。失败会使任务返回错误。

生成 MBTiles 时，瓦片在内存中处理后直接写入 SQLite，不会另存散落的 PNG/JPG 文件。SQLite 运行时可能短暂生成 `-journal` 事务日志，正常完成后关闭数据库。

仅文件输出会在瓦片根目录生成 `metadata.json`，可查看格式、层级、范围等信息；它不作为下载完成凭证。MBTiles 元数据仍在数据库中，S3 不额外生成该文件。

## 恢复、格式与任务控制

- `output.resume = true`：跳过已保存的非空瓦片。文件和 MBTiles 默认使用稳定基名 `Name-zmin-max`；可用 `output.name` 指定基名。恢复时必须保持数据源与输出路径配置一致。旧版带随机 ID 的产物可把其完整基名填入 `output.name`（MBTiles 不含 `.mbtiles`），或先重命名为新的稳定名称。
- `tm.format = "png"` / `"jpg"` 会实际转换图像编码。PBF 的 `output.gzip_pbf` 未配置时，本地文件默认原始数据，MBTiles/S3 默认 gzip；显式配置可覆盖默认值，S3 会按内容设置 Content-Encoding。
- `task.timeout_seconds = 30`、`task.retries = 3` 控制请求超时和失败重试；重试网络错误及 HTTP 408、429、5xx，遵循标准重定向及 HTTP(S) 代理环境变量。可按数据源设置 `task.user_agent`、`task.referer`。
- `./tiler -i -c conf.toml` 开启交互：输入 `p` / `r` / `q` 后按 Enter 暂停、继续、取消。暂停停止调度新瓦片，已发请求仍会完成。Ctrl+C 取消并保留已存瓦片，下次设置 `resume = true` 重启。
- 任务结束日志输出 `saved`、`skipped`、`failed`，分别表示实际保存成功、已存非空瓦片跳过和瓦片错误；计数可安全用于并发下载。文件恢复的跳过项不会等待 `task.timedelay`。

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
