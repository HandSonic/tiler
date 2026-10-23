package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paulmach/orb/maptile"
)

func TestCompactConvertRoundTripAndAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	makePNG := func(c color.Color) []byte {
		picture := image.NewRGBA(image.Rect(0, 0, 256, 256))
		draw.Draw(picture, picture.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
		var out bytes.Buffer
		if err := png.Encode(&out, picture); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	red := makePNG(color.RGBA{R: 255, A: 255})
	blue := makePNG(color.RGBA{B: 255, A: 255})
	green := makePNG(color.RGBA{G: 255, A: 255})
	regionA := filepath.Join(root, "region-a")
	// Distinct rows/columns on both sides of a bundle boundary expose axis
	// reversal and errors in selecting the bundle during a conversion.
	archiveTestWrite(t, filepath.Join(regionA, "8", "127", "129.png"), red)
	archiveTestWrite(t, filepath.Join(regionA, "8", "128", "130.png"), blue)
	archiveTestWrite(t, filepath.Join(regionA, "7", "0", "0.png"), blue)
	customJSON := `{"nested":true,"revision":9007199254740993}`
	archiveTestWrite(t, filepath.Join(regionA, "metadata.json"), []byte(`{"json":{"json":`+customJSON+`,"custom_id":9007199254740993,"id":9007199254740993,"basename":false,"custom_bool":true}}`))
	mbtiles := filepath.Join(root, "region-a.mbtiles")
	if _, err := ConvertArchive(ctx, regionA, mbtiles, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	compact := filepath.Join(root, "compact")
	if result, err := ConvertArchive(ctx, mbtiles, compact, ArchiveConvertOptions{OutputKind: "compactv2"}); err != nil || result.Tiles != 3 {
		t.Fatalf("MBTiles to Compact v2: result=%+v err=%v", result, err)
	}
	untouched := []string{compactPath(compact, 7, 0, 0), compactPath(compact, 8, 127, 129)}
	previousFiles := make([]os.FileInfo, len(untouched))
	for i, path := range untouched {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		previousFiles[i] = info
	}
	// On Unix, a writable parent permits replacing a read-only bundle. Its
	// staged copy must stay writable until all incoming tiles have been merged.
	readOnlyBundle := compactPath(compact, 8, 128, 130)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(readOnlyBundle, 0444); err != nil {
			t.Fatal(err)
		}
	}
	// A partial download's repair report stays in place, with bytes and
	// destination binding intact, while affected bundles are appended.
	report := filepath.Join(compact, "failures.json")
	manifest := newFailureManifest("world", PNG)
	if err := manifest.SetTarget(FailureTarget{Kind: "compactv2", Path: compact}); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Record(maptile.Tile{Z: 8, X: 130, Y: 132}, "https://tiles.example/{z}/{x}/{y}", errors.New("missing tile")); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(report); err != nil {
		t.Fatal(err)
	}
	originalReport, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	regionB := filepath.Join(root, "region-b")
	archiveTestWrite(t, filepath.Join(regionB, "8", "127", "129.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "8", "129", "131.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "9", "130", "132.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "metadata.json"), []byte(`{"bounds":[0,0,1,1],"custom_bool":"incoming"}`))
	// Readers accept external level aliases, but modifying one in place must
	// fail before a second, canonical bundle can duplicate its coordinates.
	canonicalLevel := filepath.Join(compact, "_alllayers", "L08")
	aliasLevel := filepath.Join(compact, "_alllayers", "L8")
	if err := os.Rename(canonicalLevel, aliasLevel); err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := os.ReadFile(filepath.Join(compact, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("append accepted an external bundle path alias: %v", err)
	}
	previousMetadata, err := compactReadMetadata(compact)
	if err != nil {
		t.Fatal(err)
	}
	if cache, err := compactOpen(ctx, compact, previousMetadata, true); err == nil {
		cache.Close()
		t.Fatal("direct download resume accepted an external bundle path alias")
	}
	if afterMetadata, err := os.ReadFile(filepath.Join(compact, "metadata.json")); err != nil || !bytes.Equal(afterMetadata, beforeMetadata) {
		t.Fatalf("rejected append changed metadata: %v", err)
	}
	if tiles := archiveV2Snapshot(t, compact, ""); len(tiles) != 3 || !bytes.Equal(tiles["8/128/130"], blue) {
		t.Fatal("rejected append changed old tiles or prevented reading an external cache")
	}
	if err := os.Rename(aliasLevel, canonicalLevel); err != nil {
		t.Fatal(err)
	}
	// An existing conf.xml identifies the destination without -output-type.
	if result, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true, Metadata: map[string]string{"name": "combined"}}); err != nil || result.Tiles != 2 || !result.Incremental {
		t.Fatalf("Compact v2 append: result=%+v err=%v", result, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(readOnlyBundle); err != nil || info.Mode().Perm() != 0444 {
			t.Fatalf("append changed the existing bundle permissions: %v: %v", info, err)
		}
	}
	for i, path := range untouched {
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(previousFiles[i], info) || !previousFiles[i].ModTime().Equal(info.ModTime()) {
			t.Fatalf("append rewrote an unrelated or overlap-only bundle %s: %v", path, err)
		}
	}
	merged, err := compactReadMetadata(compact)
	if err != nil || merged["minzoom"] != "7" || merged["maxzoom"] != "9" || merged["name"] != "combined" {
		t.Fatalf("append lost old coverage or explicit metadata: %v: %v", merged, err)
	}
	bounds, err := downloadMetadataBounds(merged["bounds"])
	if err != nil || bounds[2] < float64(130)/256*360-180 {
		t.Fatalf("declared input bounds hid an appended tile: %v: %v", bounds, err)
	}
	if result, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err != nil || result.Tiles != 0 || !result.Incremental {
		t.Fatalf("retry did not skip completed append tiles: result=%+v err=%v", result, err)
	}
	if preserved, err := os.ReadFile(report); err != nil || !bytes.Equal(preserved, originalReport) {
		t.Fatalf("append lost or changed the original repair report: %v", err)
	}
	expected := map[string][]byte{"7/0/0": blue, "8/127/129": red, "8/128/130": blue, "8/129/131": green, "9/130/132": green}
	checkTiles := func(path string) {
		t.Helper()
		actual := archiveV2Snapshot(t, path, "")
		if len(actual) != len(expected) {
			t.Fatalf("%s: got %d tiles, want %d", path, len(actual), len(expected))
		}
		for key, want := range expected {
			if !bytes.Equal(actual[key], want) {
				t.Fatalf("%s: tile %s changed", path, key)
			}
		}
	}
	checkTiles(compact)
	if _, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true, OutputKind: "file"}); err == nil {
		t.Fatal("loose tile append accepted a Compact v2 destination")
	}

	// A size unsupported by Compact v2 fails during export, after SQLite
	// staging, and must leave the published directory intact.
	invalid := filepath.Join(root, "wrong-size")
	archiveTestWrite(t, filepath.Join(invalid, "8", "130", "132.png"), archiveTestPNG(t, color.White))
	if _, err := ConvertArchive(ctx, invalid, compact, ArchiveConvertOptions{Append: true}); err == nil {
		t.Fatal("Compact v2 accepted a 2x2 tile")
	}
	checkTiles(compact)

	large := image.NewRGBA(image.Rect(0, 0, 512, 512))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, large); err != nil {
		t.Fatal(err)
	}
	archiveTestWrite(t, filepath.Join(invalid, "8", "130", "132.png"), encoded.Bytes())
	if _, err := ConvertArchive(ctx, invalid, compact, ArchiveConvertOptions{Append: true}); err == nil {
		t.Fatal("append changed the established Compact tile dimensions")
	}
	checkTiles(compact)
	// A cancelled initial download can leave metadata but no bundle. Its
	// default 256-pixel config must still permit the first actual 512 tile.
	empty := filepath.Join(root, "initialized")
	cache, err := compactOpen(ctx, empty, merged, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(empty, "_alllayers", "L08"), 0755); err != nil {
		t.Fatal(err)
	}
	if result, err := ConvertArchive(ctx, invalid, empty, ArchiveConvertOptions{Append: true}); err != nil || result.Tiles != 1 {
		t.Fatalf("append into initialized cache: %+v: %v", result, err)
	}
	if metadata, err := compactReadMetadata(empty); err != nil || metadata["pixel_scale"] != "512" {
		t.Fatalf("initial tile dimensions were not established: %v: %v", metadata, err)
	}

	archiveTestWrite(t, report, []byte("invalid repair report"))
	if _, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err == nil {
		t.Fatal("append discarded an unreadable repair report")
	}
	if preserved, err := os.ReadFile(report); err != nil || string(preserved) != "invalid repair report" {
		t.Fatalf("failed append changed the original repair report: %v", err)
	}
	checkTiles(compact)
	archiveTestWrite(t, report, originalReport)
	if err := os.Remove(report); err != nil {
		t.Fatal(err)
	}
	if result, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err != nil || result.Tiles != 0 {
		t.Fatalf("append without an optional repair report: %+v: %v", result, err)
	}

	pmtiles := filepath.Join(root, "combined.pmtiles")
	if _, err := ConvertArchive(ctx, compact, pmtiles, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	fromPM := filepath.Join(root, "from-pm")
	if _, err := ConvertArchive(ctx, pmtiles, fromPM, ArchiveConvertOptions{OutputKind: "compactv2"}); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(root, "zyx-files")
	if _, err := ConvertArchive(ctx, fromPM, files, ArchiveConvertOptions{OutputTilePath: "{z}/{y}/{x}.{format}"}); err != nil {
		t.Fatal(err)
	}
	checkTiles(files)
	// The reserved container name can also be a custom PMTiles JSON key.
	// Preserve that key and integer precision through the complete roundtrip.
	metadata, err := archiveReadDirectoryMetadata(files)
	if err != nil {
		t.Fatal(err)
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata["json"]), &extensions); err != nil {
		t.Fatal(err)
	}
	if string(extensions["json"]) != customJSON || string(extensions["custom_id"]) != "9007199254740993" || string(extensions["id"]) != "9007199254740993" || string(extensions["basename"]) != "false" {
		t.Fatalf("Compact roundtrip changed JSON extensions: %s", metadata["json"])
	}
	if string(extensions["custom_bool"]) != "true" {
		t.Fatalf("append let an incoming string replace old typed metadata: %v", metadata)
	}
}
