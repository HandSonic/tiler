# 离线转换、S3/MinIO 归档上传与失败补洞

三个子命令均内置于 `tiler`，沿用 Go 1.20.14、CGO SQLite 和 Windows 7 构建目标，不需要额外安装 Python、Node、`mc`、`mb-util` 或 PMTiles CLI。Windows 示例中将 `./tiler` 换为 `.\tiler.exe`。实际 Win7 运行仍需真机验证。

## 1. `convert`：从已有瓦片构建归档

| 输入 | 输出 | 限制 |
| --- | --- | --- |
| XYZ 目录 | `.mbtiles` | 路径为 `{z}/{x}/{y}.{format}`，支持 z0–30 |
| XYZ 目录 | `.pmtiles` | PMTiles v3，支持 z0–26，与官方 Web reader 的坐标范围一致 |
| `.mbtiles` | `.pmtiles` | 正确读取 MBTiles 的 TMS 行号，支持 z0–26 |

暂不支持 PMTiles 输入或 MBTiles → MBTiles。MBTiles 和 PMTiles 的“容器转换”不改变瓦片覆盖范围；不会镶嵌为大图或裁剪边缘图像。

```sh
./tiler convert -input output/tiles -output maps/basemap.mbtiles
./tiler convert -input maps/basemap.mbtiles -output maps/basemap.pmtiles
# 也可从 XYZ 目录直接构建 PMTiles
./tiler convert -input output/tiles -output maps/basemap.pmtiles
# 实际转换图像编码，使用可选归档名称
./tiler convert -input output/tiles -output maps/basemap-jpg.mbtiles -format jpg -name basemap
./tiler convert -h
```

`convert` 不读取 TOML。默认拒绝已存在的目标；`-overwrite` 显式允许在转换完成后替换目标。格式可指定 `png`、`jpg`、`pbf` 或 `webp`。未指定时优先使用元数据中的 `format`，否则从输入瓦片确定；混合格式目录需显式指定目标格式。

- `png`/`jpg` 会解码并真实转码，JPEG 的透明区域合成白色背景。
- `webp` 保留已有 WebP 数据，不提供从其他图像到 WebP 的编码。
- `pbf` 将原始或已 gzip 的输入规范为一层 gzip，不进行矢量与栅格之间的转换。
- 目录中的 `metadata.json` 和无关文件不会作为瓦片。坐标错误、重复坐标、空数据或格式错误会让转换失败。

目录可在根部提供 `metadata.json`，MBTiles 则读取数据库的 `metadata` 表。名称、范围等信息会保留，实际最小/最大级别由瓦片计算。PBF → PMTiles 必须提供有效的 TileJSON `vector_layers`，不会自动猜测矢量图层。例如，XYZ 目录的 `metadata.json`：

```json
{
  "name": "basemap",
  "format": "pbf",
  "bounds": "116.0,39.0,117.0,40.0",
  "vector_layers": [
    {"id": "roads", "fields": {"class": "String"}, "minzoom": 0, "maxzoom": 14}
  ]
}
```

