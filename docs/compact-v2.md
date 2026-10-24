# Compact v2 下载与 MapProxy 读取

`compactv2` 是 Esri Compact Cache v2 栅格缓存目录。每个 `.bundle` 保存同一级别内最多 128×128 张瓦片，索引内置，无独立 `.bundlx`。tiler 在内存处理图像后直接写 bundle，不生成散落的 PNG/JPEG 临时文件，也不要求先下载成一个全球大库。

支持标准 Web Mercator XYZ 网格、z0–30、PNG/JPEG、统一的 256×256 或 512×512 像素瓦片。首张图像确定尺寸，之后不得混用。PBF、TPK/TPKX 打包、任意坐标系与自定义 bundle 边长不在此功能范围；WebP 输入可以用 `convert -format png` 或 `-format jpg` 实际转码后导出。PMTiles 互转仍受其 z0–26 范围约束。

读取外部缓存时允许 Esri 元数据的十进制舍入，但各层级的原点误差与全球范围累计分辨率误差合计不得超过半个像素；不通过检查的网格不会被直接当成标准 XYZ。

## 直接下载

保留 `task`、`tm` 和 `[[lrs]]`，修改输出：

```toml
[output]
format = "compactv2"
directory = "output"
name = "world-compact"
resume = true
append = false
```

`tm.format` 使用 `png` 或 `jpg`。输出为 `output/world-compact/`，包含 `conf.xml`、`conf.cdi`、`metadata.json` 及 `_alllayers/Lxx/RxxxxCxxxx.bundle`。标准固定布局不使用 `output.tile_path`；R/C 分别是 bundle 起始行/列的十六进制数，内部是左上角原点 XYZ 行号。

- `resume = true` 检查具体瓦片的有效非空索引，跳过已存坐标；bundle 存在不代表整包已完成。
- `append = true` 同时启用 resume，要求固定的 `output.name`。只更改 `[[lrs]]` 地区或层级，即可将 B 追加进 A 的原目录，保留已有重叠瓦片，合并范围和层级。
- `repair` 沿用原目标和失败清单，默认清单为缓存根目录内的 `failures.json`。完整恢复中断任务仍应重跑原范围并开启 resume。
- `resume = false` 会覆盖本次请求的坐标，其他坐标保留；重写的旧字节可能留在 bundle 中。需要回收空间时转换到新目录。新建数据集应使用新名称。

单张编码图像受规范的 24 位长度字段限制，最大 16,777,215 字节。每包 16,384 张是瓦片数量上限，非固定字节容量；反复覆盖也会增加文件大小。只允许一个进程修改同一缓存。正常取消会结束已开始的瓦片写入，突然断电或磁盘错误不具备数据库式事务保证。

## 已有数据互转、追加和 S3

```sh
# 本地散瓦片、MBTiles、PMTiles 均可作为输入
./tiler convert -input world.mbtiles -output output/world-compact -output-type compactv2
# 已有 Compact 根目录由 conf.xml 自动识别
./tiler convert -input output/world-compact -output world.pmtiles
./tiler convert -input output/world-compact -output output/xyz -output-type file
# 合并新地区，重叠坐标保留已有瓦片
./tiler convert -input region-b.mbtiles -output output/world-compact -append
# S3 前缀以 / 结尾；endpoint、region、凭证来自 conf.toml
./tiler convert -c conf.toml -input world.mbtiles -output s3://my-map-tiles/world-compact/ -output-type compactv2
./tiler convert -c conf.toml -input s3://my-map-tiles/world-compact/ -output restored.mbtiles
./tiler convert -c conf.toml -input region-b.pmtiles -output s3://my-map-tiles/world-compact/ -output-type compactv2 -append
# 上传已完成的本地缓存，output.s3.prefix 应指向它的专用前缀
./tiler upload -c conf.toml output/world-compact
```

