package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func configureTaskTest(t *testing.T, output string) string {
	t.Helper()
	configureS3Test(t)
	viper.Set("output.format", output)
	viper.Set("task.workers", 2)
	viper.Set("task.savepipe", 4)
	viper.Set("task.timedelay", 0)
	dir := filepath.Join(t.TempDir(), "tiles")
	viper.Set("output.directory", dir)
	return dir
}

func taskForTest(url, format string, points orb.Collection) *Task {
	return NewTask([]Layer{{Zoom: 2, Collection: points}}, TileMap{
		Name: "test-map", Min: 2, Max: 2, Format: format,
		URL: url + "/{z}/{x}/{y}",
	})
}

func TestTaskDownloadS3(t *testing.T) {
	for _, format := range []string{PNG, PBF} {
		t.Run(format, func(t *testing.T) {
			outdir := configureTaskTest(t, "s3")
			viper.Set("output.s3.prefix", "maps")
			body := []byte{0x1a, 0x05, 0x0a, 0x03, 'm', 'a', 'p'}
			contentType := "application/x-protobuf"
			if format == PNG {
				body = tileDataPNG(t)
				contentType = "image/png"
			}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(body)
			}))
			defer source.Close()
			type upload struct {
				path, contentType, encoding string
				body                        []byte
				err                         error
			}
			uploads := make(chan upload, 2)
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			var completed int32
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				select {
				case uploads <- upload{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), data, err}:
				default:
					t.Errorf("unexpected extra S3 upload: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				entered <- struct{}{}
				<-release
				atomic.AddInt32(&completed, 1)
				w.WriteHeader(http.StatusOK)
			}))
			defer sink.Close()
			defer releaseAll()
			viper.Set("output.s3.endpoint", sink.URL)
			task := taskForTest(source.URL, format, orb.Collection{orb.Point{-135, 45}, orb.Point{45, -45}})
			defer task.Cancel()
			result := make(chan error, 1)
			go func() { result <- task.Download() }()
			for i := 0; i < 2; i++ {
				select {
				case <-entered:
				case err := <-result:
					t.Fatalf("Download returned before both uploads started: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("both S3 uploads did not start")
				}
			}
			select {
			case err := <-result:
				t.Fatalf("Download returned before uploads completed: %v", err)
			default:
			}
			releaseAll()
			if err := awaitRegressionError(t, result); err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(&completed); got != 2 {
				t.Errorf("completed uploads = %d; want 2 before Download returns", got)
			}
			wantPaths := map[string]bool{
				"/test-bucket/maps/2/0/1." + format: true,
				"/test-bucket/maps/2/2/2." + format: true,
			}
			for i := 0; i < 2; i++ {
				got := <-uploads
				if got.err != nil {
					t.Fatal(got.err)
				}
				if !wantPaths[got.path] {
					t.Errorf("unexpected or duplicate object path %q", got.path)
				}
				delete(wantPaths, got.path)
				if got.contentType != contentType {
					t.Errorf("Content-Type = %q; want %q", got.contentType, contentType)
				}
				data := got.body
				if format == PBF {
					if got.encoding != "gzip" {
						t.Errorf("PBF Content-Encoding = %q; want gzip", got.encoding)
					}
					zr, err := gzip.NewReader(bytes.NewReader(data))
					if err != nil {
						t.Fatal(err)
					}
					data, err = io.ReadAll(zr)
					zr.Close()
					if err != nil {
						t.Fatal(err)
					}
				} else if got.encoding != "" {
					t.Errorf("PNG Content-Encoding = %q; want none", got.encoding)
				}
				if !bytes.Equal(data, body) {
					t.Error("saved tile differs from the source tile")
				}
			}
			if task.File != "" {
				t.Errorf("S3 mode set a local output path: %q", task.File)
			}
			if _, err := os.Stat(outdir); !os.IsNotExist(err) {
				t.Errorf("S3 mode created the local output directory: %v", err)
			}
		})
	}
}

