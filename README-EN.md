# Tiler — map tile downloader

[中文](README.md)

Download map tiles concurrently within GeoJSON boundaries to files, MBTiles (SQLite), Compact v2, or S3. Sources must use standard Web Mercator XYZ/TMS grids. URL placeholders are `{z}`, `{x}`, `{y}`, and `{-y}` for an inverted row. Multiple `[[lrs]]` sections can assign different boundaries to different zoom levels.

The same executable includes `convert` for conversion and appending between local/S3 tile directories, MBTiles, PMTiles, and Compact v2, `upload` for existing archives or Compact caches on S3/MinIO, and `repair` for replaying failed tile coordinates.

For large raster datasets, `output.format = "compactv2"` writes each tile directly into its standard 128×128 bundle, without loose image files. Resume, append, and repair work per tile. External MapProxy can expose the whole cache as one layer. See the [Compact v2 guide](docs/compact-v2.md) and [MapProxy example](docs/mapproxy-compactv2.yaml). CI verifies the generated cache using real MapProxy XYZ, TMS, and WMTS requests; MapProxy is not a runtime dependency of tiler.

`lrs.geojson` accepts a FeatureCollection, a single Feature, a bare Geometry, or a Nominatim array containing `geojson` geometry fields; no manual FeatureCollection wrapper is needed.

## Build and run

Run from the repository root using Go 1.20.14 and a C compiler for SQLite/CGO. AWS SDK dependencies are pinned to Go 1.20 compatible versions:

```sh
CGO_ENABLED=1 go build -o tiler .
./tiler -c conf.toml
# Or run the complete package:
go run . -c conf.toml
```

Build the whole package, rather than `go run main.go`. Keep the dependencies pinned in `go.mod`; do not separately upgrade upstream orb with `go get`. Extended tile coverage helpers are now local and do not require private fork APIs. Configure `tm.url`, `tm.format`, and each `[[lrs]]` before running. Relative paths use the process working directory.

Windows 7 targets require exactly Go 1.20.14 and an MSVCRT MinGW compiler. Cross-build from Linux:

```sh
GO_CMD=/path/to/go1.20.14/bin/go MINGW_ROOT=/path/to/llvm-mingw-msvcrt bash scripts/build-windows.sh
```

Or build on Windows in PowerShell with a C compiler matching the target architecture:

```powershell
.\scripts\build-windows.ps1 -Arch amd64 -Compiler gcc
# Use -Arch 386 and its matching compiler for 32-bit Windows.
```

The outputs are `dist/tiler_windows_amd64.exe` and/or `dist/tiler_windows_386.exe`, including CGO SQLite support. CI builds both architectures with pinned tools. These are builds and checks targeting Win7; execution on a real Win7 system still needs verification. Put the executable alongside `conf.toml` and `geojson`, open a terminal there, and run `tiler_windows_amd64.exe -c conf.toml`.

## Direct S3 output

Keep your existing `tm`, `lrs`, and `task` configuration and set:

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

Create the bucket beforehand and grant `s3:PutObject` on the target objects. Plaintext `access_key_id` and `secret_access_key` may be set under `[output.s3]`, with `session_token` for temporary credentials. Configured credentials take precedence over environment variables, profiles, and IAM roles; an empty configured token does not inherit the environment token. Leave all three fields empty to use the AWS default chain: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, shared configuration/`AWS_PROFILE`, or an IAM role. A partial key pair or a token without keys is a configuration error. Direct output, `upload`, and S3 `convert` share these settings. An empty `region` uses `AWS_REGION`, `AWS_DEFAULT_REGION`, or the AWS profile.

