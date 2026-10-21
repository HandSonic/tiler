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

// MBTileVersion mbtiles版本号
const MBTileVersion = "1.2"

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
	Current      int64
	Bar          *pb.ProgressBar
	db           *sql.DB
	workerCount  int
	savePipeSize int
	timeDelay    int
	bufSize      int
	tileWG       sync.WaitGroup
	workers      chan maptile.Tile
	savingpipe   chan Tile
	tileSet      Set
	outformat    string
	s3           *s3Output
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
}

// NewTask 创建下载任务
func NewTask(layers []Layer, m TileMap) *Task {
	if len(layers) == 0 {
		return nil
	}
	id, _ := shortid.Generate()

	task := Task{
		ID:      id,
		Name:    m.Name,
		Layers:  layers,
		Min:     m.Min,
		Max:     m.Max,
		TileMap: m,
	}

	task.ctx, task.cancel = context.WithCancel(context.Background())
	task.Min, task.Max = layers[0].Zoom, layers[0].Zoom
	for i := 0; i < len(layers); i++ {
		if layers[i].Zoom < 0 || layers[i].Zoom > 30 {
			task.initErr = fmt.Errorf("layer zoom must be between 0 and 30")
			return &task
		}
		if layers[i].Zoom < task.Min {
			task.Min = layers[i].Zoom
		}
		if layers[i].Zoom > task.Max {
			task.Max = layers[i].Zoom
		}
		if layers[i].URL == "" {
			layers[i].URL = m.URL
		}
		if err := validateTileURL(layers[i].URL); err != nil {
			task.initErr = fmt.Errorf("zoom %d: %w", layers[i].Zoom, err)
			return &task
		}
		count, err := cover.Count(layers[i].Collection, maptile.Zoom(layers[i].Zoom))
		if err != nil {
			task.initErr = err
			return &task
		}
		layers[i].Count = count
		log.Printf("zoom: %d, tiles: %d \n", layers[i].Zoom, layers[i].Count)
		task.Total += layers[i].Count
	}
	if task.Total == 0 {
		task.initErr = fmt.Errorf("no tiles selected; check the layer GeoJSON and zoom levels")
		return &task
	}

	task.workerCount = viper.GetInt("task.workers")
	task.savePipeSize = viper.GetInt("task.savepipe")
	task.timeDelay = viper.GetInt("task.timedelay")
	if task.workerCount <= 0 || task.savePipeSize < 0 || task.timeDelay < 0 || viper.GetInt("task.mergebuf") < 0 {
		task.initErr = fmt.Errorf("task.workers must be positive; savepipe, timedelay and mergebuf must be nonnegative")
		return &task
	}
	task.workers = make(chan maptile.Tile, task.workerCount)
	task.savingpipe = make(chan Tile, task.savePipeSize)
	task.bufSize = viper.GetInt("task.mergebuf")
	task.tileSet = Set{M: make(maptile.Set)}

	task.outformat = strings.ToLower(viper.GetString("output.format"))
	task.resume = viper.GetBool("output.resume")
	task.gzipPBF = task.outformat != "file"
	if viper.IsSet("output.gzip_pbf") {
		task.gzipPBF = viper.GetBool("output.gzip_pbf")
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
		"type":        task.TileMap.Schema,
		"pixel_scale": strconv.Itoa(TileSize),
		"version":     MBTileVersion,
		"bounds":      fmt.Sprintf(`%f,%f,%f,%f`, b.Left(), b.Bottom(), b.Right(), b.Top()),
		"center":      fmt.Sprintf(`%f,%f,%d`, c.X(), c.Y(), (task.Min+task.Max)/2),
		"minzoom":     strconv.Itoa(task.Min),
		"maxzoom":     strconv.Itoa(task.Max),
		"json":        task.TileMap.JSON,
	}
	return data
}