func TestTaskDownloadS3AccessDenied(t *testing.T) {
	configureTaskTest(t, "s3")
	body := tileDataPNG(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer source.Close()
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	defer sink.Close()
	viper.Set("output.s3.endpoint", sink.URL)
	task := taskForTest(source.URL, PNG, orb.Collection{orb.Point{-135, 45}})
	if err := task.Download(); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("Download error = %v; want the S3 AccessDenied error", err)
	}
}

func TestTaskDownloadLocalOutputs(t *testing.T) {
	for _, output := range []string{"file", "mbtiles"} {
		t.Run(output, func(t *testing.T) {
			configureTaskTest(t, output)
			body := tileDataPNG(t)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
			defer source.Close()
			points := orb.Collection{orb.Point{-135, 45}, orb.Point{-45, 45}, orb.Point{45, -45}, orb.Point{135, -45}}
			task := taskForTest(source.URL, PNG, points)
			if err := task.Download(); err != nil {
				t.Fatal(err)
			}
			if output == "file" {
				for _, tile := range []string{"2/0/1.png", "2/1/1.png", "2/2/2.png", "2/3/2.png"} {
					data, err := os.ReadFile(filepath.Join(task.File, filepath.FromSlash(tile)))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(data, body) {
						t.Errorf("saved tile %s differs from the source", tile)
					}
				}
				return
			}
			db, err := sql.Open("sqlite3", task.File)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := db.Query("SELECT zoom_level, tile_column, tile_row, tile_data FROM tiles")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			want := map[string]bool{"2/0/2": true, "2/1/2": true, "2/2/1": true, "2/3/1": true}
			for rows.Next() {
				var z, x, y int
				var data []byte
				if err := rows.Scan(&z, &x, &y, &data); err != nil {
					t.Fatal(err)
				}
				key := fmt.Sprintf("%d/%d/%d", z, x, y)
				if !want[key] {
					t.Errorf("unexpected MBTiles coordinate %s", key)
				}
				delete(want, key)
				if !bytes.Equal(data, body) {
					t.Errorf("MBTiles tile %s differs from the source", key)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(want) != 0 {
				t.Errorf("Download returned before all MBTiles rows were saved: missing %v", want)
			}
		})
	}
}

func TestTaskMBTilesBatchCommitFailureAndRetry(t *testing.T) {
	db, err := archiveOpenSQLite(filepath.Join(t.TempDir(), "tiles.mbtiles"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Both inserts succeed; the missing parent makes COMMIT fail. This checks
	// that queued tiles are acknowledged only after the whole batch commits.
	_, err = db.Exec(`PRAGMA foreign_keys=ON;
		CREATE TABLE allowed (x INTEGER PRIMARY KEY);
		INSERT INTO allowed VALUES (0);
		CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB,
			FOREIGN KEY(tile_column) REFERENCES allowed(x) DEFERRABLE INITIALLY DEFERRED);`)
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{db: db, manifest: newFailureManifest("batch", PNG)}
	tiles := []Tile{
		{T: maptile.Tile{Z: 1}, C: []byte("first"), Source: manifestTestURL},
		{T: maptile.Tile{Z: 1, X: 1}, C: []byte("second"), Source: manifestTestURL},
	}
	writeQueued := func() {
		task.savingpipe = make(chan Tile, len(tiles))
		for _, tile := range tiles {
			task.saveWG.Add(1)
			task.savingpipe <- tile
		}
		close(task.savingpipe)
		task.savePipe()
		task.saveWG.Wait()
	}
	writeQueued()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM tiles").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || task.Stats() != (DownloadStats{Failed: 2}) || task.manifest.Len() != 2 {
		t.Fatalf("failed commit was acknowledged: rows=%d stats=%+v failures=%d", count, task.Stats(), task.manifest.Len())
	}
	if _, err := db.Exec("INSERT INTO allowed VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	writeQueued()
	if err := db.QueryRow("SELECT count(*) FROM tiles").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 || task.Stats() != (DownloadStats{Saved: 2, Failed: 2}) || task.manifest.Len() != 0 {
		t.Fatalf("retry did not commit and resolve failures: rows=%d stats=%+v failures=%d", count, task.Stats(), task.manifest.Len())
	}
}
