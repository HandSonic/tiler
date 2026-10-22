package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/teris-io/shortid"
	pb "gopkg.in/cheggaaa/pb.v1"
	"tiler/internal/cover"
)

// Catch obvious misconfiguration before allocating pools and queues. This
// count limit is not a process memory budget; large tiles need low concurrency.
const maxTaskSlots = 1024

// Task 下载任务
type Task struct {
	ID           string
	Name         string
	Description  string
	File         string
	Min          int
	Max          int
	Layers       []Layer
	TileMap      TileMap
	Total        int64
	Bar          *pb.ProgressBar
	db           *sql.DB
	workerCount  int
	savePipeSize int
	timeDelay    int
	tileWG       sync.WaitGroup
	saveWG       sync.WaitGroup
	workers      chan maptile.Tile
	savingpipe   chan Tile
	outformat    string
	tilePath     string
	s3           *s3Output
	compact      *compactCache
	errMu        sync.Mutex
	firstErr     error
	initErr      error
	ctx          context.Context
	cancel       context.CancelFunc
	stateMu      sync.Mutex
	paused       bool
	resumeSignal chan struct{}
	resume       bool
	gzipPBF      bool
	httpClient   *http.Client
	retries      int
	userAgent    string
	referer      string
	saved        atomic.Int64
	skipped      atomic.Int64
	failed       atomic.Int64
	started      atomic.Bool
	manifest     *FailureManifest
	failurePath  string
	repairTiles  map[int][]maptile.Tile
	repairMode   bool
	layerSources []string
	sourceChecks map[int]map[maptile.Tile]int
	sourceLast   map[int]int
	sourceTiles  int
}

// NewTask 创建下载任务
func NewTask(layers []Layer, m TileMap) *Task {
	return NewTaskContext(context.Background(), layers, m)
}

// NewTaskContext also cancels tile counting when the caller stops the task.
func NewTaskContext(parent context.Context, layers []Layer, m TileMap) *Task {
	return newDownloadTaskContext(parent, layers, m, nil)
}

