package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
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
	mbtiles := filepath.Join(root, "region-a.mbtiles")
	if _, err := ConvertArchive(ctx, regionA, mbtiles, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	compact := filepath.Join(root, "compact")
	if result, err := ConvertArchive(ctx, mbtiles, compact, ArchiveConvertOptions{OutputKind: "compactv2"}); err != nil || result.Tiles != 2 {
		t.Fatalf("MBTiles to Compact v2: result=%+v err=%v", result, err)
	}
	// A partial download's repair report must survive replacing the cache
	// directory during conversion append, with bytes and destination intact.
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
	// An existing conf.xml identifies the destination without -output-type.
	if result, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err != nil || result.Tiles != 3 {
		t.Fatalf("Compact v2 append: result=%+v err=%v", result, err)
	}
	if preserved, err := os.ReadFile(report); err != nil || !bytes.Equal(preserved, originalReport) {
		t.Fatalf("append lost or changed the original repair report: %v", err)
	}
	expected := map[string][]byte{"8/127/129": red, "8/128/130": blue, "8/129/131": green}
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

	archiveTestWrite(t, report, []byte("invalid repair report"))
	if _, err := ConvertArchive(ctx, regionB, compact, ArchiveConvertOptions{Append: true}); err == nil {
		t.Fatal("append discarded an unreadable repair report")
	}
	if preserved, err := os.ReadFile(report); err != nil || string(preserved) != "invalid repair report" {
		t.Fatalf("failed append changed the original repair report: %v", err)
	}
	checkTiles(compact)
	archiveTestWrite(t, report, originalReport)

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
}
