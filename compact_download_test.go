package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
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

// Exercise the complete lifecycle against actual HTTP responses and bundle
// files. The coordinates cross both 128-tile bundle boundaries.
func TestCompactDownloadCancelRepairResumeAppend(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("output.format", "compactv2")
	viper.Set("output.name", "world")
	viper.Set("output.resume", true)
	viper.Set("output.failure_file", filepath.Join(viper.GetString("output.directory"), "world", "_alllayers", "L00", "errors.json"))
	viper.Set("task.workers", 1)
	viper.Set("task.timeout_seconds", 10)
	coordinates := []maptile.Tile{
		{Z: 8, X: 127, Y: 12},
		{Z: 8, X: 128, Y: 13},
		{Z: 8, X: 129, Y: 140},
	}
	bodies := make(map[maptile.Tile][]byte)
	responses := make(map[string][]byte)
	for i, tile := range coordinates {
		picture := image.NewRGBA(image.Rect(0, 0, 512, 512))
		picture.Set(0, 0, color.RGBA{R: uint8(i + 1), A: 255})
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, picture); err != nil {
			t.Fatal(err)
		}
		bodies[tile] = encoded.Bytes()
		responses[fmt.Sprintf("/%d/%d/%d", tile.Z, tile.X, tile.Y)] = encoded.Bytes()
	}
	var available atomic.Bool
	var failFirst atomic.Bool
	failFirst.Store(true)
	var requests atomic.Int32
	blocked := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, ok := responses[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if failFirst.Swap(false) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/8/128/13" && !available.Load() {
			close(blocked)
			<-r.Context().Done()
			return
		}
		w.Write(body)
	}))
	defer source.Close()
	template := source.URL + "/{z}/{x}/{y}"
	tileMap := TileMap{Name: "compact-world", URL: template, Format: PNG}
	layers := func(tiles ...maptile.Tile) []Layer {
		points := make(orb.MultiPoint, len(tiles))
		for i, tile := range tiles {
			points[i] = tile.Bound().Center()
		}
		return []Layer{{Zoom: 8, URL: template, Collection: orb.Collection{points}}}
	}
	// The initial failure leaves metadata and a custom report beneath a level,
	// but no bundle. That report must not lock the first actual tile to 256px.
	initial := NewTask(layers(coordinates[0]), tileMap)
	t.Cleanup(initial.Cancel)
	if err := initial.Download(); err == nil || initial.Stats().Saved != 0 {
		t.Fatalf("initial failure should not save a tile: stats=%+v err=%v", initial.Stats(), err)
	}
	if report, err := LoadFailureManifest(initial.failurePath); err != nil || report.Len() != 1 {
		t.Fatalf("initial failure did not leave its nested report: %v", err)
	}
	first := NewTask(layers(coordinates[:2]...), tileMap)
	t.Cleanup(first.Cancel)
	finished := make(chan error, 1)
	go func() { finished <- first.Download() }()
	select {
	case <-blocked:
		first.Cancel()
	case err := <-finished:
		t.Fatalf("download finished before the second tile: %v", err)
	case <-time.After(3 * time.Second):
		first.Cancel()
		t.Fatal("second tile request did not start")
	}
	if err := awaitRegressionError(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled download error = %v", err)
	}
	if first.Stats() != (DownloadStats{Saved: 1, Failed: 1}) {
		t.Fatalf("partial download stats = %+v", first.Stats())
	}
	before, err := compactReadMetadata(first.File)
	if err != nil {
		t.Fatalf("cancellation left unreadable metadata: %v", err)
	}
	manifest, err := LoadFailureManifest(first.failurePath)
	if err != nil || manifest.Len() != 1 {
		t.Fatalf("cancellation failure manifest: %v", err)
	}
	if target, ok := manifest.Target(); !ok || target.Kind != "compactv2" || target.Path != first.File {
		t.Fatalf("wrong repair destination: %+v", target)
	}

	// Repair uses the original target and its metadata even if the selected
	// geometry and current output name cover only the failed tile.
	available.Store(true)
	viper.Set("output.name", "different-name")
	repair, err := NewRepairTask(layers(coordinates[1]), tileMap, manifest, first.failurePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repair.Cancel)
	if err := repair.Download(); err != nil {
		t.Fatal(err)
	}
	if repair.File != first.File || repair.Stats() != (DownloadStats{Saved: 1}) || requests.Load() != 4 {
		t.Fatalf("repair changed target or downloaded extra tiles: %s, %+v, requests=%d", repair.File, repair.Stats(), requests.Load())
	}
	after, err := compactReadMetadata(first.File)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"bounds", "minzoom", "maxzoom"} {
		if after[key] != before[key] {
			t.Fatalf("repair changed %s: %q -> %q", key, before[key], after[key])
		}
	}
	manifest, err = LoadFailureManifest(first.failurePath)
	if err != nil || manifest.Len() != 0 {
		t.Fatalf("repair did not clear failure manifest: %v", err)
	}

	viper.Set("output.name", "world")
	viper.Set("output.resume", true)
	resumed := NewTask(layers(coordinates[:2]...), tileMap)
	t.Cleanup(resumed.Cancel)
	// Stored tiles must skip scheduling and its network rate limit entirely.
	// Occupying the only worker slot proves this without timing assertions.
	resumed.workers <- coordinates[0]
	resumedDone := make(chan error, 1)
	go func() { resumedDone <- resumed.Download() }()
	if err := awaitRegressionError(t, resumedDone); err != nil {
		t.Fatal(err)
	}
	<-resumed.workers
	if resumed.Stats() != (DownloadStats{Skipped: 2}) || requests.Load() != 4 {
		t.Fatalf("resume requested stored tiles: %+v, requests=%d", resumed.Stats(), requests.Load())
	}
	viper.Set("output.append", true)
	appended := NewTask(layers(coordinates[0], coordinates[2]), tileMap)
	t.Cleanup(appended.Cancel)
	if err := appended.Download(); err != nil {
		t.Fatal(err)
	}
	if appended.File != first.File || appended.Stats() != (DownloadStats{Saved: 1, Skipped: 1}) || requests.Load() != 5 {
		t.Fatalf("append did not preserve existing tiles: %+v, requests=%d", appended.Stats(), requests.Load())
	}
	seen := make(map[maptile.Tile]bool)
	metadata, err := archiveWalkCompact(context.Background(), first.File, func(z int, x, y uint32, body []byte, format string) error {
		tile := maptile.Tile{Z: maptile.Zoom(z), X: x, Y: y}
		if !bytes.Equal(body, bodies[tile]) || format != PNG || seen[tile] {
			t.Fatalf("wrong or duplicate stored tile %v", tile)
		}
		seen[tile] = true
		return nil
	})
	if err != nil || len(seen) != len(coordinates) {
		t.Fatalf("read complete cache: tiles=%d, error=%v", len(seen), err)
	}
	bounds, err := downloadMetadataBounds(metadata["bounds"])
	if err != nil {
		t.Fatal(err)
	}
	for _, tile := range coordinates {
		point := tile.Bound().Center()
		if point[0] < bounds[0] || point[0] > bounds[2] || point[1] < bounds[1] || point[1] > bounds[3] {
			t.Fatalf("merged bounds exclude %v: %s", tile, metadata["bounds"])
		}
	}
	bundles := 0
	if err := filepath.WalkDir(first.File, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch filepath.Ext(path) {
		case ".bundle":
			bundles++
		case ".png", ".jpg", ".jpeg", ".bundlx":
			t.Errorf("unexpected loose tile or v1 index: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if bundles != 3 {
		t.Fatalf("bundle count = %d, want 3", bundles)
	}

	// Rejected repair/output changes must leave the original cache intact.
	originalMetadata, err := os.ReadFile(filepath.Join(first.File, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A stale report must not resolve coordinates from a cache whose image
	// encoding no longer matches the original download.
	wrongFormat := newFailureManifest(tileMap.Name, JPG)
	if err := wrongFormat.SetTarget(FailureTarget{Kind: "compactv2", Path: first.File}); err != nil {
		t.Fatal(err)
	}
	if err := wrongFormat.Record(coordinates[0], template, errors.New("stale JPEG failure")); err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(filepath.Dir(first.File), "wrong-format-failures.json")
	if err := wrongFormat.Save(wrongPath); err != nil {
		t.Fatal(err)
	}
	originalReport, err := os.ReadFile(wrongPath)
	if err != nil {
		t.Fatal(err)
	}
	wrongMap := tileMap
	wrongMap.Format = JPG
	mismatched, err := NewRepairTask(layers(coordinates[0]), wrongMap, wrongFormat, wrongPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mismatched.Cancel)
	if err := mismatched.Download(); err == nil || !strings.Contains(err.Error(), "tile format differs") {
		t.Fatalf("repair accepted a cache with a different encoding: %v", err)
	}
	unchangedReport, err := os.ReadFile(wrongPath)
	if err != nil || !bytes.Equal(unchangedReport, originalReport) || wrongFormat.Len() != 1 || requests.Load() != 5 {
		t.Fatalf("rejected repair modified its report or requested tiles: %v", err)
	}
	unchangedMetadata, err := os.ReadFile(filepath.Join(first.File, "metadata.json"))
	if err != nil || !bytes.Equal(unchangedMetadata, originalMetadata) {
		t.Fatalf("rejected repair modified cache metadata: %v", err)
	}
	// Changing output mode must not mix loose tiles into an existing cache,
	// even if a separate failure report avoids the old target-kind check.
	viper.Set("output.format", "file")
	viper.Set("output.append", false)
	viper.Set("output.resume", false)
	viper.Set("output.failure_file", filepath.Join(filepath.Dir(first.File), "loose-failures.json"))
	loose := NewTask(layers(coordinates[0]), tileMap)
	t.Cleanup(loose.Cancel)
	if err := loose.Download(); err == nil {
		t.Fatal("loose download accepted an existing Compact v2 cache")
	}
	unchanged, err := os.ReadFile(filepath.Join(first.File, "metadata.json"))
	if err != nil || !bytes.Equal(unchanged, originalMetadata) || requests.Load() != 5 {
		t.Fatalf("rejected output change modified cache metadata or requested tiles: %v", err)
	}
}