func newDownloadTaskContext(parent context.Context, layers []Layer, m TileMap, manifest *FailureManifest) *Task {
	if len(layers) == 0 {
		return nil
	}
	layers = append([]Layer(nil), layers...)
	id, _ := shortid.Generate()

	task := Task{
		ID:          id,
		Name:        m.Name,
		Description: m.Description,
		Layers:      layers,
		Min:         m.Min,
		Max:         m.Max,
		TileMap:     m,
	}

	task.ctx, task.cancel = context.WithCancel(parent)
	if err := task.ctx.Err(); err != nil {
		task.initErr = err
		return &task
	}
	task.manifest = manifest
	task.repairMode = manifest != nil
	if task.manifest == nil {
		task.manifest = newFailureManifest(m.Name, m.Format)
	}
	for i := range layers {
		if layers[i].URL == "" {
			layers[i].URL = m.URL
		}
	}
	if task.repairMode {
		records, err := manifest.RecordsForLayers(layers)
		if err != nil {
			task.initErr = err
			return &task
		}
		task.repairTiles = make(map[int][]maptile.Tile)
		selectedSources := make(map[maptile.Tile]string)
		for _, record := range records {
			fingerprint, err := FailureSourceFingerprint(record.URL)
			if err != nil {
				task.initErr = err
				return &task
			}
			if previous, exists := selectedSources[record.Tile]; exists && previous != fingerprint {
				task.initErr = fmt.Errorf("repair manifest contains conflicting sources for the same tile; use separate outputs")
				return &task
			}
			selectedSources[record.Tile] = fingerprint
			for i, layer := range layers {
				if layer.Zoom == int(record.Tile.Z) && layer.URL == record.URL {
					task.repairTiles[i] = append(task.repairTiles[i], record.Tile)
					break
				}
			}
		}
	}
	task.Min, task.Max = layers[0].Zoom, layers[0].Zoom
	for i := 0; i < len(layers); i++ {
		if layers[i].Zoom < 0 || layers[i].Zoom > 30 {
			task.initErr = fmt.Errorf("layer zoom must be between 0 and 30")
			return &task
		}
		if err := validateTileURL(layers[i].URL); err != nil {
			task.initErr = fmt.Errorf("zoom %d: %w", layers[i].Zoom, err)
			return &task
		}
		if task.repairMode {
			layers[i].Count = int64(len(task.repairTiles[i]))
		} else {
			count, err := cover.CountContext(task.ctx, layers[i].Collection, maptile.Zoom(layers[i].Zoom))
			if err != nil {
				task.initErr = err
				return &task
			}
			layers[i].Count = count
		}
		log.Printf("zoom: %d, tiles: %d \n", layers[i].Zoom, layers[i].Count)
		if layers[i].Count > 0 {
			if task.Total == 0 {
				task.Min, task.Max = layers[i].Zoom, layers[i].Zoom
			} else {
				if layers[i].Zoom < task.Min {
					task.Min = layers[i].Zoom
				}
				if layers[i].Zoom > task.Max {
					task.Max = layers[i].Zoom
				}
			}
		}
		if layers[i].Count > math.MaxInt64-task.Total {
			task.initErr = fmt.Errorf("selected tile count exceeds int64 capacity; split the download into smaller jobs")
			return &task
		}
		task.Total += layers[i].Count
	}
	if task.Total == 0 && !task.repairMode {
		task.initErr = fmt.Errorf("no tiles selected; check the layer GeoJSON and zoom levels")
		return &task
	}
	task.layerSources = make([]string, len(layers))
	sourcesByZoom := make(map[int]map[string]bool)
	task.sourceLast = make(map[int]int)
	task.sourceChecks = make(map[int]map[maptile.Tile]int)
	for i, layer := range layers {
		fingerprint, err := FailureSourceFingerprint(layer.URL)
		if err != nil {
			task.initErr = err
			return &task
		}
		task.layerSources[i] = fingerprint
		if layer.Count == 0 {
			continue
		}
		if sourcesByZoom[layer.Zoom] == nil {
			sourcesByZoom[layer.Zoom] = make(map[string]bool)
		}
		sourcesByZoom[layer.Zoom][fingerprint] = true
		task.sourceLast[layer.Zoom] = i
	}
	for zoom, sources := range sourcesByZoom {
		if len(sources) > 1 {
			task.sourceChecks[zoom] = make(map[maptile.Tile]int)
		}
	}

	task.workerCount = viper.GetInt("task.workers")
	task.savePipeSize = viper.GetInt("task.savepipe")
	task.timeDelay = viper.GetInt("task.timedelay")
	if task.workerCount <= 0 || task.savePipeSize < 0 || task.timeDelay < 0 {
		task.initErr = fmt.Errorf("task.workers must be positive; savepipe and timedelay must be nonnegative")
		return &task
	}
	if task.workerCount > maxTaskSlots || task.savePipeSize > maxTaskSlots {
		task.initErr = fmt.Errorf("task.workers and task.savepipe must not exceed %d", maxTaskSlots)
		return &task
	}
	if int64(task.timeDelay) > math.MaxInt64/int64(time.Millisecond) {
		task.initErr = fmt.Errorf("task.timedelay exceeds the supported duration")
		return &task
	}
	task.workers = make(chan maptile.Tile, task.workerCount)
	task.savingpipe = make(chan Tile, task.savePipeSize)

	task.outformat = strings.ToLower(viper.GetString("output.format"))
	if task.outformat == "file" || task.outformat == "s3" {
		var err error
		task.tilePath, err = normalizeTilePath(viper.GetString("output.tile_path"))
		if err != nil {
			task.initErr = fmt.Errorf("output.tile_path: %w", err)
			return &task
		}
	}
	task.resume = viper.GetBool("output.resume") || viper.GetBool("output.append")
	if viper.GetBool("output.append") && !task.repairMode && (task.outformat == "file" || task.outformat == "mbtiles" || task.outformat == "compactv2") && strings.TrimSpace(viper.GetString("output.name")) == "" {
		task.initErr = fmt.Errorf("output.append requires a fixed output.name for file, mbtiles or compactv2 output")
		return &task
	}
	if task.outformat == "compactv2" {
		format, err := archiveFormat(m.Format)
		if err != nil || (format != PNG && format != JPG) {
			task.initErr = fmt.Errorf("Compact v2 supports PNG or JPEG raster tiles only")
			return &task
		}
	}
	task.gzipPBF = task.outformat != "file"
	if viper.IsSet("output.gzip_pbf") {
		task.gzipPBF = viper.GetBool("output.gzip_pbf")
	}
	if task.outformat == "mbtiles" && strings.EqualFold(m.Format, PBF) && !task.gzipPBF {
		task.initErr = fmt.Errorf("MBTiles PBF tiles require gzip encoding; output.gzip_pbf must be true")
	}
	return &task
}

