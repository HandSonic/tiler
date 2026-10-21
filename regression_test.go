package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func configureRegressionOutput(t *testing.T) string {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	viper.Set("task.workers", 2)
	viper.Set("task.savepipe", 1)
	viper.Set("task.timedelay", 0)
	viper.Set("task.timeout_seconds", 1)
	viper.Set("task.retries", 0)
	viper.Set("output.format", "file")
	viper.Set("output.directory", dir)
	return dir
}

func newRegressionTask(t *testing.T, tileURL string) *Task {
	t.Helper()
	task := NewTask([]Layer{{Zoom: 0, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{
		Name: "regression", URL: tileURL, Format: PNG, Schema: "xyz",
	})
	if task == nil {
		t.Fatal("NewTask returned nil for a point layer")
	}
	t.Cleanup(task.Cancel)
	return task
}

func awaitRegressionError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}

func TestFetchTileFollowsRedirectsAndUsesConfiguredHeaders(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("task.user_agent", "tiler-test-agent")
	viper.Set("task.referer", "https://example.test/maps")
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Header.Get("User-Agent") != "tiler-test-agent" || r.Header.Get("Referer") != "https://example.test/maps" {
			t.Errorf("%s headers = User-Agent %q, Referer %q; want configured values", r.URL.Path, r.Header.Get("User-Agent"), r.Header.Get("Referer"))
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/tile", http.StatusFound)
			return
		}
		io.WriteString(w, "tile bytes")
	}))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	if err := task.setupHTTP(); err != nil {
		t.Fatal(err)
	}
	body, err := task.fetchTile(task.ctx, server.URL+"/redirect")
	if err != nil || string(body) != "tile bytes" {
		t.Fatalf("fetchTile = %q, %v; want the redirected body", body, err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Errorf("requests = %d; want redirect plus destination", got)
	}
}

func TestFetchTileRetriesTransientHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("task.retries", 1)
			var requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&requests, 1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, "retried tile")
			}))
			defer server.Close()
			task := newRegressionTask(t, server.URL)
			if err := task.setupHTTP(); err != nil {
				t.Fatal(err)
			}
			body, err := task.fetchTile(task.ctx, server.URL)
			if err != nil || string(body) != "retried tile" {
				t.Fatalf("fetchTile = %q, %v; want successful retry", body, err)
			}
			if got := atomic.LoadInt32(&requests); got != 2 {
				t.Errorf("requests = %d; want 2", got)
			}
		})
	}
}

func TestFetchTileHonorsHTTPTimeout(t *testing.T) {
	configureRegressionOutput(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	if err := task.setupHTTP(); err != nil {
		t.Fatal(err)
	}
	if task.httpClient.Timeout != time.Second {
		t.Fatalf("configured HTTP timeout = %s; want 1s", task.httpClient.Timeout)
	}
	// Keep the test short while exercising the same http.Client timeout path.
	task.httpClient.Timeout = 25 * time.Millisecond
	_, err := task.fetchTile(task.ctx, server.URL)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("fetchTile error = %v; want a timeout", err)
	}
}

func TestDownloadFailureDoesNotLeaveResumableFiles(t *testing.T) {
	for _, failure := range []string{"404", "partial response", "invalid image"} {
		t.Run(failure, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.resume", true)
			var requests int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requests, 1)
				switch failure {
				case "404":
					w.WriteHeader(http.StatusNotFound)
				case "partial response":
					w.Header().Set("Content-Length", "100")
					io.WriteString(w, "truncated")
				default:
					io.WriteString(w, "<html>not a tile</html>")
				}
			}))
			defer server.Close()
			task := newRegressionTask(t, server.URL)
			if err := task.Download(); err == nil {
				t.Fatal("failed HTTP/image response should fail Download")
			}
			if got := atomic.LoadInt32(&requests); got != 1 {
				t.Errorf("requests = %d; want 1 with retries disabled", got)
			}
			if err := filepath.WalkDir(task.File, func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && entry.Name() != "metadata.json" && entry.Name() != "failures.json" {
					t.Errorf("failed download left file %s", path)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResumeSkipsExistingTilesAndKeepsOutputPath(t *testing.T) {
	for _, format := range []string{"file", "mbtiles"} {
		for _, name := range []string{"", "fixed-map"} {
			t.Run(format+"/"+name, func(t *testing.T) {
				dir := configureRegressionOutput(t)
				viper.Set("output.format", format)
				viper.Set("output.resume", true)
				viper.Set("output.name", name)
				body := tileDataPNG(t)
				var requests int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					atomic.AddInt32(&requests, 1)
					w.Write(body)
				}))
				defer server.Close()
				var outputPath string
				for pass := 0; pass < 2; pass++ {
					task := newRegressionTask(t, server.URL)
					if err := task.Download(); err != nil {
						t.Fatalf("download pass %d: %v", pass+1, err)
					}
					if pass == 0 {
						outputPath = task.File
					} else if task.File != outputPath {
						t.Errorf("resume output path changed from %s to %s", outputPath, task.File)
					}
				}
				if got := atomic.LoadInt32(&requests); got != 1 {
					t.Errorf("source requests across two runs = %d; want 1", got)
				}
				wantName := name
				if wantName == "" {
					wantName = "regression-z0-0"
				}
				if format == "mbtiles" {
					wantName += ".mbtiles"
				}
				if outputPath != filepath.Join(dir, wantName) {
					t.Errorf("output = %s; want stable name %s", outputPath, wantName)
				}
				var stored []byte
				if format == "file" {
					var err error
					stored, err = os.ReadFile(filepath.Join(outputPath, "0", "0", "0.png"))
					if err != nil {
						t.Fatal(err)
					}
				} else {
					db, err := sql.Open("sqlite3", outputPath)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err := db.QueryRow("select tile_data from tiles where zoom_level=0 and tile_column=0 and tile_row=0").Scan(&stored); err != nil {
						t.Fatal(err)
					}
				}
				if !bytes.Equal(stored, body) {
					t.Error("resume changed or lost the stored tile")
				}
			})
		}
	}
}

