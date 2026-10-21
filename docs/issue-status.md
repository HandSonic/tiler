# Issues 处理范围

记录截至 2026-10-21 对上游 #1–#26 的审阅，以及当前本地分支的改动或说明。这里的“本地修复”不代表已向 GitHub 提交、合并或关闭 Issue；外部数据源与历史发行包问题也不能由本地测试完整证明已解决。

| Issue | 分类 | 本地处理与剩余范围 |
| --- | --- | --- |
| [#1 下载产物为空](https://github.com/atlasdatatech/tiler/issues/1) | 本地修复 / 说明 | MBTiles 等待保存队列排空再关闭数据库，下载和写入错误向任务返回；MBTiles 内的 `tiles` 表保存数据。原报告的下载源是否成功仍需看错误日志。 |
| [#2 如何获取 XYZ](https://github.com/atlasdatatech/tiler/issues/2) | 文档说明 | README 给出地址模板与运行步骤；程序按 GeoJSON 和层级计算 XYZ，用户只需配置占位符。 |
| [#3 百度切片](https://github.com/atlasdatatech/tiler/issues/3) | 未实现 | 百度特殊坐标/瓦片计算未实现，替换 URL 不能保证正确。EPSG:4326 网格也不在当前范围。 |
| [#4 PBF 文件大小不同](https://github.com/atlasdatatech/tiler/issues/4) | 本地修复 / 说明 | 可配置 PBF gzip；本地文件默认原始 PBF，MBTiles/S3 默认 gzip。文件大小会随压缩不同，客户端/HTTP 服务需正确处理编码。 |
| [#5 历史发行包 execute.bat](https://github.com/atlasdatatech/tiler/issues/5) | 历史产物待核实 | 新构建脚本仅生成本仓库的 EXE，不生成 `execute.bat`。不能由此倒推历史 v0.1.3 ZIP 的内容或风险，也没有修改历史发行包。 |
| [#6 单文件运行 / SQLite 编译](https://github.com/atlasdatatech/tiler/issues/6) | 文档说明 / 格式支持 | 说明在根目录编译/运行整个包，并准备 CGO C 编译器；PBF 不压缩可通过配置选择，不必修改源码。历史 `LoadExtension` 报错不等同于当前编译结果。 |
| [#7 无层级时空指针](https://github.com/atlasdatatech/tiler/issues/7) | 本地修复 | 缺少有效 `lrs` 时返回清晰配置错误，避免对空任务调用 Download。 |
| [#8 QGIS 打开 MBTiles](https://github.com/atlasdatatech/tiler/issues/8) | 本地修复 / 说明 | 排空 SQLite 保存队列并关闭数据库，修正范围等元数据；矢量瓦片仍需匹配的 TileJSON/渲染配置，不能保证任意查看器直接显示。 |
| [#9 天地图用法](https://github.com/atlasdatatech/tiler/issues/9) | 文档说明 | 提供 `*_w` 地址模板、自有密钥占位符、配置与运行步骤；请求频率可通过并发和间隔控制。 |
| [#10 谷歌无法下载](https://github.com/atlasdatatech/tiler/issues/10) | 本地改进 / 外部条件 | 增加超时、重试、标准重定向和代理支持；网络连通性及服务可用性仍需用户环境验证。 |
| [#11 跳过已有瓦片](https://github.com/atlasdatatech/tiler/issues/11) | 本地功能 | `output.resume = true` 使用稳定路径并跳过非空瓦片；支持文件、MBTiles 和 S3。恢复需维持同一数据源和目标配置。 |
| [#12 IP / key 封禁](https://github.com/atlasdatatech/tiler/issues/12) | 外部条件 | 可限并发、设请求间隔并重试暂时错误；服务端配额、封禁或授权规则不在工具保证范围，不能承诺避免封禁。 |
| [#13 天地图范围错误](https://github.com/atlasdatatech/tiler/issues/13) | 本地校验 / 未支持网格 | 检测已知 `*_c` / EPSG:4326 配置并提示改用 `*_w`；修正 bounds 元数据计算。没有实现经纬度瓦片网格重投影。 |
| [#14 ArcGIS 503](https://github.com/atlasdatatech/tiler/issues/14) | 本地改进 / 外部条件 | 支持 5xx 等暂时错误重试、超时、重定向；文档建议降低并发和增大间隔。503 的服务端原因无法由工具保证消除。 |
| [#15 高德用法 / 下载层级](https://github.com/atlasdatatech/tiler/issues/15) | 文档说明 / 元数据修正 | 可替换兼容的瓦片 URL；实际级别与范围由各 `lrs` 指定，元数据自动取实际层级。具体图层地址与授权仍由数据源提供。 |
| [#16 瓦片镶嵌](https://github.com/atlasdatatech/tiler/issues/16) | 未实现 | 没有内置拼图、镶嵌或带地理配准的大图导出。Issue 中的第三方示例不属于本项目实现。 |
| [#17 轮廓裁切](https://github.com/atlasdatatech/tiler/issues/17) | 未实现 / 文档纠正 | GeoJSON 仅选择相交瓦片，边缘瓦片保持完整；撤销原 README 对像素级轮廓裁切的承诺。 |
| [#18 浏览器可开但程序失败](https://github.com/atlasdatatech/tiler/issues/18) | 本地改进 / 外部条件 | 标准重定向、代理、可配 User-Agent/Referer、超时重试改善兼容性；原地址、凭证和用户网络仍需实测。 |
| [#19 历史年份](https://github.com/atlasdatatech/tiler/issues/19) | 未实现 | 没有统一年份接口；仅当数据源提供历史影像地址时，可作为自定义 URL 下载。 |
| [#20 PBF 加载报错](https://github.com/atlasdatatech/tiler/issues/20) | 本地修复 / 说明 | 原始/压缩 PBF 可配置，并处理已有 gzip 数据；S3 设置匹配的 Content-Encoding。本地自行托管 gzip 文件时仍需正确响应头。 |
| [#21 自定义范围不全](https://github.com/atlasdatatech/tiler/issues/21) | 既有配置问题 / 说明 | 原提问者已确认需修改对应层的 GeoJSON，上游原本已关闭；文档强调 `lrs.geojson` 决定每层实际范围。 |
| [#22 SQLite 支持](https://github.com/atlasdatatech/tiler/issues/22) | 已有功能 / 收尾修复 | 明确 MBTiles 即 SQLite，使用 `output.format = "mbtiles"`；等待保存完成并关闭数据库，避免队列尚未写完即退出。 |
| [#23 使用教程](https://github.com/atlasdatatech/tiler/issues/23) | 文档说明 | 提供编译、配置、Windows 命令行运行及目录放置说明；无需先掌握 Go 即可运行已构建的程序。 |
| [#24 tilecover 符号缺失](https://github.com/atlasdatatech/tiler/issues/24) | 本地修复 | 覆盖计数/遍历扩展函数内置，移除 fork 私有 API 依赖；按 `go.mod` 编译整个包。 |
| [#25 JPG 扩展名但内容 PNG](https://github.com/atlasdatatech/tiler/issues/25) | 本地修复 | PNG/JPG 输出执行真实图像转码，使内容与目标扩展名一致。 |
| [#26 恢复 / 暂停 / 稳定目录](https://github.com/atlasdatatech/tiler/issues/26) | 本地功能 | 增加 resume 跳过、稳定输出名、可配置 `output.name`、交互暂停/继续/取消及 Ctrl+C。暂停只停止新调度，已发请求可完成；取消后用 resume 重启。 |

验证范围见提交/交付说明。面向 Windows 7 的固定 Go 1.20.14、MSVCRT 构建及 CI 不等于已经在真实 Win7 系统上运行验证；真实 Win7 和实际云端 S3 仍需各自环境测试。