// Bound 范围
func (task *Task) Bound() orb.Bound {
	var bound orb.Bound
	initialized := false
	for _, layer := range task.Layers {
		for _, g := range layer.Collection {
			if g == nil || g.Bound().IsEmpty() {
				continue
			}
			if !initialized {
				bound = g.Bound()
				initialized = true
			} else {
				bound = bound.Union(g.Bound())
			}
		}
	}
	return bound
}

// Center 中心点
func (task *Task) Center() orb.Point { return task.Bound().Center() }

// MetaItems 输出
func (task *Task) MetaItems() map[string]string {
	b := task.Bound()
	c := task.Center()
	data := map[string]string{
		"id":          task.ID,
		"name":        task.Name,
		"description": task.Description,
		"attribution": `<a href="http://www.atlasdata.cn/" target="_blank">&copy; MapCloud</a>`,
		"basename":    task.TileMap.Name,
		"format":      task.TileMap.Format,
		"type":        "baselayer",
		"bounds":      fmt.Sprintf(`%s,%s,%s,%s`, formatDownloadCoordinate(b.Left()), formatDownloadCoordinate(b.Bottom()), formatDownloadCoordinate(b.Right()), formatDownloadCoordinate(b.Top())),
		"center":      fmt.Sprintf(`%s,%s,%d`, formatDownloadCoordinate(c.X()), formatDownloadCoordinate(c.Y()), (task.Min+task.Max)/2),
		"minzoom":     strconv.Itoa(task.Min),
		"maxzoom":     strconv.Itoa(task.Max),
		"json":        task.TileMap.JSON,
	}
	return data
}

