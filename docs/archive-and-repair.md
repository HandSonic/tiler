# 离线转换、S3/MinIO 归档上传与失败补洞

三个子命令均内置于 `tiler`，沿用 Go 1.20.14、CGO SQLite 和 Windows 7 构建目标，不需要额外安装 Python、Node、`mc`、`mb-util` 或 PMTiles CLI。Windows 示例中将 `./tiler` 换为 `.\tiler.exe`。实际 Win7 运行仍需真机验证。

## 1. `convert`：转换已有数据与追加地区

下列输入可转换为任一种输出。S3 地址使用同一份连接配置，URI 指定 bucket 和对象键或前缀。

| 数据形式 | 本地地址 | S3 地址 |
| --- | --- | --- |
| Compact v2 缓存 | `output/world-compact` | `s3://bucket/world-compact/` |
| 单张瓦片目录 | `output/tiles` | `s3://bucket/tiles/` |
| MBTiles 归档 | `maps/basemap.mbtiles` | `s3://bucket/maps/basemap.mbtiles` |
| PMTiles v3 归档 | `maps/basemap.pmtiles` | `s3://bucket/maps/basemap.pmtiles` |

支持归档互转、归档导出散瓦片，以及散瓦片构建归档。Compact v2 的 PNG/JPEG、256/512 像素限制、转换/上传语义和 MapProxy 配置见 [Compact v2 说明](compact-v2.md)。PMTiles 输入支持 v3 的 PNG/JPEG/WebP/MVT，支持无压缩和 gzip；Brotli、Zstd、AVIF 会明确报错。MBTiles 支持 z0–30；PMTiles 支持 z0–26，与官方 Web reader 的坐标范围一致。转换正确处理 MBTiles 的 TMS 行号和 PMTiles 的内部索引，不改变瓦片覆盖范围，也不会镶嵌大图或裁剪边缘图像。

当前 PMTiles 写出器单档最多支持 536,608,768 张瓦片，来自最多 512 个根目录条目与读取时同时存活的 1,048,576 个目录条目预算，不是 PMTiles 格式上限。全球 z0–14 共 357,913,941 张在此数量范围内，z0–15 共 1,431,655,765 张会超限并明确报错。更大任务应拆分归档或使用 Compact v2；此数量限制不代表磁盘容量或 Win7 内存足够运行。

```sh
# 本地目录、MBTiles、PMTiles 互转
./tiler convert -input output/tiles -output maps/basemap.mbtiles
./tiler convert -input maps/basemap.mbtiles -output maps/basemap.pmtiles
./tiler convert -input maps/basemap.pmtiles -output maps/restored.mbtiles
./tiler convert -input maps/basemap.pmtiles -output output/zyx -output-type file -tile-path '{z}/{y}/{x}.{format}'
# 更改实际图像编码或输入布局
./tiler convert -input output/tiles -output maps/basemap-jpg.mbtiles -format jpg -name basemap
./tiler convert -input output/zyx -input-tile-path '{z}/{y}/{x}.{format}' -output maps/from-zyx.mbtiles
# S3/MinIO 瓦片与归档转换
./tiler convert -c conf.toml -input s3://my-map-tiles/basemap/ -output maps/basemap.mbtiles
./tiler convert -c conf.toml -input maps/basemap.pmtiles -output s3://my-map-tiles/zyx/ -output-type file -tile-path '{z}/{y}/{x}.{format}'
./tiler convert -c conf.toml -input s3://my-map-tiles/maps/basemap.pmtiles -output maps/restored.mbtiles
./tiler convert -c conf.toml -input maps/basemap.mbtiles -output s3://my-map-tiles/maps/basemap.pmtiles
./tiler convert -h
```

Windows PowerShell 可直接使用示例中的单引号模板；将 `./tiler` 换为 `.\tiler.exe`。纯本地转换默认不读 TOML；涉及 S3 时用 `-c conf.toml` 提供 endpoint、region 和凭证配置，未传 `-c` 时默认读取 `conf.toml`。URI 中的 bucket 和路径优先于配置中的 bucket/prefix。S3 散瓦片前缀请以 `/` 结尾，以免与归档对象混淆。读取 S3 散瓦片需要列举该前缀及读取对象的权限。导出 S3 散瓦片也会读取元数据、列举现有对象、HEAD 检查 key，再写入瓦片，需相应的 `s3:ListBucket`、`s3:GetObject`、`s3:PutObject` 权限。S3 归档追加还需读取原归档；分段失败清理需 `s3:AbortMultipartUpload`。

