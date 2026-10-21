# Tiler — map tile downloader

[中文](README.md)

Download map tiles concurrently within GeoJSON boundaries to files, MBTiles (SQLite), or S3. Sources must use standard Web Mercator XYZ/TMS grids. URL placeholders are `{z}`, `{x}`, `{y}`, and `{-y}` for an inverted row. Multiple `[[lrs]]` sections can assign different boundaries to different zoom levels.

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

[output.s3]
bucket = "my-map-tiles"
prefix = "basemap"
region = "ap-northeast-1"
endpoint = ""
force_path_style = false
timeout_seconds = 60
```

Create the bucket beforehand and grant `s3:PutObject` on the target objects. Credentials use the AWS default chain: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, shared configuration/`AWS_PROFILE`, or an IAM role. Keep credentials out of TOML. An empty `region` uses `AWS_REGION`, `AWS_DEFAULT_REGION`, or the AWS profile.

Keys are `basemap/{z}/{x}/{y}.{format}`, or `{z}/{x}/{y}.{format}` with an empty prefix. S3 writes no local copy and ignores `output.directory` and `output.name`. With `resume = false`, existing keys are overwritten; resume uses HEAD to skip nonempty objects and additionally requires `s3:GetObject`. AWS S3 also needs `s3:ListBucket` to distinguish missing objects (404) from access denial (403); see [HeadObject permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html).

For MinIO or another compatible service, set `endpoint = "http://localhost:9000"`, `force_path_style = true`, and the service's region and credentials. Downloads and uploads share `task.workers`. Uploads use SDK retries; positive `output.s3.timeout_seconds` limits each upload including retries. Failures cause the task to return an error.

MBTiles output processes tiles in memory and writes directly to SQLite, without loose PNG/JPG files. SQLite may temporarily create a `-journal` transaction log; the database is closed after all queued writes finish.

File output alone writes `metadata.json` in the tile root with format, zoom, and bounds information. It is not proof of download completion. MBTiles retains metadata in SQLite, and S3 does not add this file.

## Resume, encoding, and controls

- `output.resume = true` skips saved, nonempty tiles. Files and MBTiles use the stable basename `Name-zmin-max`; `output.name` overrides it. Keep the same source and destination configuration when resuming. For old outputs with random IDs, set `output.name` to their full basename (without `.mbtiles`) or rename them to the new stable name.
- `tm.format = "png"` or `"jpg"` actually converts the image encoding. Unless `output.gzip_pbf` overrides it, PBF uses raw bytes for files and gzip for MBTiles/S3. S3 sets Content-Encoding to match.
- `task.timeout_seconds = 30` and `task.retries = 3` control download request timeouts and retries for network failures and HTTP 408, 429, and 5xx. Standard redirects and HTTP(S) proxy environment variables are supported. Set `task.user_agent` and `task.referer` when needed by the source.
- Run `./tiler -i -c conf.toml` for controls: enter `p`, `r`, or `q`, then Enter, to pause, resume, or cancel. Pausing stops new scheduling while in-flight requests finish. Ctrl+C cancels while preserving saved tiles; restart with resume enabled.
- End-of-task logs report concurrency-safe `saved`, `skipped`, and `failed` counts for successful writes, verified existing nonempty tiles, and tile errors. File resume skips do not wait for `task.timedelay`.

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