// SetupMBTileTables 初始化配置MBTile库
func (task *Task) SetupMBTileTables() error {
	if task.initErr != nil {
		return task.initErr
	}
	if err := task.ctx.Err(); err != nil {
		return err
	}
	if task.File == "" {
		file, err := task.outputPath()
		if err != nil {
			return err
		}
		task.File = file
	}
	if err := validateOutputFilePath(viper.GetString("output.directory"), task.File); err != nil {
		return err
	}
	metadata := task.MetaItems()
	if !task.repairMode {
		var err error
		metadata, err = mergeDownloadMetadata(nil, metadata, false)
		if err != nil {
			return err
		}
	}
	var existing bool
	if task.resume {
		info, err := os.Stat(task.File)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("existing MBTiles output must be a regular database file")
			}
			existing = info.Size() > 0
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if task.repairMode && !existing {
		return fmt.Errorf("repair target must be the original nonempty MBTiles database")
	}
	if !existing {
		if err := validateDownloadMBMetadata(metadata); err != nil {
			return err
		}
	}
	if err := task.ctx.Err(); err != nil {
		return err
	}
	if !task.resume {
		if err := checkSQLiteSidecars(task.File); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(task.File), os.ModePerm); err != nil {
		return err
	}
	if !task.resume {
		if err := task.ctx.Err(); err != nil {
			return err
		}
		if err := checkSQLiteSidecars(task.File); err != nil {
			return err
		}
		if _, err := os.Lstat(task.File); err == nil {
			// Recreate the database without resetting an existing file's
			// access mode. New paths retain SQLite's normal creation policy.
			if err := archiveWriteFileAtomic(task.File, nil); err != nil {
				return fmt.Errorf("recreate MBTiles database: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	db, err := archiveOpenSQLite(task.File, false)
	if err != nil {
		return err
	}
	ready := false
	defer func() {
		if !ready {
			db.Close()
		}
	}()
	// archiveOpenSQLite serializes reads and writes for resume mode too.
	if existing {
		previous, err := readDownloadMBMetadata(db)
		if err != nil {
			return err
		}
		rows, err := db.Query("select zoom_level, tile_column, tile_row, tile_data from tiles limit 0")
		if err != nil {
			return fmt.Errorf("read original MBTiles tiles table: %w", err)
		}
		rows.Close()
		if task.repairMode {
			if !sameTileEncoding(strings.TrimSpace(previous["format"]), strings.TrimSpace(task.TileMap.Format)) {
				return fmt.Errorf("repair target tile format differs from the failure manifest")
			}
			metadata = previous
		} else {
			metadata, err = mergeDownloadMetadata(previous, metadata, false)
			if err != nil {
				return err
			}
		}
	}
	if err := validateDownloadMBMetadata(metadata); err != nil {
		return err
	}
	if err := task.ctx.Err(); err != nil {
		return err
	}
	if err := optimizeConnection(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		"create table if not exists tiles (zoom_level integer, tile_column integer, tile_row integer, tile_data blob);",
		"create table if not exists metadata (name text, value text);",
		"create unique index if not exists name on metadata (name);",
		"create unique index if not exists tile_index on tiles(zoom_level, tile_column, tile_row);",
	} {
		if _, err := tx.Exec(query); err != nil {
			return err
		}
	}
	if !task.repairMode {
		for name, value := range metadata {
			if _, err := tx.Exec("insert or replace into metadata (name, value) values (?, ?)", name, value); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	task.db = db
	ready = true
	return nil
}

// SavePipe 保存瓦片管道
func (task *Task) savePipe() {
	for tile := range task.savingpipe {
		// Combine only tiles already waiting in the configured queue. Never
		// wait for another tile while a transaction owns the sole DB connection:
		// resume checks may need it before another download can be scheduled.
		batch := make([]Tile, len(task.savingpipe)+1)
		batch[0] = tile
		for i := 1; i < len(batch); i++ {
			batch[i] = <-task.savingpipe
		}
		batchErr := saveToMBTiles(batch, task.db)
		for _, tile := range batch {
			err := batchErr
			if err != nil && len(batch) > 1 {
				// The batch has rolled back. Isolate a failing tile so other
				// queued tiles can still commit, as with individual writes.
				err = saveToMBTiles([]Tile{tile}, task.db)
			}
			if err != nil {
				task.recordTileFailure(tile.T, tile.Source, fmt.Errorf("save %v tile to mbtiles: %w", tile.T, err))
			} else {
				task.saved.Add(1)
				task.resolveTileFailure(tile.T, tile.Source)
			}
			task.saveWG.Done()
		}
	}
}

// SaveTile 保存瓦片
func (task *Task) saveTile(tile Tile) error {
	if task.outformat == "s3" {
		return task.s3.saveContext(task.ctx, tile, task.TileMap.Format)
	}
	if task.outformat == "compactv2" {
		return task.compact.Save(task.ctx, int(tile.T.Z), tile.T.X, tile.T.Y, tile.C)
	}
	return saveToFiles(tile, task)
}

// recordError keeps the first failure while allowing other workers to finish.
func (task *Task) recordError(err error) {
	task.errMu.Lock()
	if task.firstErr == nil {
		task.firstErr = err
	}
	task.errMu.Unlock()
	log.Error(safeFailureError(err))
}

// tileFetcher 瓦片加载器
func (task *Task) tileFetcher(mt maptile.Tile, url string) {
	start := time.Now()
	defer task.tileWG.Done() //结束该瓦片请求
	defer func() {
		<-task.workers //workers完成并清退
	}()

	prep := func(t maptile.Tile, url string) string {
		url = strings.Replace(url, "{x}", strconv.Itoa(int(t.X)), -1)
		url = strings.Replace(url, "{y}", strconv.Itoa(int(t.Y)), -1)
		maxY := int(math.Pow(2, float64(t.Z))) - 1
		url = strings.Replace(url, "{-y}", strconv.Itoa(maxY-int(t.Y)), -1)
		url = strings.Replace(url, "{z}", strconv.Itoa(int(t.Z)), -1)
		return url
	}
	tile := prep(mt, url)
	if task.resume {
		exists, err := task.tileExists(task.ctx, mt)
		if err != nil {
			task.recordTileFailure(mt, url, fmt.Errorf("check existing tile %v: %w", mt, err))
			return
		}
		if exists {
			task.skipped.Add(1)
			task.resolveTileFailure(mt, url)
			return
		}
	}
	body, err := task.fetchTile(task.ctx, tile)
	if err != nil {
		task.recordTileFailure(mt, url, fmt.Errorf("fetch tile %v: %w", mt, err))
		return
	}
	data, err := prepareTileData(body, task.TileMap.Format, task.gzipPBF)
	if err != nil {
		task.recordTileFailure(mt, url, fmt.Errorf("prepare tile %v: %w", mt, err))
		return
	}
	td := Tile{T: mt, C: data, Source: url}

	//enable savingpipe
	if task.outformat == "mbtiles" {
		task.saveWG.Add(1)
		task.savingpipe <- td
	} else {
		if err := task.saveTile(td); err != nil {
			task.recordTileFailure(mt, url, fmt.Errorf("save tile %v: %w", mt, err))
			return
		}
		task.saved.Add(1)
		task.resolveTileFailure(mt, url)
	}

	cost := time.Since(start).Milliseconds()
	log.Infof("tile(z:%d, x:%d, y:%d), %dms , %.2f kb ...\n", mt.Z, mt.X, mt.Y, cost, float32(len(body))/1024.0)
}

// DownloadZoom 下载指定层级
func (task *Task) downloadLayer(layer Layer, index int) {
	bar := pb.New64(layer.Count).Prefix(fmt.Sprintf("Zoom %d : ", layer.Zoom)).Postfix("\n")
	bar.Start()
	visit := func(tile maptile.Tile) error {
		if err := task.waitUntilRunning(task.ctx); err != nil {
			return err
		}
		if err := task.claimTileSource(tile, index); err != nil {
			task.recordError(err)
			task.Cancel()
			return err
		}
		if task.resume && (task.outformat == "file" || task.outformat == "mbtiles" || task.outformat == "compactv2") {
			exists, err := task.tileExists(task.ctx, tile)
			if err != nil || exists {
				if err != nil {
					task.recordTileFailure(tile, layer.URL, fmt.Errorf("check existing tile %v: %w", tile, err))
				} else {
					task.skipped.Add(1)
					task.resolveTileFailure(tile, layer.URL)
				}
				bar.Increment()
				task.Bar.Increment()
				return nil
			}
		}
		select {
		case task.workers <- tile:
		case <-task.ctx.Done():
			return task.ctx.Err()
		}
		// Pause may have arrived while waiting for a worker slot.
		if err := task.waitUntilRunning(task.ctx); err != nil {
			<-task.workers
			return err
		}
		if task.timeDelay > 0 {
			timer := time.NewTimer(time.Duration(task.timeDelay) * time.Millisecond)
			select {
			case <-timer.C:
			case <-task.ctx.Done():
				timer.Stop()
				<-task.workers
				return task.ctx.Err()
			}
		}
		if err := task.waitUntilRunning(task.ctx); err != nil {
			<-task.workers
			return err
		}
		bar.Increment()
		task.Bar.Increment()
		task.tileWG.Add(1)
		go task.tileFetcher(tile, layer.URL)
		return nil
	}
	var err error
	if task.repairMode {
		for _, tile := range task.repairTiles[index] {
			if err = visit(tile); err != nil {
				break
			}
		}
	} else {
		err = cover.Walk(task.ctx, layer.Collection, maptile.Zoom(layer.Zoom), visit)
	}
	if err != nil {
		task.recordError(err)
	}
	task.tileWG.Wait()
	// A following layer may resume overlapping coordinates. Verify only after
	// the previous layer's SQLite writes have actually committed.
	task.saveWG.Wait()
	if task.sourceLast[layer.Zoom] == index {
		task.sourceTiles -= len(task.sourceChecks[layer.Zoom])
		delete(task.sourceChecks, layer.Zoom)
	}
	bar.FinishPrint(fmt.Sprintf("Task %s Zoom %d finished ~", task.ID, layer.Zoom))
}

func (task *Task) claimTileSource(tile maptile.Tile, layerIndex int) error {
	sources := task.sourceChecks[int(tile.Z)]
	if sources == nil {
		return nil
	}
	current := task.layerSources[layerIndex]
	if previous, exists := sources[tile]; exists {
		if task.layerSources[previous] != current {
			return fmt.Errorf("tile %d/%d/%d belongs to conflicting source layers; use separate outputs", tile.Z, tile.X, tile.Y)
		}
		return nil
	}
	// The last layer only checks earlier claims. No later layer can need a
	// claim for a new coordinate, so do not retain it or apply the index cap.
	if task.sourceLast[int(tile.Z)] == layerIndex {
		return nil
	}
	// Only mixed-source zooms need this index. Bound its retained coordinates
	// so overlap validation cannot grow for the entire download without limit.
	const maxSourceCheckTiles = 1_000_000
	if task.sourceTiles >= maxSourceCheckTiles {
		return fmt.Errorf("mixed-source layers exceed the %d-tile overlap check limit; split their source layers into separate jobs", maxSourceCheckTiles)
	}
	sources[tile] = layerIndex
	task.sourceTiles++
	return nil
}

// Download 开启下载任务
func (task *Task) Download() error {
	if task.initErr != nil {
		return task.initErr
	}
	if !task.started.CompareAndSwap(false, true) {
		return fmt.Errorf("download task has already been started; create a new task to resume")
	}
	defer task.cancel()
	if err := task.ctx.Err(); err != nil {
		return err
	}
	if task.repairMode && task.Total == 0 {
		return nil
	}
	if err := task.setupHTTP(); err != nil {
		return err
	}
	defer task.httpClient.CloseIdleConnections()
	var saveDone chan struct{}
	switch task.outformat {
	case "s3":
		if task.s3 == nil {
			out, err := newS3Output(task.ctx)
			if err != nil {
				return err
			}
			task.s3 = out
		}
		task.s3.tilePath = task.tilePath
		if task.resume {
			if err := task.s3.validateResumeTarget(task.ctx, task.TileMap.Format); err != nil {
				return err
			}
		}
	case "file", "mbtiles", "compactv2":
		if task.File == "" {
			file, err := task.outputPath()
			if err != nil {
				return err
			}
			task.File = file
		}
		if task.repairMode {
			if err := validateExistingRepairTarget(task.outformat, task.File); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported output.format %q (expected file, mbtiles, compactv2 or s3)", task.outformat)
	}
	if err := task.setupFailureManifest(); err != nil {
		return err
	}
	if err := task.ctx.Err(); err != nil {
		return err
	}
	switch task.outformat {
	case "compactv2":
		if err := task.validateFileOutputSafety(); err != nil {
			return err
		}
		metadata := task.MetaItems()
		if task.repairMode {
			var err error
			metadata, err = compactReadMetadata(task.File)
			if err != nil {
				return fmt.Errorf("read original Compact v2 metadata: %w", err)
			}
			if !sameTileEncoding(strings.TrimSpace(metadata["format"]), strings.TrimSpace(task.TileMap.Format)) {
				return fmt.Errorf("repair target tile format differs from the failure manifest")
			}
		}
		var err error
		task.compact, err = compactOpen(task.File, metadata, task.resume)
		if err != nil {
			return fmt.Errorf("initialize Compact v2: %w", err)
		}
	case "mbtiles":
		if err := task.SetupMBTileTables(); err != nil {
			return fmt.Errorf("initialize mbtiles: %w", err)
		}
		defer task.db.Close()
		saveDone = make(chan struct{})
		go func() {
			task.savePipe()
			close(saveDone)
		}()
		defer func() {
			if saveDone != nil {
				close(task.savingpipe)
				<-saveDone
			}
		}()
	case "file":
		if err := task.validateFileOutputSafety(); err != nil {
			return err
		}
		if compact, err := compactIsDirectory(task.File); err != nil {
			return err
		} else if compact {
			return fmt.Errorf("file output cannot write into an existing Compact v2 cache; keep output.format compactv2 or choose a new directory")
		}
		if err := task.validateFileResumeMetadata(); err != nil {
			return err
		}
		if err := os.MkdirAll(task.File, os.ModePerm); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
		if !task.repairMode {
			if err := task.saveFileMetadata(); err != nil {
				return fmt.Errorf("save file metadata: %w", err)
			}
		}
	}
	//g orb.Geometry, minz int, maxz int
	task.Bar = pb.New64(task.Total).Prefix("Task : ").Postfix("\n")
	// task.Bar.SetRefreshRate(10 * time.Second)
	// task.Bar.Format("<.- >")
	task.Bar.Start()
	for i, layer := range task.Layers {
		if layer.Count == 0 {
			continue
		}
		if task.ctx.Err() != nil {
			break
		}
		task.downloadLayer(layer, i)
	}
	if saveDone != nil {
		close(task.savingpipe)
		<-saveDone
		saveDone = nil
	}
	// All tile writes have finished. Finalize metadata even after cancellation
	// so already downloaded bundles can be resumed or served independently.
	if task.compact != nil {
		if err := task.compact.Close(); err != nil {
			task.recordError(fmt.Errorf("finalize Compact v2: %w", err))
		}
	}
	if err := task.persistFailureManifest(); err != nil {
		task.recordError(err)
	}
	stats := task.Stats()
	log.Infof("Task %s: saved=%d skipped=%d failed=%d", task.ID, stats.Saved, stats.Skipped, stats.Failed)
	task.errMu.Lock()
	err := task.firstErr
	if err == nil {
		err = task.ctx.Err()
	}
	task.errMu.Unlock()
	if err != nil {
		task.Bar.FinishPrint(fmt.Sprintf("Task %s failed ~", task.ID))
		return err
	}
	task.Bar.FinishPrint(fmt.Sprintf("Task %s finished ~", task.ID))
	return nil
}