// SetupMBTileTables 初始化配置MBTile库
func (task *Task) SetupMBTileTables() error {
	if task.File == "" {
		file, err := task.outputPath()
		if err != nil {
			return err
		}
		task.File = file
	}
	if err := os.MkdirAll(filepath.Dir(task.File), os.ModePerm); err != nil {
		return err
	}
	if !task.resume {
		if err := os.Remove(task.File); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	db, err := sql.Open("sqlite3", task.File)
	if err != nil {
		return err
	}
	ready := false
	defer func() {
		if !ready {
			db.Close()
		}
	}()
	// Serializing reads and writes also prevents SQLite locking races in resume mode.
	db.SetMaxOpenConns(1)
	if err := optimizeConnection(db); err != nil {
		return err
	}
	for _, query := range []string{
		"create table if not exists tiles (zoom_level integer, tile_column integer, tile_row integer, tile_data blob);",
		"create table if not exists metadata (name text, value text);",
		"create unique index if not exists name on metadata (name);",
		"create unique index if not exists tile_index on tiles(zoom_level, tile_column, tile_row);",
	} {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}
	for name, value := range task.MetaItems() {
		if _, err := db.Exec("insert or replace into metadata (name, value) values (?, ?)", name, value); err != nil {
			return err
		}
	}
	task.db = db
	ready = true
	return nil
}

// SavePipe 保存瓦片管道
func (task *Task) savePipe() {
	for tile := range task.savingpipe {
		err := saveToMBTile(tile, task.db)
		if err != nil {
			task.tileFailure(fmt.Errorf("save %v tile to mbtiles: %w", tile.T, err))
		} else {
			task.saved.Add(1)
		}
	}
}

// SaveTile 保存瓦片
func (task *Task) saveTile(tile Tile) error {
	if task.outformat == "s3" {
		return task.s3.saveContext(task.ctx, tile, task.TileMap.Format)
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
	log.Error(err)
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
			task.tileFailure(fmt.Errorf("check existing tile %v: %w", mt, err))
			return
		}
		if exists {
			task.skipped.Add(1)
			return
		}
	}
	body, err := task.fetchTile(task.ctx, tile)
	if err != nil {
		task.tileFailure(fmt.Errorf("fetch tile %v: %w", mt, err))
		return
	}
	data, err := prepareTileData(body, task.TileMap.Format, task.gzipPBF)
	if err != nil {
		task.tileFailure(fmt.Errorf("prepare tile %v: %w", mt, err))
		return
	}
	td := Tile{T: mt, C: data}

	//enable savingpipe
	if task.outformat == "mbtiles" {
		task.savingpipe <- td
	} else {
		if err := task.saveTile(td); err != nil {
			task.tileFailure(fmt.Errorf("save tile %v: %w", mt, err))
			return
		}
		task.saved.Add(1)
	}

	cost := time.Since(start).Milliseconds()
	log.Infof("tile(z:%d, x:%d, y:%d), %dms , %.2f kb, %s ...\n", mt.Z, mt.X, mt.Y, cost, float32(len(body))/1024.0, tile)
}

// DownloadZoom 下载指定层级
func (task *Task) downloadLayer(layer Layer) {
	bar := pb.New64(layer.Count).Prefix(fmt.Sprintf("Zoom %d : ", layer.Zoom)).Postfix("\n")
	bar.Start()
	err := cover.Walk(task.ctx, layer.Collection, maptile.Zoom(layer.Zoom), func(tile maptile.Tile) error {
		if err := task.waitUntilRunning(task.ctx); err != nil {
			return err
		}
		if task.resume && task.outformat == "file" {
			exists, err := task.tileExists(task.ctx, tile)
			if err != nil || exists {
				if err != nil {
					task.tileFailure(fmt.Errorf("check existing tile %v: %w", tile, err))
				} else {
					task.skipped.Add(1)
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
	})
	if err != nil {
		task.recordError(err)
	}
	task.tileWG.Wait()
	bar.FinishPrint(fmt.Sprintf("Task %s Zoom %d finished ~", task.ID, layer.Zoom))
}

// Download 开启下载任务
func (task *Task) Download() error {
	if task.initErr != nil {
		return task.initErr
	}
	if err := task.setupHTTP(); err != nil {
		return err
	}
	defer task.cancel()
	var saveDone chan struct{}
	switch task.outformat {
	case "s3":
		out, err := newS3Output(task.ctx)
		if err != nil {
			return err
		}
		task.s3 = out
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
	case "file":
		if task.File == "" {
			file, err := task.outputPath()
			if err != nil {
				return err
			}
			task.File = file
		}
		if err := os.MkdirAll(task.File, os.ModePerm); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
		if err := task.saveFileMetadata(); err != nil {
			return fmt.Errorf("save file metadata: %w", err)
		}
	default:
		return fmt.Errorf("unsupported output.format %q (expected file, mbtiles or s3)", task.outformat)
	}
	//g orb.Geometry, minz int, maxz int
	task.Bar = pb.New64(task.Total).Prefix("Task : ").Postfix("\n")
	// task.Bar.SetRefreshRate(10 * time.Second)
	// task.Bar.Format("<.- >")
	task.Bar.Start()
	for _, layer := range task.Layers {
		if task.ctx.Err() != nil {
			break
		}
		task.downloadLayer(layer)
	}
	if saveDone != nil {
		close(task.savingpipe)
		<-saveDone
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
