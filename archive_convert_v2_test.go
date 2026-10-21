package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pm "tiler/internal/pmtiles"
)

func archiveV2Snapshot(t *testing.T, path, pattern string) map[string][]byte {
	t.Helper()
	tiles := map[string][]byte{}
	_, err := archiveWalkLocal(context.Background(), path, pattern, func(z int, x, y uint32, body []byte, format string) error {
		if format == PBF {
			var err error
			body, err = prepareTileData(body, format, false)
			if err != nil {
				return err
			}
		}
		key := fmt.Sprintf("%d/%d/%d", z, x, y)
		if _, ok := tiles[key]; ok {
			return fmt.Errorf("duplicate %s", key)
		}
		tiles[key] = append([]byte(nil), body...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tiles
}

func TestArchiveV2LocalRoundTripMatrixAndCustomPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	xyz := filepath.Join(root, "xyz")
	red := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
	blue := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	archiveTestWrite(t, filepath.Join(xyz, "2", "1", "3.png"), red)
	archiveTestWrite(t, filepath.Join(xyz, "1", "1", "0.png"), blue)
	expected := map[string][]byte{"2/1/3": red, "1/1/0": blue}
	sources := map[string]string{"file": xyz}
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		path := filepath.Join(root, "source."+kind)
		if _, err := convertLocalArchive(ctx, xyz, path, ArchiveConvertOptions{}); err != nil {
			t.Fatal(err)
		}
		sources[kind] = path
	}
	for sourceKind, source := range sources {
		for _, targetKind := range []string{"mbtiles", "pmtiles", "file"} {
			t.Run(sourceKind+"-to-"+targetKind, func(t *testing.T) {
				destination := filepath.Join(t.TempDir(), "result."+targetKind)
				pattern := "tiles/{z}/{y}/{x}.{format}"
				result, err := convertLocalArchive(ctx, source, destination, ArchiveConvertOptions{OutputKind: targetKind, OutputTilePath: pattern})
				if err != nil {
					t.Fatal(err)
				}
				if result.Tiles != 2 || result.Format != PNG {
					t.Fatalf("result=%+v", result)
				}
				actual := archiveV2Snapshot(t, destination, "")
				if len(actual) != len(expected) {
					t.Fatalf("tiles=%v", actual)
				}
				for key, want := range expected {
					if !bytes.Equal(actual[key], want) {
						t.Fatalf("tile %s changed", key)
					}
				}
				if targetKind == "file" {
					if _, err := os.Stat(filepath.Join(destination, "tiles", "2", "3", "1.png")); err != nil {
						t.Fatal(err)
					}
					metadata, err := archiveReadDirectoryMetadata(destination)
					if err != nil || metadata["tile_path"] != pattern {
						t.Fatalf("metadata=%v err=%v", metadata, err)
					}
				} else {
					dbPath := destination
					if targetKind == "pmtiles" {
						dbPath = filepath.Join(t.TempDir(), "back.mbtiles")
						if _, err := convertLocalArchive(ctx, destination, dbPath, ArchiveConvertOptions{}); err != nil {
							t.Fatal(err)
						}
					}
					db, err := archiveOpenSQLite(dbPath, true)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					var n int
					if err := db.QueryRow("SELECT count(*) FROM metadata WHERE name='tile_path'").Scan(&n); err != nil || n != 0 {
						t.Fatalf("path metadata leaked into archive: %d %v", n, err)
					}
				}
			})
		}
	}
	for _, pattern := range []string{"{z}/{x}/{-y}", "{z}_{y}_{x}.tile"} {
		output := filepath.Join(t.TempDir(), "custom")
		if _, err := convertLocalArchive(ctx, sources["pmtiles"], output, ArchiveConvertOptions{OutputTilePath: pattern}); err != nil {
			t.Fatal(err)
		}
		actual := archiveV2Snapshot(t, output, "")
		for key, want := range expected {
			if !bytes.Equal(actual[key], want) {
				t.Fatalf("%s tile %s changed", pattern, key)
			}
		}
	}
}

func TestArchiveV2AppendPreservesRegionAndUnionsMetadata(t *testing.T) {
	ctx := context.Background()
	red := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
	blue := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	green := archiveTestPNG(t, color.RGBA{G: 255, A: 255})
	regionA := t.TempDir()
	regionB := t.TempDir()
	archiveTestWrite(t, filepath.Join(regionA, "1", "0", "0.png"), red)
	archiveTestWrite(t, filepath.Join(regionA, "metadata.json"), []byte(`{"name":"all-regions","bounds":"-170,5,-10,80","json":{"bounds":[-170,5,-10,80],"center":[-90,45,1],"minzoom":1,"maxzoom":1,"custom":"keep","custom_id":9007199254740993,"nested":{"id":9007199254740993}}}`))
	archiveTestWrite(t, filepath.Join(regionB, "1", "0", "0.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "2", "3", "3.png"), blue)
	archiveTestWrite(t, filepath.Join(regionB, "metadata.json"), []byte(`{"name":"region-b","bounds":"100,-80,170,-5"}`))
	for _, kind := range []string{"mbtiles", "pmtiles", "file"} {
		t.Run(kind, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "combined."+kind)
			pattern := "{z}/{y}/{x}.{format}"
			if _, err := convertLocalArchive(ctx, regionA, output, ArchiveConvertOptions{OutputKind: kind, OutputTilePath: pattern}); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				archiveTestWrite(t, filepath.Join(output, "sentinel.txt"), []byte("preserve"))
			}
			result, err := convertLocalArchive(ctx, regionB, output, ArchiveConvertOptions{OutputKind: kind, Append: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Tiles != 2 {
				t.Fatalf("tiles=%d", result.Tiles)
			}
			actual := archiveV2Snapshot(t, output, "")
			if len(actual) != 2 || !bytes.Equal(actual["1/0/0"], red) || !bytes.Equal(actual["2/3/3"], blue) {
				t.Fatalf("append lost or replaced a region: %v", actual)
			}
			metadata, err := archiveWalkLocal(ctx, output, "", func(int, uint32, uint32, []byte, string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if metadata["minzoom"] != "1" || metadata["maxzoom"] != "2" || metadata["name"] != "all-regions" {
				t.Fatalf("metadata=%v", metadata)
			}
			bounds, err := archiveNumbers(metadata["bounds"], 4)
			if err != nil || bounds[0] > -170 || bounds[1] > -80 || bounds[2] < 170 || bounds[3] < 80 {
				t.Fatalf("bounds shrank: %v %v", bounds, err)
			}
			var tileJSON map[string]interface{}
			if kind == "pmtiles" {
				// Read the actual JSON: archiveWalkPM derives coverage from the
				// header and would mask stale TileJSON coverage fields.
				_, _, tileJSON = archiveTestReadPM(t, output)
			} else if err := json.Unmarshal([]byte(metadata["json"]), &tileJSON); err != nil {
				t.Fatal(err)
			}
			jsonBounds, _ := tileJSON["bounds"].([]interface{})
			jsonCenter, _ := tileJSON["center"].([]interface{})
			if _, hasBounds := tileJSON["bounds"]; hasBounds && (len(jsonBounds) != 4 || jsonBounds[0].(float64) > -170 || jsonBounds[1].(float64) > -80 || jsonBounds[2].(float64) < 170 || jsonBounds[3].(float64) < 80) {
				t.Errorf("TileJSON coverage stayed at region A: %v", tileJSON)
			}
			for key, want := range map[string]float64{"minzoom": 1, "maxzoom": 2} {
				if value, exists := tileJSON[key]; exists && value != want {
					t.Errorf("TileJSON %s stayed at region A: %v", key, tileJSON)
				}
			}
			if _, hasCenter := tileJSON["center"]; hasCenter && (len(jsonCenter) != 3 || jsonCenter[0] != float64(0) || jsonCenter[1] != float64(0) || jsonCenter[2] != float64(1)) {
				t.Errorf("TileJSON center or custom field changed incorrectly: %v", tileJSON)
			}
			if tileJSON["custom"] != "keep" || (kind != "pmtiles" && (len(jsonBounds) == 0 || len(jsonCenter) == 0)) {
				t.Errorf("TileJSON fields were lost: %v", tileJSON)
			}
			if kind == "pmtiles" {
				if metadata["custom_id"] != "9007199254740993" || !strings.Contains(metadata["nested"], "9007199254740993") {
					t.Errorf("PMTiles custom integers rounded: %v", metadata)
				}
			} else {
				var raw map[string]json.RawMessage
				if err := json.Unmarshal([]byte(metadata["json"]), &raw); err != nil || string(raw["custom_id"]) != "9007199254740993" || string(raw["nested"]) != `{"id":9007199254740993}` {
					t.Errorf("custom integers rounded: %s, %v", metadata["json"], err)
				}
			}
			if kind == "file" {
				body, err := os.ReadFile(filepath.Join(output, "sentinel.txt"))
				if err != nil || string(body) != "preserve" {
					t.Fatalf("sentinel changed: %s %v", body, err)
				}
			}
		})
	}
}

func TestArchiveV2AppendFailureLeavesOriginalArchiveUntouched(t *testing.T) {
	ctx := context.Background()
	region := t.TempDir()
	archiveTestWrite(t, filepath.Join(region, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	broken := t.TempDir()
	archiveTestWrite(t, filepath.Join(broken, "1", "1", "1.png"), []byte("invalid image"))
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		dir := t.TempDir()
		output := filepath.Join(dir, "map."+kind)
		if _, err := convertLocalArchive(ctx, region, output, ArchiveConvertOptions{}); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(output)
		if _, err := convertLocalArchive(ctx, broken, output, ArchiveConvertOptions{Append: true}); err == nil {
			t.Fatal("broken append accepted")
		}
		after, _ := os.ReadFile(output)
		if !bytes.Equal(before, after) {
			t.Fatal("failed append changed original archive")
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("temporary artifacts remain: %v", entries)
		}
	}
}

func TestArchiveV2FileOverwriteAndAppendPathProtection(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	b := t.TempDir()
	archiveTestWrite(t, filepath.Join(a, "1", "0", "0.png"), archiveTestPNG(t, color.White))
	archiveTestWrite(t, filepath.Join(b, "1", "1", "1.png"), archiveTestPNG(t, color.Black))
	output := filepath.Join(t.TempDir(), "tiles")
	if _, err := convertLocalArchive(ctx, a, output, ArchiveConvertOptions{OutputTilePath: "{z}/{y}/{x}.{format}"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(output, "metadata.json"))
	if _, err := convertLocalArchive(ctx, b, output, ArchiveConvertOptions{Append: true, OutputTilePath: "{z}/{x}/{y}.{format}"}); err == nil {
		t.Fatal("append path change accepted")
	}
	after, _ := os.ReadFile(filepath.Join(output, "metadata.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("rejected append changed metadata")
	}
	if _, err := convertLocalArchive(ctx, b, output, ArchiveConvertOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	actual := archiveV2Snapshot(t, output, "")
	if len(actual) != 1 || actual["1/1/1"] == nil {
		t.Fatalf("overwrite did not replace old directory: %v", actual)
	}
	for _, opts := range []ArchiveConvertOptions{{Append: true, Overwrite: true}, {OutputTilePath: "metadata.json/{z}/{x}/{y}.{format}"}} {
		if _, err := convertLocalArchive(ctx, a, filepath.Join(t.TempDir(), "new"), opts); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
}

func archiveV2WritePM(t *testing.T, path string, entries []pm.EntryV3, body []byte, compression pm.Compression, headerMutator func(*pm.HeaderV3)) {
	t.Helper()
	root := pm.SerializeEntries(entries)
	metadata := []byte(`{"name":"external"}`)
	if compression == pm.NoCompression {
		reader, err := gzip.NewReader(bytes.NewReader(root))
		if err != nil {
			t.Fatal(err)
		}
		root, err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var err error
		metadata, err = archiveGzip(metadata)
		if err != nil {
			t.Fatal(err)
		}
	}
	offset := uint64(pm.HeaderV3LenBytes + len(root))
	header := pm.HeaderV3{SpecVersion: 3, RootOffset: 127, RootLength: uint64(len(root)), MetadataOffset: offset, MetadataLength: uint64(len(metadata)), TileDataOffset: offset + uint64(len(metadata)), TileDataLength: uint64(len(body)), AddressedTilesCount: 2, TileEntriesCount: uint64(len(entries)), TileContentsCount: 1, InternalCompression: compression, TileCompression: pm.NoCompression, TileType: pm.Png, MinZoom: 1, MaxZoom: 1, MinLonE7: -1800000000, MinLatE7: -850511288, MaxLonE7: 1800000000, MaxLatE7: 850511288, CenterZoom: 1}
	if headerMutator != nil {
		headerMutator(&header)
	}
	archiveTestWrite(t, path, append(append(append(pm.SerializeHeader(header), root...), metadata...), body...))
}

func TestArchiveV2ReadsOfficialPMRunLengthAndCompression(t *testing.T) {
	ctx := context.Background()
	body := archiveTestPNG(t, color.White)
	for _, compression := range []pm.Compression{pm.NoCompression, pm.Gzip} {
		source := filepath.Join(t.TempDir(), "external.pmtiles")
		archiveV2WritePM(t, source, []pm.EntryV3{{TileId: 1, Offset: 0, Length: uint32(len(body)), RunLength: 2}}, body, compression, nil)
		output := filepath.Join(t.TempDir(), "out.mbtiles")
		result, err := convertLocalArchive(ctx, source, output, ArchiveConvertOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Tiles != 2 {
			t.Fatalf("run length not expanded: %+v", result)
		}
		actual := archiveV2Snapshot(t, output, "")
		for id := uint64(1); id < 3; id++ {
			z, x, y := pm.IdToZxy(id)
			if !bytes.Equal(actual[fmt.Sprintf("%d/%d/%d", z, x, y)], body) {
				t.Fatal("run tile changed")
			}
		}
	}
	source := filepath.Join(t.TempDir(), "gzip-raster.pmtiles")
	packed, err := archiveGzip(body)
	if err != nil {
		t.Fatal(err)
	}
	archiveV2WritePM(t, source, []pm.EntryV3{{TileId: 1, Offset: 0, Length: uint32(len(packed)), RunLength: 2}}, packed, pm.Gzip, func(h *pm.HeaderV3) { h.TileCompression = pm.Gzip })
	if _, err := convertLocalArchive(ctx, source, filepath.Join(t.TempDir(), "decoded.mbtiles"), ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveV2RejectsMalformedPMWithoutPublishing(t *testing.T) {
	body := archiveTestPNG(t, color.White)
	mutations := []func(*pm.HeaderV3){
		func(h *pm.HeaderV3) { h.TileDataOffset = 1 << 63 },
		func(h *pm.HeaderV3) { h.TileDataLength++ },
		func(h *pm.HeaderV3) { h.InternalCompression = pm.Brotli },
		func(h *pm.HeaderV3) { h.TileCompression = pm.Zstd },
		func(h *pm.HeaderV3) { h.TileType = pm.Avif },
		func(h *pm.HeaderV3) { h.AddressedTilesCount = 3 },
	}
	for i, mutate := range mutations {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "bad.pmtiles")
			archiveV2WritePM(t, source, []pm.EntryV3{{TileId: 1, Offset: 0, Length: uint32(len(body)), RunLength: 2}}, body, pm.Gzip, mutate)
			output := filepath.Join(t.TempDir(), "out.mbtiles")
			if _, err := convertLocalArchive(context.Background(), source, output, ArchiveConvertOptions{}); err == nil {
				t.Fatal("bad PMTiles accepted")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("output published: %v", err)
			}
		})
	}
}

func TestArchiveV2VectorRoundTripAndMetadataMerge(t *testing.T) {
	raw := []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.pbf"), raw)
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"vector_layers":[{"id":"test","fields":{}}]}`))
	pmtiles := filepath.Join(t.TempDir(), "vector.pmtiles")
	if _, err := convertLocalArchive(context.Background(), input, pmtiles, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "tiles")
	if _, err := convertLocalArchive(context.Background(), pmtiles, output, ArchiveConvertOptions{OutputTilePath: "{z}/{y}/{x}"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(output, "0", "0", "0"))
	if err != nil || !bytes.Equal(body, raw) {
		t.Fatalf("PBF not raw after extraction: %x %v", body, err)
	}
	rearchive := filepath.Join(t.TempDir(), "vector-again.pmtiles")
	if _, err := convertLocalArchive(context.Background(), output, rearchive, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	a := map[string]string{"format": "pbf", "json": `{"vector_layers":[{"id":"roads","fields":{"name":"String"},"minzoom":5,"maxzoom":8}]}`}
	b := map[string]string{"format": "pbf", "vector_layers": `[{"id":"roads","fields":{"class":"String"},"minzoom":2,"maxzoom":10},{"id":"water","fields":{}}]`}
	merged, err := archiveMergeMetadata(a, b)
	if err != nil {
		t.Fatal(err)
	}
	var layers []map[string]interface{}
	if err := json.Unmarshal([]byte(merged["vector_layers"]), &layers); err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 || layers[0]["minzoom"] != float64(2) || layers[0]["maxzoom"] != float64(10) || len(layers[0]["fields"].(map[string]interface{})) != 2 {
		t.Fatalf("layers=%v", layers)
	}
	b["vector_layers"] = `[{"id":"roads","fields":{"name":"Number"}}]`
	if _, err := archiveMergeMetadata(a, b); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatal("vector metadata conflict ignored")
	}
}

func TestArchiveV2DirectoryRollbackFailureKeepsRecoverableBackup(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "output")
	staged := filepath.Join(root, "work", "tiles")
	archiveTestWrite(t, filepath.Join(destination, "old.txt"), []byte("original region"))
	archiveTestWrite(t, filepath.Join(staged, "new.txt"), []byte("new region"))
	calls := 0
	err := archivePublishDirectoryWithRename(staged, destination, true, func(from, to string) error {
		calls++
		if calls == 1 {
			return os.Rename(from, to)
		}
		return fmt.Errorf("injected rename failure")
	})
	if err == nil || !strings.Contains(err.Error(), "original data retained at") {
		t.Fatalf("missing recovery location: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "work")); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(root, ".tiler-previous-*", "previous-output", "old.txt"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("original backup missing: %v %v", backups, err)
	}
	body, err := os.ReadFile(backups[0])
	if err != nil || string(body) != "original region" {
		t.Fatalf("original backup lost: %q %v", body, err)
	}
}

func TestArchiveV2AppendTypedDestinationAndWALProtection(t *testing.T) {
	ctx := context.Background()
	a := t.TempDir()
	b := t.TempDir()
	archiveTestWrite(t, filepath.Join(a, "1", "0", "0.png"), archiveTestPNG(t, color.White))
	archiveTestWrite(t, filepath.Join(b, "1", "1", "1.png"), archiveTestPNG(t, color.Black))
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		output := filepath.Join(t.TempDir(), "without-extension")
		if _, err := convertLocalArchive(ctx, a, output, ArchiveConvertOptions{OutputKind: kind}); err != nil {
			t.Fatal(err)
		}
		result, err := convertLocalArchive(ctx, b, output, ArchiveConvertOptions{OutputKind: kind, Append: true})
		if err != nil || result.Tiles != 2 {
			t.Fatalf("typed append failed: %+v %v", result, err)
		}
	}
	output := filepath.Join(t.TempDir(), "live.mbtiles")
	if _, err := convertLocalArchive(ctx, a, output, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	db, err := archiveOpenSQLite(output, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO metadata(name,value) VALUES ('wal-only','uncheckpointed')"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(output)
	if _, err := convertLocalArchive(ctx, b, output, ArchiveConvertOptions{Append: true}); err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("live WAL append accepted: %v", err)
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected append modified main DB")
	}
	var value string
	if err := db.QueryRow("SELECT value FROM metadata WHERE name='wal-only'").Scan(&value); err != nil || value != "uncheckpointed" {
		t.Fatalf("WAL data changed: %s %v", value, err)
	}
}

func TestArchiveV2RejectsAncestorDirectoryOutputAndOversizeTile(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	input := filepath.Join(parent, "source.mbtiles")
	xyz := t.TempDir()
	archiveTestWrite(t, filepath.Join(xyz, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	if _, err := convertLocalArchive(ctx, xyz, input, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(input)
	if _, err := convertLocalArchive(ctx, input, parent, ArchiveConvertOptions{OutputKind: "file", Overwrite: true}); err == nil {
		t.Fatal("output containing its input accepted")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected ancestor conversion modified its source")
	}
	huge := t.TempDir()
	tile := filepath.Join(huge, "0", "0", "0.png")
	archiveTestWrite(t, tile, []byte("sparse"))
	if err := os.Truncate(tile, int64(maxTileDataBytes)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := convertLocalArchive(ctx, huge, filepath.Join(t.TempDir(), "out.mbtiles"), ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversize tile accepted: %v", err)
	}
	// SQLite TEXT length stops at the first NUL. Count encoded bytes before
	// fetching tile_data, even when a malformed archive stores it as TEXT.
	textArchive := filepath.Join(t.TempDir(), "text.mbtiles")
	db, err := archiveOpenSQLite(textArchive, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(fmt.Sprintf("CREATE TABLE metadata(name TEXT,value TEXT); INSERT INTO metadata VALUES('format','png'); CREATE VIEW tiles AS SELECT 0 AS zoom_level,0 AS tile_column,0 AS tile_row,CAST(zeroblob(%d) AS TEXT) AS tile_data", maxTileDataBytes+1))
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("create oversized TEXT fixture: %v; close: %v", err, closeErr)
	}
	if _, err := convertLocalArchive(ctx, textArchive, filepath.Join(t.TempDir(), "out.mbtiles"), ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "input limit") {
		t.Fatalf("oversize TEXT tile was not rejected before loading its bytes: %v", err)
	}
}

func TestArchiveV2PMDirectoryBudgetRejectsForgedAllocations(t *testing.T) {
	body, err := archiveGzip([]byte{0xff, 0xff, 0xff, 0xff, 0x7f})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := archiveReadPMDirectory(body, pm.Gzip, 64<<20, 1<<20); err == nil {
		t.Fatal("forged directory count accepted")
	}
	normal := pm.SerializeEntries([]pm.EntryV3{{TileId: 1, Offset: 0, Length: 1, RunLength: 1}})
	if _, _, err := archiveReadPMDirectory(normal, pm.Gzip, uint64(len(normal)), 1); err == nil {
		t.Fatal("directory memory budget ignored")
	}
	if _, _, err := archiveReadPMDirectory(normal, pm.Gzip, 64<<20, 0); err == nil {
		t.Fatal("live entry budget ignored")
	}
}

func TestArchiveV2TileJSONCoordinateArrays(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"bounds":[-170,-80,170,80],"center":[10,20,0],"format":"png"}`))
	output := filepath.Join(t.TempDir(), "array.mbtiles")
	if _, err := convertLocalArchive(context.Background(), input, output, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	metadata, err := archiveWalkMB(context.Background(), output, func(int, uint32, uint32, []byte, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if metadata["bounds"] != "-170.0000000,-80.0000000,170.0000000,80.0000000" || metadata["center"] != "10.0000000,20.0000000,0" {
		t.Fatalf("array metadata not normalized: %v", metadata)
	}
}

func TestArchiveV2FileAppendRejectsNoncanonicalExistingNames(t *testing.T) {
	ctx := context.Background()
	regionB := t.TempDir()
	archiveTestWrite(t, filepath.Join(regionB, "1", "1", "0.png"), archiveTestPNG(t, color.Black))
	for _, oldPath := range []string{"01/0/1.png", "1/0/1.PNG", "1/00/1.png"} {
		t.Run(oldPath, func(t *testing.T) {
			output := t.TempDir()
			body := archiveTestPNG(t, color.White)
			archiveTestWrite(t, filepath.Join(output, filepath.FromSlash(oldPath)), body)
			metadata := []byte(`{"format":"png","tile_path":"{z}/{x}/{y}.{format}"}`)
			archiveTestWrite(t, filepath.Join(output, "metadata.json"), metadata)
			if _, err := convertLocalArchive(ctx, regionB, output, ArchiveConvertOptions{OutputKind: "file", Append: true}); err == nil || !strings.Contains(err.Error(), "canonical") {
				t.Fatalf("noncanonical append accepted: %v", err)
			}
			got, _ := os.ReadFile(filepath.Join(output, "metadata.json"))
			if !bytes.Equal(got, metadata) {
				t.Fatal("rejected append changed metadata")
			}
			repaired := filepath.Join(t.TempDir(), "canonical")
			if _, err := convertLocalArchive(ctx, output, repaired, ArchiveConvertOptions{}); err != nil {
				t.Fatalf("input conversion must still accept legacy paths: %v", err)
			}
			if _, err := convertLocalArchive(ctx, regionB, repaired, ArchiveConvertOptions{Append: true}); err != nil {
				t.Fatalf("canonicalized append failed: %v", err)
			}
		})
	}
}