Keys combine `prefix` with `output.tile_path`, producing `basemap/{z}/{x}/{y}.{format}` by default. For example, `{z}/{y}/{x}.{format}` swaps the directory order. An empty prefix starts directly with the template. S3 writes no local tile copy; `output.directory` provides the default local failure-manifest directory, and `output.name` does not affect object keys. With `resume = false`, existing keys are overwritten; resume uses HEAD to skip nonempty objects and additionally requires `s3:GetObject`. AWS S3 also needs `s3:ListBucket` to distinguish missing objects (404) from access denial (403); see [HeadObject permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

For MinIO or another compatible service, set `endpoint = "http://localhost:9000"`, `force_path_style = true`, and the service's region and credentials. Downloads and uploads share `task.workers`. Uploads use SDK retries; positive `output.s3.timeout_seconds` limits each upload including retries. Failures cause the task to return an error.

MBTiles output processes tiles in memory and writes directly to SQLite, without loose PNG/JPG files. SQLite may temporarily create a `-journal` transaction log; the database is closed after all queued writes finish.

File downloads write `metadata.json` in the tile root with format, zoom, bounds, and `tile_path` for later conversion. It is not proof of download completion. MBTiles retains metadata in SQLite. Direct S3 downloads do not add this file; `convert` writes metadata when exporting an S3 tile directory.

## Tile paths and appending regions

Local files and direct S3 tiles share `output.tile_path`:

```toml
[output]
format = "file"
directory = "output"
name = "basemap"
tile_path = "{z}/{y}/{x}.{format}"
append = true
```

Templates support `{z}`, `{x}`, `{y}`, `{-y}`, and `{format}`. The inverted row is `2^z-1-y`. Include z, x, and either y or -y; fixed subdirectories, filename prefixes, and extensionless paths such as `tiles/{z}/{x}_{y}` are allowed. Paths must be relative and numeric coordinate tokens need a nonnumeric separator: `{x}{y}` is ambiguous and rejected. MBTiles/PMTiles/Compact v2 use their own internal layouts and do not use this template.

`append = true` also enables tile resume. After downloading region A, keep the same source, format, layout, and target, change `[[lrs]]` to select region B, and run again. Existing nonempty tiles remain and new coordinates join the same dataset. Local files, MBTiles, and Compact v2 require a fixed `output.name`; their bounds and zoom ranges are merged. S3 appends objects under the same bucket/prefix. Use a new directory or prefix when changing the template.

Downloads write directly to files, MBTiles, Compact v2, or S3. For a growing PMTiles dataset, maintain an MBTiles working archive and convert it, or use `convert -append` below.

## Offline conversion, archive upload, and repair

```sh
# XYZ directory paths are z/x/y.png (jpg, pbf, and webp are also accepted).
./tiler convert -input output/tiles -output basemap.mbtiles
./tiler convert -input basemap.mbtiles -output basemap.pmtiles
# Actually encode images as JPEG while building MBTiles.
./tiler convert -input output/tiles -output basemap-jpg.mbtiles -format jpg
# Export PMTiles into a different tile layout.
./tiler convert -input basemap.pmtiles -output output/zyx -output-type file -tile-path '{z}/{y}/{x}.{format}'
# Build an archive from existing S3 tiles, or export an archive to S3 tiles.
./tiler convert -c conf.toml -input s3://my-map-tiles/basemap/ -output basemap.mbtiles
./tiler convert -c conf.toml -input basemap.pmtiles -output s3://my-map-tiles/zyx/ -output-type file -tile-path '{z}/{y}/{x}.{format}'
# Append region B while keeping existing coordinates in the target archive.
./tiler convert -input region-b.mbtiles -output basemap.pmtiles -append
# Use output.s3 from conf.toml; put flags before input paths.
./tiler upload -c conf.toml -timeout 2h basemap.mbtiles basemap.pmtiles
# Retry only unresolved manifest entries at their original output destination.
./tiler repair -c conf.toml -failures output/basemap.mbtiles.failures.json
```

`convert` supports both directions between local/S3 tile directories, MBTiles, PMTiles v3, and Compact v2. S3 archive URLs can be `s3://bucket/maps/basemap.pmtiles`; use a trailing `/` for tile prefixes. Purely local conversion does not read TOML by default. S3 conversion uses `-c` for the endpoint, region, and credential configuration, while the URI selects the bucket and key. Directory metadata can identify `tile_path` automatically; `-input-tile-path` overrides the input layout, and `-tile-path` selects the output layout.

MBTiles supports z0–30 and PMTiles z0–26. Existing targets are rejected by default. `-overwrite` permits replacement; `-append` keeps old coordinates and merges new tiles. MBTiles/PMTiles appends stage a complete merged archive before replacement. Appending to an existing Compact cache only merges bundles touched by the new input; S3 downloads those bundles and uploads only changed bundles plus metadata. It reports newly added tiles, not the cache's total. See the [Compact guide](docs/compact-v2.md) for staging space and failure behavior. Local directory replacement is staged in full; Compact appends, loose tile directory appends, and loose S3 outputs may be partially complete after publication is interrupted. Repeat `-append` to preserve completed tiles and fill the remaining gaps. Conversion does not persist build checkpoints across processes. Archives normalize PBF to one gzip layer, while local tile directories export raw PBF; PBF → MBTiles/PMTiles requires valid `vector_layers`, supplied by input metadata or `-metadata metadata.json`. Archive building uses temporary databases/archives and cleans them up without unpacking loose images; individual tiles are created only when a tile-directory output is requested.

`upload` also accepts an explicit Compact v2 root (see its guide). It accepts multiple `.mbtiles`/`.pmtiles` files or directories, recursively finding archives and preserving directory-relative paths. Defaults are a one-hour timeout per file, initial 8 MiB parts, and three concurrent parts per file. An existing key is replaced when its upload completes. A failed multipart upload is aborted where possible; the command reports previously completed uploads and does not resume multipart state across processes. Stop writes and complete the WAL checkpoint before uploading MBTiles; a nonempty `-wal` sidecar or an active rollback journal is rejected.

Downloads with failures write a JSON manifest and log its path. Set `output.failure_file` to choose a `.json` path. A successful first MBTiles download does not create an extra failure manifest. Repair reuses the recorded destination; keep the same map name, format, source, zooms, and output settings. Authentication parameters such as tokens may be refreshed. Manifests store coordinates, source fingerprints, and redacted errors, without original source URLs or keys. Coordinates not yet scheduled when Ctrl+C was pressed are absent; use `output.resume = true` to recover the whole interrupted task.

See [conversion, upload, and repair details](docs/archive-and-repair.md) for options, metadata examples, and manifest paths. On Windows, use `.\tiler.exe` in place of `./tiler`. These commands require no separate Python, Node, `mc`, or PMTiles CLI installation.

## Resume, encoding, and controls

- `output.resume = true` skips saved, nonempty tiles. Files, MBTiles, and Compact v2 use the stable basename `Name-zmin-max`; `output.name` overrides it. Keep the same source and destination configuration when resuming. For old outputs with random IDs, set `output.name` to their full basename (without `.mbtiles`) or rename them to the new stable name.
- `output.append = true` adds regions to the same dataset and merges local file/MBTiles/Compact v2 bounds and zooms; those targets require a fixed `output.name`. It includes resume behavior and keeps existing nonempty tiles at overlapping coordinates. `output.tile_path` is shared by file/S3 writes, resume, and repair; use a new target when changing the layout.
- `tm.format = "png"` or `"jpg"` actually converts the image encoding. PBF defaults to raw bytes for files and gzip for MBTiles/S3. Files/S3 can override `output.gzip_pbf`; MBTiles requires gzip PBF. S3 sets Content-Encoding to match.
- `task.timeout_seconds = 30` and `task.retries = 3` control download request timeouts and retries for network failures and HTTP 408, 429, and 5xx. Standard redirects and HTTP(S) proxy environment variables are supported. Set `task.user_agent` and `task.referer` when needed by the source.
- Run `./tiler -i -c conf.toml` for controls: enter `p`, `r`, or `q`, then Enter, to pause, resume, or cancel. Pausing stops new scheduling while in-flight requests finish. Ctrl+C cancels while preserving saved tiles; restart with resume enabled.
- End-of-task logs report concurrency-safe `saved`, `skipped`, and `failed` counts for successful writes, verified existing nonempty tiles, and tile errors. File resume skips do not wait for `task.timedelay`.

Throughput depends on the provider, network, encoding and disk. `task.workers` controls concurrency, and the HTTP idle connection pool scales with it. `task.timedelay` controls request start spacing: the example's `50` milliseconds allows at most about 20 starts per second. Where the provider permits, use `0` and increase concurrency gradually; reduce concurrency or restore spacing on 429/503 responses.

`task.savepipe` is the MBTiles queue capacity, not a writer count. The writer commits already queued tiles together; with an empty queue it writes one tile immediately, without waiting to fill a batch. Only committed tiles count as `saved`. A failed batch rolls back and retries each tile individually; only tiles that still fail enter the failure manifest, so one bad tile does not prevent its neighbors from being saved. A larger queue can improve batching on a slow disk but uses more memory. Rectangular GeoJSON regions, including the example world extent, use direct counting and streaming enumeration. PBF is fully decompressed, validated and encoded for the destination; gzip output is normalized to one member for browser compatibility. These optimizations preserve tile validation and configured request pacing.

## FAQ and scope

| Question | Answer |
| --- | --- |
| Why only zoom 11 or an incomplete region? | Each `[[lrs]].min/max/geojson` defines the actual zooms and boundaries. `tm.min/max` metadata is derived from the actual layers. [#15](https://github.com/atlasdatatech/tiler/issues/15) |
| SQLite output? | Use `output.format = "mbtiles"`; MBTiles is a SQLite database. [#22](https://github.com/atlasdatatech/tiler/issues/22) |
| Exact contour cropping? | GeoJSON selects intersecting tiles; boundary tiles remain whole images. Pixel-level contour clipping is not implemented. [#17](https://github.com/atlasdatatech/tiler/issues/17) |
| Baidu or EPSG:4326? | Baidu's custom grid and EPSG:4326/Tianditu `*_c` grids are unsupported. Use Tianditu `*_w` services such as `img_w` or `vec_w`. [#3](https://github.com/atlasdatatech/tiler/issues/3) |
| Historical years or mosaics? | No built-in year selector or image stitching. Historical imagery requires a suitable source URL. [#19](https://github.com/atlasdatatech/tiler/issues/19), [#16](https://github.com/atlasdatatech/tiler/issues/16) |
| A URL works in a browser but downloads fail? | Check the exact tile URL, credentials, proxy, and Referer. For 429/503, reduce `workers` or increase `timedelay`. Retries cannot guarantee recovery from expired endpoints or server restrictions. [#18](https://github.com/atlasdatatech/tiler/issues/18), [#14](https://github.com/atlasdatatech/tiler/issues/14) |

Example templates: Google `https://mt0.google.com/vt/lyrs=s&x={x}&y={y}&z={z}`; Tianditu `https://t0.tianditu.gov.cn/DataServer?T=img_w&x={x}&y={y}&l={z}&tk=YOUR_KEY`; ArcGIS typically uses `/MapServer/tile/{z}/{y}/{x}`. Use authorized services and your own keys, and check the grid and response format.

See the [issue status table](docs/issue-status.md) for the scope of all reviewed issues.

See [fork review and adopted concepts](docs/fork-review.md) for the inventory, compatibility findings, and features independently rewritten for this branch.