| 参数 | 用途 |
| --- | --- |
| `-input` / `-output` | 必填输入和输出路径或 S3 URI |
| `-output-type file\|mbtiles\|pmtiles\|compactv2` | 显式指定输出形式；新 Compact 目录须显式指定；已有 Compact 由 conf.xml 识别，其余目录默认散瓦片，归档由扩展名识别 |
| `-input-tile-path` | 显式输入目录布局；未提供时读取 `metadata.json` 的 `tile_path`，再回落到默认 XYZ |
| `-tile-path` | 输出散瓦片布局；本地默认 XYZ，S3 未显式指定时先使用配置中的 `output.tile_path`，再默认 XYZ |
| `-format png\|jpg\|pbf\|webp` | 目标编码；混合格式输入需明确选择目标 |
| `-name` | 可选归档名称 |
| `-metadata FILE` | 读取额外 JSON 对象（最多 16 MiB），例如 S3 散瓦片所需的矢量 `vector_layers` |
| `-overwrite` | 显式替换归档/本地目录，或覆盖 S3 前缀中的同 key 瓦片 |
| `-append` | 保留已有坐标，添加新地区并合并范围和层级 |
| `-c` | S3 连接配置文件 |

默认拒绝已有目标。`-overwrite` 用于替换，`-append` 用于扩展已有数据集；二者不能同时使用。转换不会把一个归档误当作新输入覆盖自身，请将 B 地区作为输入、A 地区已有数据集作为输出：

```sh
# 把 B 地区合入已有 A 地区归档
./tiler convert -input region-b.mbtiles -output basemap.mbtiles -append
./tiler convert -input region-b.mbtiles -output basemap.pmtiles -append
# 也可向 S3 的既有归档或散瓦片前缀追加
./tiler convert -c conf.toml -input region-b.pmtiles -output s3://my-map-tiles/maps/basemap.pmtiles -append
./tiler convert -c conf.toml -input region-b.mbtiles -output s3://my-map-tiles/basemap/ -output-type file -append
```

追加时重合坐标保留旧的非空瓦片，新增坐标加入同一数据集；使用相同地图图层、网格与目标格式。MBTiles/PMTiles 归档转换会先在临时文件合并，再完整生成新归档并替换原文件。PMTiles 不进行原位增量修改。S3 归档在本地生成完整结果后分段上传，完成后替换对象。本地目录的新建或 `-overwrite` 先在临时目录完整导出，再发布结果；`-overwrite` 替换整个原目录。本地散瓦片目录 `-append` 和 S3 散瓦片输出逐个保存，中断时可能已有部分文件或对象写入。S3 散瓦片前缀的 `-overwrite` 只更新输入坐标对应的 key，不删除前缀中其他对象；其元数据范围合并旧数据和新数据。已有 S3 前缀的模板或格式不一致时仍会拒绝，应改用新 prefix。

`convert` 不持久化构建进度，也不保存跨进程 S3 分段状态。重新执行归档转换需要重新构建；散瓦片目标可用 `-append` 保留已完成的非空瓦片。不要让多个进程同时修改同一个目标。

向已有散瓦片目录或 S3 前缀追加时，旧路径必须与模板的规范输出一致。带前导零的坐标、大写扩展名或 `.jpeg` 别名等旧路径，可先转换到一个新目录或前缀完成规范化，再继续追加；普通输入转换仍接受这些旧路径。

### 路径模板

下载的本地文件和逐瓦片 S3 输出共用配置：

```toml
[output]
tile_path = "{z}/{y}/{x}.{format}"
```

本地最终路径是 `output.directory/output.name/模板`；S3 对象键是 `output.s3.prefix/模板`。未设置时采用 `{z}/{x}/{y}.{format}`。`convert` 则用 `-input-tile-path` / `-tile-path`，不依赖下载任务中的地图范围。

| 模板 | 示例用途 |
| --- | --- |
| `{z}/{x}/{y}.{format}` | 默认 XYZ |
| `{z}/{y}/{x}.{format}` | 交换 x/y 目录顺序 |
| `{z}/{x}/{-y}.{format}` | 使用 TMS 行号 `2^z-1-y` |
| `tiles/{z}/{x}_{y}` | 固定子目录、自定义文件名、无扩展名 |

必须含 `{z}`、`{x}`，并且包含 `{y}` 或 `{-y}` 中的一种；`{format}` 可选。只接受这些占位符。数字坐标占位符之间必须有非数字分隔符，拒绝 `{x}{y}`、`{x}0{y}` 等会让不同坐标生成同一路径的模板。路径必须相对根目录，不能使用 `..`、绝对路径、反斜杠、Windows 保留名称或非法文件名字符。

本地目录的 `metadata.json` 会记录 `tile_path`，转换时可自动识别。恢复、追加、补洞必须使用同一模板；更换模板时应使用新目录或新 S3 prefix。旧失败报告没有 `tile_path` 时按默认 XYZ 处理。MBTiles/PMTiles/Compact v2 内部没有目录布局设置。

