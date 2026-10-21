# Fork 评估与采纳

2026-10-21 对公开 fork 做分支和提交盘点，并对有独立改动的分支进行静态源码审查。没有运行 fork 的程序、预编译文件、构建脚本或前端，也没有整体合并任何 fork。

## 盘点范围

- 发现 86 个 public fork；85 个可通过公开 Git 访问，共 102 个分支头。`1369521908/tiler` 的公开 Git 访问失败，未取得其源码，不能给出实现结论。
- 排除已存在于上游 master、dev 或 tag 历史中的提交后，10 个 fork 包含独立改动，共 79 个按 SHA 去重的独立提交。各 fork 的提交数包含共享历史，不能直接相加。
- 下表记录审查时的分支头；差异以 `atlasdatatech/tiler` 的 [`8f4fb63`](https://github.com/atlasdatatech/tiler/commit/8f4fb63f8b60e5a7af7d2ca12cd97dc4ba5ad6d5) 为参照。RoninZc 从更早版本分叉，另有 3 个上游提交未包含。

完整分支头清单见 [fork-inventory.json](fork-inventory.json)。

## 有独立改动的 10 个 fork

| Fork / 分支头 | 提交数 | 主要改动 | 评估与限制 |
| --- | ---: | --- | --- |
| [Mike-Acb/tiler](https://github.com/Mike-Acb/tiler) · master [`4f7f9abb`](https://github.com/Mike-Acb/tiler/commit/4f7f9abbdb2c9770b13dee02f712f50d0c817c19) | 25 | 代理池、重试恢复、文件元数据、PMTiles/MinIO 同步、NLSC 模型下载与转换 | 借鉴文件元数据和提前跳过已有瓦片。PMTiles 独立构建镜像使用 Go 1.25；NLSC 含无 Windows 适配的 `syscall.Flock`。其容器、shell 工具和模型处理栈不纳入 Win7 CLI。 |
| [moshuiyan/tiler](https://github.com/moshuiyan/tiler) · mo-dev [`7043778`](https://github.com/moshuiyan/tiler/commit/704377878c4cc200250e02ace0ba9297048b47d5) | 17 | 稳定文件目录、存在性跳过、仅统计、JSON 瓦片清单、下载次数上限、专用 URL 签名 | 功能需求有参考价值；恢复只检查存在性，MBTiles 仍重建；计数存在竞争，上限在请求完成后检查，不能严格限制派发。部分退出、清单模式和分层 URL 行为有回退。 |
| [yeleibo/tiler](https://github.com/yeleibo/tiler) · master [`8268b828`](https://github.com/yeleibo/tiler/commit/8268b82806ea7ddb1f69c6a970ec04649c654eee) | 20 | 多种 GeoJSON 输入、日志、恢复/进度库、Qt 尝试、后续移除 Qt、容器与瓦片目录转换 | 借鉴输入格式兼容。存在直接交换 X/Y 的数据源处理、成功标记早于实际保存等风险；Qt 历史方案和 modernc SQLite 未证明适配 Win7，不引入。 |
| [Saturn87930/tiler](https://github.com/Saturn87930/tiler) · master [`c18041e7`](https://github.com/Saturn87930/tiler/commit/c18041e7a88ff3c558e6dc3142e78a7ff4c5959d) | 10 | 合入 yeleibo 的恢复、日志、多输入格式等历史 | HEAD 文件树与 yeleibo 的 [`face2130`](https://github.com/yeleibo/tiler/commit/face21303355031db9100ea499e8913912319296) 完全相同；没有额外独立实现，不重复采纳。 |
| [RoninZc/tiler](https://github.com/RoninZc/tiler) · master [`7fccde75`](https://github.com/RoninZc/tiler/commit/7fccde7542764e56ada303e5f2ed04fe5b064aa3) | 5 | 文件专用重构、检查点、日志级别/日期日志、信号退出 | 移除了 MBTiles；保存失败仍可能写成功检查点。退出不可靠等待 worker/检查点落盘，目录被 `filepath.Base` 截断；不能作为当前多输出恢复实现。 |
| [leafson/tiler](https://github.com/leafson/tiler) · master [`e20fa7c4`](https://github.com/leafson/tiler/commit/e20fa7c45ae98190ee7696552af31ec6f70cdb73) | 3 | 恢复、代理、重试、统计与说明文档 | 借鉴成功/跳过/失败分开统计。原实现部分计数直接 `++`，有并发竞争；只按存在性跳过，零字节产物可能被当作完成。 |
| [zhuiyuzhuiyu/tiler](https://github.com/zhuiyuzhuiyu/tiler) · master [`97ed9e95`](https://github.com/zhuiyuzhuiyu/tiler/commit/97ed9e95d7f64765ed557b3ccda388c2a2f6cb13) | 3 | React/Cesium 控制台、任务存储、预览与瓦片浏览器 | 借鉴任务统计字段。大型无鉴权 HTTP 控制台扩大运行范围，且下载失败仍可能进入 completed 状态；不引入服务器和 Node 构建链。 |
| [qilimazhualuo/tiler](https://github.com/qilimazhualuo/tiler) · master [`ab79a628`](https://github.com/qilimazhualuo/tiler/commit/ab79a628af9a429ba4faa327ede141e730bc012f) | 2 | 文件直接存输出根目录，下载前检查文件/MBTiles 瓦片 | 可借鉴稳定目标和跳过思路；`os.Stat` 未检查非空与文件类型，数据库查询错误被忽略。MBTiles 随机命名并先删除，无法实现跨运行恢复。 |
| [kdsrsl/tiler_down](https://github.com/kdsrsl/tiler_down) · master [`b874358`](https://github.com/kdsrsl/tiler_down/commit/b87435812dd21f7b6e997b73ee5bd3df739bd75e) | 2 | Gin 本地瓦片 API、配置/范围示例 | 下载器核心未改。新增 `api4.go` 与原文件同包各有 `main`，破坏整包构建；服务固定路径/格式，读取失败仍可能返回 200。不整体引入。 |
| [wei2019-destiny/tiler](https://github.com/wei2019-destiny/tiler) · master [`28e0e653`](https://github.com/wei2019-destiny/tiler/commit/28e0e653d9515bb8b9851d8d1c60c2b680d43fbe) | 1 | 天地图 token 与最小级别配置调整 | 没有可复用的代码功能；不复制第三方密钥或个人配置。 |

## 本分支采纳的概念

以下功能参考了具体 fork 的需求和设计，按现有三种存储模式及 Go 1.20/Win7 约束独立重写，没有复制整个 fork：

| 来源提交 | 本分支行为 |
| --- | --- |
| Mike-Acb [`753072b4`](https://github.com/Mike-Acb/tiler/commit/753072b4dad42ba47a74def158674d0468cb7e48) | 文件输出根目录生成 `metadata.json`，记录格式、范围、层级等元数据。仅 `file` 模式生成；MBTiles 使用数据库元数据，S3 不增加该对象。 |
| Mike-Acb [`27c8e935`](https://github.com/Mike-Acb/tiler/commit/27c8e93524c5e2b8d9aa42cdaad5b434a9825d3c) | 文件恢复时提前检查已有非空瓦片，跳过项不消耗请求间隔；不把零字节占位文件视为成功。 |
| yeleibo [`9a27df1`](https://github.com/yeleibo/tiler/commit/9a27df1a699eb72d3bbcc76e9cc767ba1f1de732) | 统一读取 FeatureCollection、单个 Feature、裸 Geometry，以及含 `geojson` 几何字段的 Nominatim 数组；保持范围选择与元数据一致。 |
| leafson [`20bed843`](https://github.com/leafson/tiler/commit/20bed84302b7160fbfa477b10e7409de497e2baa)；zhuiyuzhuiyu [`db472605`](https://github.com/zhuiyuzhuiyu/tiler/commit/db47260530a129e85c840f90ad9d8efa47cef865) | 并发安全地统计 `saved`、`skipped`、`failed`；实际保存成功后才计入 saved，SQLite 入队不算保存成功。 |

## 未采纳与兼容性边界

- **代理/网络栈**：保留标准 HTTP(S) 代理环境变量、可配请求头和有界重试；不移植代理池、Linux shell 守护或容器调度。公开服务的授权、限流与可用性仍需实际环境验证。
- **恢复与预算**：不采用“路径存在就成功”、404 空文件标记或未落盘的成功检查点。JSON 瓦片补洞清单、仅统计和严格下载预算可作为后续需求，当前不因 fork 中出现就声明已有。
- **格式与坐标**：不照搬 X/Y 对调、全局 URL 替代各层 URL、固定 Referer 或始终 gzip PBF。专用签名代码也不移植：moshuiyan 的 [`7043778`](https://github.com/moshuiyan/tiler/commit/704377878c4cc200250e02ace0ba9297048b47d5) 在 URL 不匹配时可能索引空匹配，并把签名密钥写入日志。
- **界面与额外工具**：暂不移入 React/Cesium、Qt、Gin 服务器、NLSC/3D Tiles 和 PMTiles 同步流水线。相关功能需单独确定产品范围、依赖版本和 Windows 支持。
- **验证范围**：本次对 fork 的结论来自提交、差异和源码静态检查，没有验证其运行结果。任何 fork 的预编译 EXE、Linux 构建成功或 `go.mod` 版本声明，都不能证明真实 Win7 兼容。本分支仍固定 Go 1.20.14；实际云端 S3 与真实 Win7 运行验证独立进行。