请填写瓦片中实际存在的图层 ID 和字段。也可通过元数据的 `json` 字段提供包含 `vector_layers` 的 JSON；下载时的 `tm.json` 会存入该字段。规范见 [PMTiles v3 JSON metadata](https://github.com/protomaps/PMTiles/blob/main/spec/v3/spec.md#5-json-metadata)。

转换逐瓦片处理，PMTiles 排序使用临时 SQLite 索引。工作目录位于输出所在目录，可能包含临时 MBTiles、索引及归档，因此需预留相应磁盘空间；成功、失败或正常取消后会清理。转换不会解压出散落的 PNG/JPG 文件。直接下载 MBTiles 同样在内存处理后写入数据库，可能短暂出现 SQLite 事务日志。

## 2. `upload`：上传已有归档

先在 `conf.toml` 配置 `[output.s3]`；`upload` 不要求 `output.format` 改成 `s3`，也不读取下载范围。MinIO 示例：

```toml
[output.s3]
bucket = "my-map-tiles"
prefix = "archives"
region = "us-east-1"
endpoint = "http://localhost:9000"
force_path_style = true
timeout_seconds = 60
```

使用 AWS 默认凭证链提供密钥，例如 `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`、可选 `AWS_SESSION_TOKEN` 或 `AWS_PROFILE`。AWS S3 的 `endpoint` 留空，通常使用 `force_path_style = false`。目标 bucket 必须已存在；上传需要目标对象的 `s3:PutObject` 权限，中止失败分段上传还需 `s3:AbortMultipartUpload`。

```sh
./tiler upload -c conf.toml basemap.mbtiles basemap.pmtiles
./tiler upload -c conf.toml -timeout 2h -part-size-mib 16 -concurrency 4 maps
./tiler upload -h
```

所有选项必须放在文件/目录路径之前。显式文件使用其基名作为 key，目录则递归查找 `.mbtiles`、`.pmtiles`，保留相对路径：

| 输入 | S3 对象键（prefix 为 `archives`） |
| --- | --- |
| `maps/basemap.pmtiles`，作为单文件传入 | `archives/basemap.pmtiles` |
| 传入 `maps`，其中有 `region/basemap.pmtiles` | `archives/region/basemap.pmtiles` |

开始网络请求前会检查输入、非空文件和目标 key 冲突；符号链接及特殊文件会被拒绝。扩展名检查不代替归档内容校验，建议上传已成功转换的文件。上传期间不要修改源文件。

上传 MBTiles 前须停止数据库写入并完成 WAL checkpoint；存在非空的同名 `-wal` 文件时会拒绝上传，避免只传主数据库而漏掉尚在 WAL 中的数据。程序不会自动修改源数据库或将 sidecar 一起上传。

归档逐个上传；每个文件的分段可并发。默认每文件总超时 `1h`（包含 SDK 重试），初始分段大小 `8` MiB，每文件并发 `3`。SDK 会在需要时增大分段；`-part-size-mib` 至少为 `5`，`-concurrency` 范围为 `1`–`32`。归档超时由 `-timeout` 控制，`output.s3.timeout_seconds` 仍用于直存模式的单瓦片请求。

同 key 对象在上传完成后被替换，不使用 `output.resume` 跳过已有归档。失败或 Ctrl+C 时尝试在独立的 10 秒清理请求中中止当前分段上传；清理失败也会返回错误。命令报告此前完整上传的文件数与字节数，不回滚已完成的文件，不保存跨进程分段续传状态。

## 3. `repair`：只补失败瓦片

下载中的请求、转码或保存失败会记录为 JSON 清单，任务结束时打印路径及未解决数量。首次下载全部成功时不会另建失败清单；已有清单可被更新为空。

| 下载输出 | 默认本地失败清单 |
| --- | --- |
| `file` | 瓦片根目录内的 `failures.json` |
| `mbtiles` | 数据库完整路径加 `.failures.json`，例如 `output/basemap.mbtiles.failures.json` |
| `s3` | `output.directory/地图名-zmin-max.ID.failures.json`，日志显示实际路径 |

也可在 `[output]` 设置 `failure_file = "output/failures.json"`。路径必须以 `.json` 结尾，不能覆盖文件输出的 `metadata.json`。S3 的清单保存在本地，不作为对象上传。

```sh
./tiler repair -c conf.toml -failures output/basemap.mbtiles.failures.json
# 与下载相同的 p/r/q + Enter 交互控制
./tiler repair -i -c conf.toml -failures output/tiles/failures.json
./tiler repair -h
```

补洞从清单恢复原输出目标，包括原先带随机 ID 的文件目录或 MBTiles 名称；保留原元数据。已经存在的非空瓦片会跳过并从清单消除，只有实际保存成功或确认已存在后才消除失败项。结束后重写同一清单；全部解决时保留空 `tiles` 数组。

对空清单运行 `repair` 会提示没有需要补下载的瓦片，不触发网络请求。

必须使用相同地图名、瓦片格式、来源、级别及输出配置。原本地目录/数据库必须仍存在且类型正确；目标丢失时应恢复完整任务，不能只重建清单中的子集。原本地目标还必须位于当前 `output.directory` 内。S3 校验原 bucket、prefix 以及 endpoint/region 指纹，避免补写到另一个服务。

清单保存坐标、来源指纹、脱敏错误和输出位置，不保存原始来源 URL 或认证密钥。可在 TOML 中更新常见 token/key 等认证参数后补洞；图层、样式等身份参数仍须保持一致。无法匹配来源的清单会被拒绝。

失败清单仅覆盖已尝试但失败的瓦片；Ctrl+C 时尚未调度的范围不会全部加入清单。恢复整个中断任务，请保持原目标并设置 `output.resume = true` 后重新运行下载；`repair` 适合补已知的失败项。