新写入的 S3 单张瓦片会附带模板、XYZ 坐标和格式的对象 metadata，恢复时据此检查。默认 XYZ 可兼容旧的无标记对象；自定义布局的旧对象若没有这些标记，直接下载恢复无法确认其布局，会明确拒绝。`convert` 在前缀 `metadata.json` 已确认模板和格式一致时，可以追加外部自定义布局的数据。

读取逐瓦片直存的 S3 自定义布局时，如前缀下没有 `metadata.json`，需用 `-input-tile-path '{z}/{y}/{x}.{format}'` 等参数明确原模板。转换也会核对对象上的模板、坐标和格式标记；不匹配会报错，避免静默交换 x/y。

S3 输入支持未编码对象及 `Content-Encoding: gzip` 的瓦片、元数据和归档。HTTP gzip 会先解码，瓦片与元数据仍同时检查编码和解码后的大小；归档流式解码。其他 HTTP 内容编码会明确报错。PBF 在归档中的内容压缩规则保持不变。

### 格式、元数据和临时文件

- `png`/`jpg` 会解码并真实转码，JPEG 的透明区域合成白色背景。
- `webp` 用真实解码器验证，可保留 WebP 或转为 PNG/JPEG；不提供其他图像到 WebP 的编码，动画 WebP 明确拒绝。
- `pbf` 接受原始或已 gzip 输入；归档中规范为一层 gzip，本地散瓦片导出为原始 PBF。拒绝矢量与栅格之间的伪转换。
- 目录中的 `metadata.json`、失败清单等不会当成瓦片。坐标错误、重复坐标、空数据或格式错误会让转换失败。

为控制 Win7 等环境的内存使用，单张瓦片的编码数据和解压后数据各限制为 64 MiB；PNG/JPEG/WebP 解码前还会检查像素数，上限为 16×1024×1024 像素。PBF 最多解开四层嵌套 gzip，再按目标规范为一层 gzip 或原始数据。超出大小、像素或嵌套层数限制时会明确报错。

目录读取根部 `metadata.json`，MBTiles 读取 `metadata` 表，PMTiles 读取其 JSON metadata。可用 `-metadata FILE` 补充输入元数据。PBF → MBTiles/PMTiles 必须提供有效的 `vector_layers`，不会自动猜测图层。MBTiles 将其写入 `json` 元数据，字段类型须为 `String`、`Number` 或 `Boolean`，图层 min/maxzoom 与实际导出级别取交集；散瓦片不强制要求矢量图层描述。例如：

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