新建 Compact 输出目录必须显式指定 `-output-type compactv2`；已有目录可自动识别。向已有 Compact 目标执行 `convert -append` 时，先完整验证并暂存新输入，再合并它涉及的 bundle；旧的非空瓦片保留，只添加缺失坐标。本地没有新增瓦片的 bundle 不会重写。S3 只下载新输入涉及的旧 bundle，并上传实际改变的 bundle 和合并后的元数据，不再下载、重建和上传整个旧缓存。完成提示中的数量是**本次新增瓦片数**，不是全库总数。

每个涉及的旧 bundle 仍需整包复制或下载，改变的 S3 bundle 也需整包上传。目标中完全不存在的包会复用已验证的暂存 bundle，省去合并阶段再次逐瓦片写包。需为新输入的转换暂存、临时数据库及涉及的 bundle 副本预留磁盘空间，但不会解包出散图片。格式与瓦片尺寸必须与旧缓存一致，范围和层级只扩展。

恢复或追加的目标须采用 tiler 生成的标准 bundle 路径布局。写入前会流式枚举本地目录项或分页列举 S3 对象键，发现非规范层名、十六进制大小写等 bundle 路径时拒绝写入，避免把已有包当成不存在而生成重复坐标。外部缓存仍可作为转换输入；应先 `convert` 到新的 Compact 目录归一化，再向新目录追加。

bundle 扩展名须为小写 `.bundle`，文件必须放在 `_alllayers` 的层目录内。检查会枚举整个缓存根目录；发现 `.BUNDLE` 等扩展名变体、错放的 bundle 或 bundle 同名目录时，读取、上传和写入预检会报错，避免静默漏数据。根部 `conf.xml`、`conf.cdi`、`metadata.json` 也须使用上述小写名称，大小写变体不会作为普通附带文件忽略。应先修正路径再操作；普通非 bundle 文件和目录仍允许，符号链接不允许。

这项预检的时间随旧缓存目录项或对象键数量增长，S3 会增加分页 LIST 请求；不会下载或解码无关 bundle，也不在内存建立全库索引。非 bundle 的失败报告、说明文件和空目录标记不会建立瓦片尺寸；没有实际 bundle 时，首次有效瓦片仍可决定使用 256 或 512 像素。零长度或损坏的 bundle 不视为无瓦片缓存，仍保留原尺寸限制。读取会检查索引、长度和图像头；恢复或增量追加不完整解码旧瓦片，不能替代内容完整性检查，新输入仍须完整验证。

新输入和暂存校验全部成功后，先发布扩展后的元数据，再逐 bundle 替换。验证或暂存失败不修改目标；发布过程中失败或取消，可能已完成部分新增，重新运行同一条 `-append` 命令会保留已完成的非空瓦片并补入其余瓦片。转换不保存跨进程构建或 S3 分段状态，重跑仍会重新验证输入。单个 bundle 完成替换不代表整套缓存具有原子事务或断电保护；只允许一个进程修改同一缓存，服务更新由外部部署流程协调。本地追加保留原默认 `failures.json`，报告校验失败会报错且不发布结果；其他未涉及文件也保持原地不动。

S3 新建缓存先在本地生成完整结果，再依次发布 `conf.xml`、`metadata.json`、`conf.cdi`，然后上传 bundle。标记或其余元数据上传失败后，也可用同一输入加 `-append` 重试；bundle 阶段中断后，已经完成的包不会重传。如果只成功写入 `conf.xml`，恢复会采用缺失区域元数据的保守默认值，结果可能声明更宽的范围，但不会遗漏已有瓦片。`conf.xml` 表示缓存已初始化，不是全部瓦片上传完毕的凭证，中断时可能仍有空洞。已有非空目标前缀只允许 `-append`；`-overwrite` 被拒绝，避免旧 bundle 残留并与新元数据混用。旧版本若已留下缺少 `conf.xml` 的非空前缀，仍需换空前缀或自行清理；程序不会自动删除远端对象。

同桶 S3 转换的输入、输出不得相互包含，例如 `cache/` 不能转到 `cache/copy/`；应使用独立前缀，例如 `cache-copy/`，以免新 bundle 混入原缓存。