func TestFileSaveFailureCleansTemporaryFile(t *testing.T) {
	configureRegressionOutput(t)
	task := newRegressionTask(t, "http://example.test")
	task.File = t.TempDir()
	finalPath := filepath.Join(task.File, "0", "0", "0.png")
	if err := os.MkdirAll(finalPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := saveToFiles(Tile{T: maptile.Tile{Z: 0}, C: tileDataPNG(t)}, task); err == nil {
		t.Fatal("saving over a directory should fail")
	}
	leftovers, err := filepath.Glob(filepath.Join(task.File, "0", "0", ".tile-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files after save failure = %v, %v", leftovers, err)
	}
}

func TestTaskPauseResumeAndCancel(t *testing.T) {
	configureRegressionOutput(t)
	task := newRegressionTask(t, "http://example.test")
	task.Pause()
	task.Pause() // Repeated commands must be safe.
	task.stateMu.Lock()
	paused := task.paused
	task.stateMu.Unlock()
	if !paused {
		t.Fatal("Pause did not put the task in a paused state")
	}
	result := make(chan error, 1)
	go func() { result <- task.waitUntilRunning(task.ctx) }()
	select {
	case err := <-result:
		t.Fatalf("paused task proceeded early: %v", err)
	default:
	}
	task.Resume()
	task.Resume()
	if err := awaitRegressionError(t, result); err != nil {
		t.Fatalf("resume did not release the paused waiter: %v", err)
	}
	task.Pause()
	go func() { result <- task.waitUntilRunning(task.ctx) }()
	task.Cancel()
	if err := awaitRegressionError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled paused task returned %v; want context.Canceled", err)
	}
}

func TestCancelStopsInFlightDownload(t *testing.T) {
	configureRegressionOutput(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	result := make(chan error, 1)
	go func() { result <- task.Download() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("download request did not start")
	}
	task.Cancel()
	if err := awaitRegressionError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled download returned %v; want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(task.File, "0", "0", "0.png")); !os.IsNotExist(err) {
		t.Fatalf("canceled download produced a tile file: %v", err)
	}
}

func TestConsoleControls(t *testing.T) {
	configureRegressionOutput(t)
	task := newRegressionTask(t, "http://example.test")
	var output bytes.Buffer
	task.readControls(strings.NewReader("pause\nresume\nq\n"), &output)
	if !errors.Is(task.ctx.Err(), context.Canceled) {
		t.Error("q did not cancel the task")
	}
	if !strings.Contains(output.String(), "Paused") || !strings.Contains(output.String(), "Resumed") {
		t.Errorf("console output = %q; want pause and resume feedback", output.String())
	}
}

func TestResumeRepairsEmptyMBTilesRecord(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	viper.Set("output.resume", true)
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	seed := newRegressionTask(t, server.URL)
	if err := seed.SetupMBTileTables(); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec("insert into tiles (zoom_level,tile_column,tile_row,tile_data) values (0,0,0,x'')"); err != nil {
		t.Fatal(err)
	}
	if err := seed.db.Close(); err != nil {
		t.Fatal(err)
	}
	task := newRegressionTask(t, server.URL)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", task.File)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored []byte
	if err := db.QueryRow("select tile_data from tiles where zoom_level=0 and tile_column=0 and tile_row=0").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, body) {
		t.Fatal("resume did not repair the empty tile")
	}
}

func TestMBTilesOutputDoesNotCreateLooseTileFiles(t *testing.T) {
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != task.File {
		t.Fatalf("MBTiles should leave only the database, got %v", files)
	}
}