请填写瓦片中实际存在的图层 ID 和字段。也可在元数据的 `json` 字段提供包含 `vector_layers` 的 JSON；下载时 `tm.json` 会存入该字段。`vector_layers` 追加仍合并图层描述，显式指定时替换整个描述。规范见 [PMTiles v3 JSON metadata](https://github.com/protomaps/PMTiles/blob/main/spec/v3/spec.md#5-json-metadata)。

本地/S3 目录的 `metadata.json`、下载恢复及 `convert -metadata` 都保留自定义布尔值、数组、对象、数字和 null 的 JSON 类型。追加时自定义字段保留旧键值（包括空字符串）、补入新键；显式 `-metadata` 按键覆盖源数据及配置中的 `tm.json`，未提及的键保留。

构建归档逐瓦片处理，排序使用临时 SQLite 索引。可能同时存在临时 MBTiles、索引及归档，需预留相应磁盘空间；成功、失败或正常取消后清理。S3 输入也通过临时归档/数据库处理，不额外落单张图片。只有明确输出散瓦片目录时才生成单张图片。直接下载 MBTiles 同样在内存处理后写入数据库，可能短暂出现 SQLite 事务日志。

### 下载完 A 地区后继续追加 B 地区

直接下载任务可设置：

```toml
[output]
format = "mbtiles"
directory = "output"
name = "basemap"
append = true
```

`append = true` 蕴含 `resume = true`。文件/MBTiles/Compact v2 必须指定固定 `output.name`，否则更改地区或层级后自动名称可能指向另一目标。首次目标不存在时可创建；下载完 A 后，只更换 `[[lrs]]` 的 GeoJSON/级别来选择 B，保持地图图层、格式和目标配置再运行。已有非空瓦片跳过，本地文件/MBTiles/Compact v2 的总范围、最小/最大级别合并，不缩小为 B 的范围；repair 保留原元数据。

S3 直存以 bucket/prefix 和 `tile_path` 定位同一数据集，不要求 `output.name`。PMTiles 暂不作为直接下载输出：可在 MBTiles 工作库中持续追加，再转换为 PMTiles，或使用上面的 `convert -append` 合并。

## 2. `upload`：上传已有归档

显式 Compact v2 根目录也可上传，使用独立前缀并保留标准缓存路径；具体文件筛选、覆盖和中断语义见 [Compact v2 说明](compact-v2.md)。

先在 `conf.toml` 配置 `[output.s3]`；`upload` 不要求 `output.format` 改成 `s3`，也不读取下载范围。MinIO 示例：

```toml
[output.s3]
bucket = "my-map-tiles"
prefix = "archives"
region = "us-east-1"
endpoint = "http://localhost:9000"
access_key_id = "YOUR_ACCESS_KEY_ID"
secret_access_key = "YOUR_SECRET_ACCESS_KEY"
session_token = ""
force_path_style = true
timeout_seconds = 60
```

上例可直接明文填写密钥；临时凭证需填写 `session_token`。这组配置优先于 AWS 默认凭证链，不会混用环境变量中的 token。三个凭证字段都留空时使用默认链，例如 `AWS_ACCESS_KEY_ID`、`AWS_SECRET_ACCESS_KEY`、可选 `AWS_SESSION_TOKEN` 或 `AWS_PROFILE`；不允许只填部分密钥。直存、上传和转换均使用同一凭证入口。AWS S3 的 `endpoint` 留空，通常使用 `force_path_style = false`。目标 bucket 必须已存在；上传需要目标对象的 `s3:PutObject` 权限，中止失败分段上传还需 `s3:AbortMultipartUpload`。

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

上传 MBTiles 前须停止数据库写入并完成 WAL checkpoint；存在非空的同名 `-wal` 文件或活跃回滚事务日志时会拒绝上传，避免只传主数据库而漏掉尚在 WAL 中的数据。程序不会自动修改源数据库或将 sidecar 一起上传。

归档逐个上传；每个文件的分段可并发。默认每文件总超时 `1h`（包含 SDK 重试），初始分段大小 `8` MiB，每文件并发 `3`。SDK 会在需要时增大分段；`-part-size-mib` 至少为 `5`，`-concurrency` 范围为 `1`–`32`。归档超时由 `-timeout` 控制，`output.s3.timeout_seconds` 仍用于直存模式的单瓦片请求。

同 key 对象在上传完成后被替换，不使用 `output.resume` 跳过已有归档。失败或 Ctrl+C 时尝试在独立的 10 秒清理请求中中止当前分段上传；清理失败也会返回错误。命令报告此前完整上传的文件数与字节数，不回滚已完成的文件，不保存跨进程分段续传状态。

## 3. `repair`：只补失败瓦片

下载中的请求、转码或保存失败会记录为 JSON 清单，任务结束时打印路径及未解决数量。首次下载全部成功时不会另建失败清单；已有清单可被更新为空。

向同一目标恢复或追加时，会沿用已有失败清单；未在本次范围内处理的旧失败项仍会保留。例如追加 B 地区不会抹掉 A 地区尚未解决的失败项。复用清单路径时地图名、格式和目标必须一致，否则会拒绝运行；不同数据集应另设 `output.failure_file`。要用 `repair` 解决保留的旧项，配置仍须包含它们对应的数据源和级别。

| 下载输出 | 默认本地失败清单 |
| --- | --- |
| `file` / `compactv2` | 瓦片或缓存根目录内的 `failures.json` |
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

必须使用相同地图名、瓦片格式、来源、级别及输出配置。原本地目录/数据库必须仍存在且类型正确；目标丢失时应恢复完整任务，不能只重建清单中的子集。原本地目标还必须位于当前 `output.directory` 内。文件/S3 还会校验原路径模板；S3 校验原 bucket、prefix 以及 endpoint/region 指纹，避免补写到另一个布局或服务。

清单保存坐标、来源指纹、脱敏错误和输出位置，不保存原始来源 URL 或认证密钥。可在 TOML 中更新常见 token/key 等认证参数后补洞；图层、样式等身份参数仍须保持一致。无法匹配来源的清单会被拒绝。

失败清单仅覆盖已尝试但失败的瓦片；Ctrl+C 时尚未调度的范围不会全部加入清单。恢复整个中断任务，请保持原目标并设置 `output.resume = true` 后重新运行下载；`repair` 适合补已知的失败项。

## 4. 恢复能力对照

| 操作 | 中断后如何继续 |
| --- | --- |
| 下载到本地散瓦片、MBTiles、Compact v2、S3 散瓦片 | `output.resume = true`，保持原目标与路径模板，跳过非空瓦片 |
| 在同一数据集追加地区 | 下载用 `output.append = true`；已有数据转换用 `convert -append` |
| 重试下载失败项 | `repair -failures ...`，只处理清单中的实际失败坐标 |
| 归档转换/重建 MBTiles、PMTiles、Compact v2 | 不保存跨进程构建进度，重新执行转换 |
| 上传已有归档 | 有 SDK 重试，不保存跨进程分段状态；重新运行会重传所列文件 |

追加、按瓦片恢复和大文件分段续传是不同的能力。PMTiles 的追加通过合并重建完成，S3 归档追加也需要完整新归档上传。