`upload` 识别显式传入的 Compact 根目录，上传其中全部标准元数据和 bundle，不按远端差异跳过已有包，也不上传失败清单或任意 JSON/XML。上传要求根部元数据使用小写名称 `conf.xml`、`conf.cdi`、`metadata.json`，大小写变体会报错，避免在 Windows 本地可读却漏传到 S3。对象键为 `output.s3.prefix/conf.xml`、`output.s3.prefix/_alllayers/...`，不会再加目录基名。请用专用新前缀，或只更新来自同一缓存的对象；该命令不会删除远端旧 bundle。父目录的递归归档上传仍只查找 MBTiles/PMTiles，不自动发现嵌套 Compact 缓存。逐瓦片 S3 下载使用独立前缀；开启恢复或追加时会拒绝已有 Compact 标记的前缀。未开启恢复的纯 PUT 下载不增加读取权限要求，须由调用者保持目标前缀用途一致。

## MapProxy 配置和访问

使用随包提供的 [mapproxy-compactv2.yaml](mapproxy-compactv2.yaml)，修改三项：

| 配置 | 设置 |
| --- | --- |
| `caches.world_cache.cache.directory` | 缓存的 **`_alllayers`** 绝对路径，如 `/data/world-compact/_alllayers` |
| `caches.world_cache.format` | PNG 用 `image/png`，JPEG 用 `image/jpeg` |
| `grids.webmercator.tile_size` | 与缓存一致的 `[256, 256]` 或 `[512, 512]` |

保持 `cache.type: compact`、`version: 2`、网格 `GLOBAL_WEBMERCATOR`、`origin: nw` 和 `num_levels: 31`（开放 z0–30，避免默认只到 z19）。示例不指定固定分辨率列表，MapProxy 会按瓦片尺寸计算；512 像素分辨率为 256 的一半，仍对应原 XYZ 层级。`sources: []` 表示只读取已有数据，缺失瓦片返回空白，不重新下载。

MapProxy 单独安装、运行。开发验证可以执行 `mapproxy-util serve-develop docs/mapproxy-compactv2.yaml`，正式部署使用 MapProxy 自身的部署方式。服务启动后，一个图层即可访问所有 bundle：

```text
XYZ： /tiles/world/webmercator/{z}/{x}/{y}.png
WMTS：/service?SERVICE=WMTS&REQUEST=GetCapabilities
```

JPEG 的 XYZ URL 后缀必须为 `.jpeg`。上面的 XYZ 使用左上角行号；严格 TMS 的行号翻转，而且其 global-mercator 服务层级为 XYZ 层级减 1。例如 XYZ `9/127/130` 对应 `/tms/1.0.0/world/webmercator/8/127/381.png`。优先按示例使用 XYZ 或 WMTS，勿直接混用层级和行号。

此配置读取本地目录。S3/MinIO 存档需先同步至服务机器或可靠挂载；没有实现 MapProxy 直接随机读取 S3 bundle。缓存目录复制时保留全部元数据和 `_alllayers`，更新前先让服务释放相关文件。

## 兼容验证

`scripts/check-mapproxy.py` 使用真实 **MapProxy 6.0.1** 读取由 tiler 生成的缓存，并通过真实 WSGI 的 XYZ、严格 TMS、WMTS 请求比对原始字节。小样本覆盖 PNG/JPEG、256/512 像素、行列不对称坐标、跨 128 边界、z30 及缺失瓦片；检查缺失请求不会修改 bundle。CI 与 Go 回归、Win7 目标构建一起运行此检查。

MapProxy/Python 只用于外部服务及开发验证，不是 tiler 的运行依赖。验证不等于全球规模性能测试或真实 Win7 机器运行验收。

- [Esri Compact Cache v2 规范](https://github.com/Esri/raster-tiles-compactcache/blob/master/CompactCacheV2.md)
- [MapProxy Compact 缓存配置](https://mapproxy.github.io/mapproxy/latest/caches.html#compact)
