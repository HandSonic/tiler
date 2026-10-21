package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func layoutTask(t *testing.T, source string) *Task {
	t.Helper()
	task := NewTask([]Layer{{Zoom: 2, URL: source, Collection: orb.Collection{orb.Point{90, 45}}}}, TileMap{Name: "layout", Format: PNG, Schema: "xyz", URL: source})
	if task == nil || task.initErr != nil {
		t.Fatalf("initialize layout task: %v", task)
	}
	return task
}

func TestFileTileLayoutResumeAndRepair(t *testing.T) {
	for _, pattern := range []string{"tiles/{z}/{y}/{x}.{format}", "tiles/{z}/{-y}/{x}"} {
		t.Run(pattern, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", "file")
			viper.Set("output.name", "layout")
			viper.Set("output.resume", true)
			viper.Set("output.tile_path", pattern)
			body := tileDataPNG(t)
			var fail atomic.Bool
			fail.Store(true)
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if fail.Load() {
					http.Error(w, "temporary failure", http.StatusServiceUnavailable)
					return
				}
				w.Write(body)
			}))
			defer server.Close()
			source := server.URL + "/{z}/{x}/{y}.png"
			first := layoutTask(t, source)
			if err := first.Download(); err == nil {
				t.Fatal("expected initial request failure")
			}
			report := filepath.Join(first.File, "failures.json")
			manifest, err := LoadFailureManifest(report)
			if err != nil {
				t.Fatal(err)
			}
			target, ok := manifest.Target()
			if !ok || target.TilePath != pattern {
				t.Fatalf("manifest did not bind layout: %+v", target)
			}
			viper.Set("output.tile_path", defaultTilePath)
			if _, err := NewRepairTask(first.Layers, first.TileMap, manifest, report); err == nil {
				t.Fatal("repair accepted a changed output template")
			}
			if err := layoutTask(t, source).Download(); err == nil {
				t.Fatal("ordinary resume accepted changed file metadata layout")
			}
			if requests.Load() != 1 {
				t.Fatal("layout mismatch made another source request")
			}
			viper.Set("output.tile_path", pattern)
			fail.Store(false)
			repair, err := NewRepairTask(first.Layers, first.TileMap, manifest, report)
			if err != nil {
				t.Fatal(err)
			}
			if err := repair.Download(); err != nil {
				t.Fatal(err)
			}
			relative, err := renderTilePath(pattern, maptile.Tile{Z: 2, X: 3, Y: 1}, PNG)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(first.File, filepath.FromSlash(relative)))
			if err != nil || string(got) != string(body) {
				t.Fatalf("incorrect output tile: %v", err)
			}
			if err := layoutTask(t, source).Download(); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 2 {
				t.Fatalf("resume redownloaded an existing custom-layout tile: %d requests", requests.Load())
			}
		})
	}
}

func TestS3TileLayoutHeadUsesSameKeyAndChecksIdentity(t *testing.T) {
	configureS3Test(t)
	viper.Set("output.tile_path", "{z}/{y}/{x}.{format}")
	viper.Set("output.s3.prefix", "maps")
	var mu sync.Mutex
	var stored []byte
	var metadata http.Header
	unmarked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/test-bucket/maps/2/1/3.png" {
			t.Errorf("incorrect S3 layout: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			stored, _ = io.ReadAll(r.Body)
			metadata = r.Header.Clone()
		case http.MethodHead:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(stored)))
			if !unmarked {
				for key, values := range metadata {
					if strings.HasPrefix(strings.ToLower(key), "x-amz-meta-") {
						w.Header()[key] = values
					}
				}
			}
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()
	viper.Set("output.s3.endpoint", server.URL)
	out, err := newS3Output(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tile := maptile.Tile{Z: 2, X: 3, Y: 1}
	if err := out.save(Tile{T: tile, C: []byte("tile")}, PNG); err != nil {
		t.Fatal(err)
	}
	if exists, err := out.exists(context.Background(), tile, PNG); err != nil || !exists {
		t.Fatalf("same-layout HEAD failed: %v", err)
	}
	changed := *out
	changed.tilePath = defaultTilePath
	if _, err := changed.exists(context.Background(), maptile.Tile{Z: 2, X: 1, Y: 3}, PNG); err == nil {
		t.Fatal("transposed path collision was mistaken for the requested tile")
	}
	mu.Lock()
	unmarked = true
	mu.Unlock()
	if _, err := out.exists(context.Background(), tile, PNG); err == nil {
		t.Fatal("custom layout accepted an unmarked tile without dataset metadata")
	}
	if exists, err := changed.exists(context.Background(), maptile.Tile{Z: 2, X: 1, Y: 3}, PNG); err != nil || !exists {
		t.Fatalf("legacy XYZ compatibility failed: %v", err)
	}
}

func TestFileTileLayoutCannotReplaceMetadataOrFailureManifest(t *testing.T) {
	for _, pattern := range []string{"metadata.json/{z}/{x}/{y}.{format}", "failures.json/{z}/{x}/{y}.{format}"} {
		t.Run(pattern, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.name", "layout")
			viper.Set("output.tile_path", pattern)
			task := layoutTask(t, "https://example.com/{z}/{x}/{y}.png")
			if err := task.Download(); err == nil {
				t.Fatal("accepted reserved output path conflict")
			}
			if _, err := os.Stat(filepath.Join(viper.GetString("output.directory"), "layout")); !os.IsNotExist(err) {
				t.Fatal(fmt.Sprintf("output mutated before validation: %v", err))
			}
		})
	}
}

func TestAppendKeepsFailuresFromEarlierRegion(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", kind)
			viper.Set("output.name", "regions")
			viper.Set("output.append", true)
			body := tileDataPNG(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/2/3/1.png" {
					http.Error(w, "region A unavailable", http.StatusServiceUnavailable)
					return
				}
				w.Write(body)
			}))
			defer server.Close()
			source := server.URL + "/{z}/{x}/{y}.png"
			a := layoutTask(t, source)
			if err := a.Download(); err == nil {
				t.Fatal("expected failed region A")
			}
			b := NewTask([]Layer{{Zoom: 2, URL: source, Collection: orb.Collection{orb.Point{-90, -45}}}}, a.TileMap)
			if err := b.Download(); err != nil {
				t.Fatal(err)
			}
			report := a.File + ".failures.json"
			if kind == "file" {
				report = filepath.Join(a.File, "failures.json")
			}
			manifest, err := LoadFailureManifest(report)
			if err != nil {
				t.Fatal(err)
			}
			tiles := manifest.Snapshot().Tiles
			if len(tiles) != 1 || tiles[0].Z != 2 || tiles[0].X != 3 || tiles[0].Y != 1 {
				t.Fatalf("appending B discarded A's unresolved failure: %+v", tiles)
			}
		})
	}
}
