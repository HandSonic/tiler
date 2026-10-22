package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	pm "tiler/internal/pmtiles"
)

func archiveTestPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			picture.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func archiveTestWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}
func archiveTestReadPM(t *testing.T, path string) (pm.HeaderV3, map[uint64][]byte, map[string]interface{}) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body[:8]) != "PMTiles\x03" {
		t.Fatal("not PMTiles v3")
	}
	// Inspect fixed offsets independently of the serializer as well.
	if binary.LittleEndian.Uint64(body[8:16]) != 127 {
		t.Fatal("root is not immediately after 127-byte header")
	}
	header, err := pm.DeserializeHeader(body)
	if err != nil {
		t.Fatal(err)
	}
	if header.RootOffset+header.RootLength > 16384 {
		t.Fatal("root does not fit first 16KiB")
	}
	root, _, err := archiveReadPMDirectory(body[header.RootOffset:header.RootOffset+header.RootLength], header.InternalCompression, 64<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	all := make([]pm.EntryV3, 0)
	for _, entry := range root {
		if entry.RunLength > 0 {
			all = append(all, entry)
			continue
		}
		leaf, _, err := archiveReadPMDirectory(body[header.LeafDirectoryOffset+entry.Offset:header.LeafDirectoryOffset+entry.Offset+uint64(entry.Length)], header.InternalCompression, 64<<20, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, leaf...)
	}
	contents := make(map[uint64][]byte)
	for _, entry := range all {
		for i := uint32(0); i < entry.RunLength; i++ {
			contents[entry.TileId+uint64(i)] = body[header.TileDataOffset+entry.Offset : header.TileDataOffset+entry.Offset+uint64(entry.Length)]
		}
	}
	reader, err := gzip.NewReader(bytes.NewReader(body[header.MetadataOffset : header.MetadataOffset+header.MetadataLength]))
	if err != nil {
		t.Fatal(err)
	}
	metadataBytes, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	return header, contents, metadata
}

func TestArchiveXYZToMBTilesAndPMTiles(t *testing.T) {
	input := filepath.Join(t.TempDir(), "tiles ?# map")
	red, blue := archiveTestPNG(t, color.RGBA{R: 255, A: 255}), archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	archiveTestWrite(t, filepath.Join(input, "1", "0", "0.png"), red)
	archiveTestWrite(t, filepath.Join(input, "1", "1", "1.png"), blue)
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"name":"test map","format":"png","json":{"name":"nested map","description":"nested description"},"bounds":"-180,-85.0511288,180,85.0511288"}`))
	archiveTestWrite(t, filepath.Join(input, "failures.json"), []byte(`[{"z":1,"x":0,"y":1}]`))
	output := filepath.Join(t.TempDir(), "map ?#.mbtiles")
	result, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ArchiveConvertResult{Tiles: 2, Format: PNG}) {
		t.Fatalf("result=%+v", result)
	}
	db, err := archiveOpenSQLite(output, true)
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err = db.QueryRow("SELECT tile_data FROM tiles WHERE zoom_level=1 AND tile_column=0 AND tile_row=1").Scan(&body); err != nil || !bytes.Equal(body, red) {
		t.Fatalf("TMS row flip wrong: %v", err)
	}
	var format, minzoom, maxzoom string
	if err = db.QueryRow("SELECT value FROM metadata WHERE name='format'").Scan(&format); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT value FROM metadata WHERE name='minzoom'").Scan(&minzoom); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT value FROM metadata WHERE name='maxzoom'").Scan(&maxzoom); err != nil {
		t.Fatal(err)
	}
	if format != "png" || minzoom != "1" || maxzoom != "1" {
		t.Fatalf("bad metadata %s %s %s", format, minzoom, maxzoom)
	}
	db.Close()
	for _, source := range []string{input, output} {
		pmout := filepath.Join(t.TempDir(), "map.pmtiles")
		result, err = ConvertArchive(context.Background(), source, pmout, ArchiveConvertOptions{})
		if err != nil {
			t.Fatal(err)
		}
		header, contents, metadata := archiveTestReadPM(t, pmout)
		if header.TileType != pm.Png || header.TileCompression != pm.NoCompression || header.MinZoom != 1 || header.MaxZoom != 1 || header.AddressedTilesCount != 2 || !header.Clustered {
			t.Fatalf("bad header=%+v", header)
		}
		if !bytes.Equal(contents[1], red) || !bytes.Equal(contents[3], blue) {
			t.Fatal("Hilbert coordinates or payload changed")
		}
		if metadata["name"] != "test map" || metadata["description"] != "nested description" || metadata["format"] != "png" || metadata["scheme"] != "xyz" {
			t.Fatalf("metadata=%v", metadata)
		}
	}
}

func TestArchiveTranscodesTargetJPEG(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.RGBA{R: 255, A: 255}))
	output := filepath.Join(t.TempDir(), "map.pmtiles")
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{Format: "jpg"}); err != nil {
		t.Fatal(err)
	}
	header, contents, _ := archiveTestReadPM(t, output)
	if header.TileType != pm.Jpeg {
		t.Fatal("target tile type is not JPEG")
	}
	if _, err := jpeg.Decode(bytes.NewReader(contents[0])); err != nil {
		t.Fatal("PNG was renamed instead of transcoded:", err)
	}
}

func TestArchiveRejectsRasterVectorTranscoding(t *testing.T) {
	for _, tt := range []struct{ source, target string }{{PNG, PBF}, {PBF, PNG}} {
		t.Run(tt.source+" to "+tt.target, func(t *testing.T) {
			input := t.TempDir()
			body := []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
			if tt.source == PNG {
				body = archiveTestPNG(t, color.White)
			}
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0."+tt.source), body)
			if tt.source == PBF {
				archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"vector_layers":[{"id":"test","fields":{}}]}`))
			}
			mbtiles := filepath.Join(t.TempDir(), "input.mbtiles")
			if _, err := ConvertArchive(context.Background(), input, mbtiles, ArchiveConvertOptions{}); err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{input, mbtiles} {
				output := filepath.Join(t.TempDir(), "out.pmtiles")
				if _, err := ConvertArchive(context.Background(), source, output, ArchiveConvertOptions{Format: tt.target}); err == nil || !strings.Contains(err.Error(), "raster") {
					t.Fatalf("invalid raster/vector conversion accepted: %v", err)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("partial output left: %v", err)
				}
			}
		})
	}
}

func TestArchiveRejectsImagesMislabeledAsPBF(t *testing.T) {
	image := archiveTestPNG(t, color.White)
	packed, err := archiveGzip(image)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := archiveGzip(packed)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{image, packed, nested, []byte("BM"), {0, 0, 1, 0}, []byte("RIFF\x00\x00\x00\x00WEBP")} {
		for _, source := range []string{"", PBF} {
			if _, err := archivePrepareTileData(body, source, PBF); err == nil || !strings.Contains(err.Error(), "raster") {
				t.Fatalf("image data accepted as PBF: %v", err)
			}
		}
	}
}

func TestArchivePBFHasSingleGzipLayer(t *testing.T) {
	input := t.TempDir()
	// A valid MVT with one empty layer named test, extent 4096, version 2.
	raw := []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
	// Concatenated gzip is readable by Go but must be normalized for browsers.
	packed, err := archiveGzip(raw[:6])
	if err != nil {
		t.Fatal(err)
	}
	rest, err := archiveGzip(raw[6:])
	if err != nil {
		t.Fatal(err)
	}
	packed = append(packed, rest...)
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.pbf"), packed)
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"vector_layers":[{"id":"test","fields":{}}]}`))
	output := filepath.Join(t.TempDir(), "vector.pmtiles")
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	header, contents, _ := archiveTestReadPM(t, output)
	if header.TileType != pm.Mvt || header.TileCompression != pm.Gzip {
		t.Fatalf("bad vector header=%+v", header)
	}
	compressed := bytes.NewReader(contents[0])
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	reader.Multistream(false)
	body, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || compressed.Len() != 0 || !bytes.Equal(body, raw) {
		t.Fatalf("PBF changed or double gzipped: %x %v", body, err)
	}
}

func TestArchivePBFRequiresVectorLayerMetadata(t *testing.T) {
	for _, metadata := range []string{
		`{}`,
		`{"vector_layers":null}`,
		`{"vector_layers":{}}`,
		`{"vector_layers":[{}]}`,
		`{"vector_layers":[{"id":"test"}]}`,
		`{"vector_layers":[{"id":"test","fields":{"name":3}}]}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			input := t.TempDir()
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0.pbf"), []byte{0x1a, 0})
			archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(metadata))
			output := filepath.Join(t.TempDir(), "out.pmtiles")
			if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "vector_layers") {
				t.Fatalf("invalid vector metadata accepted: %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("partial output left: %v", err)
			}
		})
	}
}

func TestArchiveVectorLayerValidationMatchesTileJSON(t *testing.T) {
	for _, text := range []string{
		`[]`,
		`[{"id":"","fields":{}}]`,
		`[{"id":"test","fields":{"population":"Number of residents"},"description":"example"}]`,
	} {
		var metadata interface{}
		if err := json.Unmarshal([]byte(text), &metadata); err != nil {
			t.Fatal(err)
		}
		if err := archiveValidateVectorLayers(metadata); err != nil {
			t.Fatalf("valid TileJSON vector_layers rejected: %s: %v", text, err)
		}
	}
}

func TestArchiveSQLiteURIWindowsAndUnixPaths(t *testing.T) {
	for _, tt := range []struct{ input, path, prefix string }{
		{`C:\maps ?#\a.mbtiles`, "/C:/maps ?#/a.mbtiles", "file:///C:/"},
		{`D:/maps ?#/a.mbtiles`, "/D:/maps ?#/a.mbtiles", "file:///D:/"},
		{`\\server\share\maps ?#\a.mbtiles`, "//server/share/maps ?#/a.mbtiles", "file:////server/"},
		{`//server/share/maps ?#/a.mbtiles`, "//server/share/maps ?#/a.mbtiles", "file:////server/"},
		{`/maps ?#/a.mbtiles`, "/maps ?#/a.mbtiles", "file:///"},
		{`/maps\name/a.mbtiles`, `/maps\name/a.mbtiles`, "file:///"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			dsn := archiveSQLiteURI(tt.input, true)
			parsed, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(dsn, tt.prefix) || parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != tt.path || parsed.Fragment != "" {
				t.Fatalf("invalid file URI %q -> %+v; want path %q", dsn, parsed, tt.path)
			}
			if parsed.Query().Get("mode") != "ro" || parsed.Query().Get("_busy_timeout") != "5000" {
				t.Fatalf("missing SQLite options: %q", dsn)
			}
			writable, err := url.Parse(archiveSQLiteURI(tt.input, false))
			if err != nil || writable.Query().Get("mode") != "" {
				t.Fatalf("unexpected readonly mode: %v %v", writable, err)
			}
		})
	}
}

func TestArchivePublishNoReplaceRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "complete.tmp"), filepath.Join(dir, "output.pmtiles")
	archiveTestWrite(t, from, []byte("new"))
	archiveTestWrite(t, to, []byte("existing"))
	if err := archivePublishNoReplace(from, to); err == nil {
		t.Fatal("existing destination replaced")
	}
	body, err := os.ReadFile(to)
	if err != nil || string(body) != "existing" {
		t.Fatalf("existing destination changed: %q %v", body, err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := archivePublishNoReplace(from, to); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(to)
	if err != nil || string(body) != "new" {
		t.Fatalf("complete file not published: %q %v", body, err)
	}
}

func TestArchivePMTilesZoomLimitPreservesMBTilesZoom30(t *testing.T) {
	compatible := t.TempDir()
	archiveTestWrite(t, filepath.Join(compatible, "26", "0", "0.png"), archiveTestPNG(t, color.White))
	if _, err := ConvertArchive(context.Background(), compatible, filepath.Join(t.TempDir(), "zoom26.pmtiles"), ArchiveConvertOptions{}); err != nil {
		t.Fatalf("valid zoom 26 PMTiles rejected: %v", err)
	}
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "30", "0", "0.png"), archiveTestPNG(t, color.White))
	mbtiles := filepath.Join(t.TempDir(), "zoom30.mbtiles")
	if _, err := ConvertArchive(context.Background(), input, mbtiles, ArchiveConvertOptions{}); err != nil {
		t.Fatalf("valid zoom 30 MBTiles rejected: %v", err)
	}
	for _, source := range []string{input, mbtiles} {
		output := filepath.Join(t.TempDir(), "out.pmtiles")
		if _, err := ConvertArchive(context.Background(), source, output, ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "zoom 0..26") {
			t.Fatalf("unsupported JavaScript zoom accepted: %v", err)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("partial high zoom output left: %v", err)
		}
	}
}

func TestArchivePMTilesUsesLeafDirectory(t *testing.T) {
	// Exercise global-scale sizing without allocating or generating the tiles.
	maxTiles := int64(maxPMRootEntries) * int64(pmDirectoryEntryLimit-maxPMRootEntries)
	for _, count := range []int64{357913941, maxTiles} { // world z0..14 and the live-entry boundary
		if _, err := archivePMLeafSize(count); err != nil {
			t.Fatalf("PMTiles sizing rejected %d tiles: %v", count, err)
		}
	}
	for _, count := range []int64{maxTiles + 1, 1431655765} { // boundary + 1 and world z0..15
		if _, err := archivePMLeafSize(count); err == nil {
			t.Fatalf("PMTiles sizing accepted %d tiles beyond its reader's directory capacity", count)
		}
	}
	input := filepath.Join(t.TempDir(), "source.mbtiles")
	db, err := archiveOpenSQLite(input, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE metadata (name TEXT,value TEXT); CREATE TABLE tiles (zoom_level INTEGER,tile_column INTEGER,tile_row INTEGER,tile_data BLOB); INSERT INTO metadata VALUES ('format','png')"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO tiles VALUES (5,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	body := archiveTestPNG(t, color.White)
	for i := 0; i < 513; i++ {
		if _, err := stmt.Exec(i%32, i/32, body); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	output := filepath.Join(t.TempDir(), "leaf.pmtiles")
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	header, contents, _ := archiveTestReadPM(t, output)
	if header.LeafDirectoryLength == 0 || header.AddressedTilesCount != 513 || len(contents) != 513 {
		t.Fatalf("missing leaf directory or entries: %+v %d", header, len(contents))
	}
	for _, data := range contents {
		if !bytes.Equal(data, body) {
			t.Fatal("wrong tile body")
		}
	}
}

func TestArchiveRejectsInvalidInputWithoutPublishing(t *testing.T) {
	tests := []struct {
		name, path string
		body       []byte
		metadata   string
	}{
		{"bad path", "1/no/0.png", nil, ""},
		{"outside grid", "1/3/0.png", nil, ""},
		{"negative zoom", "-1/0/0.png", nil, ""},
		{"corrupt image", "0/0/0.png", []byte("broken"), ""},
		{"bad metadata", "0/0/0.png", nil, `{"bounds":"invalid"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := t.TempDir()
			body := tt.body
			if body == nil {
				body = archiveTestPNG(t, color.White)
			}
			archiveTestWrite(t, filepath.Join(input, filepath.FromSlash(tt.path)), body)
			if tt.metadata != "" {
				archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(tt.metadata))
			}
			dir := t.TempDir()
			output := filepath.Join(dir, "map.pmtiles")
			if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err == nil {
				t.Fatal("invalid input succeeded")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial output or temp artifacts left: %v", entries)
			}
		})
	}
}

func TestArchiveExistingOutputAndOverwrite(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	output := filepath.Join(t.TempDir(), "existing.mbtiles")
	archiveTestWrite(t, output, []byte("old data"))
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err == nil {
		t.Fatal("output was silently replaced")
	}
	body, _ := os.ReadFile(output)
	if string(body) != "old data" {
		t.Fatal("old output changed")
	}
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	db, err := archiveOpenSQLite(output, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM tiles").Scan(&count); err != nil || count != 1 {
		t.Fatalf("overwrite did not publish valid database: %d %v", count, err)
	}
	if _, err := ConvertArchive(context.Background(), output, output, ArchiveConvertOptions{Overwrite: true}); err == nil {
		t.Fatal("input/output identity accepted")
	}
}

func TestArchiveRejectsMixedFormatsAndDuplicateCoordinates(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		input := t.TempDir()
		archiveTestWrite(t, filepath.Join(input, "1", "0", "0.png"), archiveTestPNG(t, color.White))
		path := "1/0/1.jpg"
		if duplicate {
			path = "1/0/0.jpg"
		}
		archiveTestWrite(t, filepath.Join(input, filepath.FromSlash(path)), archiveTestPNG(t, color.Black))
		output := filepath.Join(t.TempDir(), "out.mbtiles")
		options := ArchiveConvertOptions{}
		if duplicate {
			options.Format = "png"
		}
		_, err := ConvertArchive(context.Background(), input, output, options)
		if err == nil {
			t.Fatalf("mixed/duplicate accepted: duplicate=%v", duplicate)
		}
	}
}

func TestArchiveMetadataFormatDoesNotSilentlyTranscodeMixedInput(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "1", "0", "0.png"), archiveTestPNG(t, color.White))
	archiveTestWrite(t, filepath.Join(input, "1", "0", "1.jpg"), archiveTestPNG(t, color.Black))
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"format":"png"}`))
	output := filepath.Join(t.TempDir(), "out.mbtiles")
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "mixed tile formats") {
		t.Fatalf("mixed input silently transcoded: %v", err)
	}
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{Format: "png"}); err != nil {
		t.Fatalf("explicit target format rejected: %v", err)
	}
}

func TestArchiveCancellationAndBadDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := filepath.Join(t.TempDir(), "cancel.pmtiles")
	if _, err := ConvertArchive(ctx, t.TempDir(), output, ArchiveConvertOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	path := filepath.Join(t.TempDir(), "broken.mbtiles")
	archiveTestWrite(t, path, []byte("not SQLite"))
	if _, err := ConvertArchive(context.Background(), path, output, ArchiveConvertOptions{}); err == nil {
		t.Fatal("bad database accepted")
	}
}

func TestArchiveRejectsInvalidMBTilesCoordinates(t *testing.T) {
	input := filepath.Join(t.TempDir(), "invalid.mbtiles")
	db, err := sql.Open("sqlite3", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE metadata(name TEXT,value TEXT);INSERT INTO metadata VALUES ('format','png');CREATE TABLE tiles(zoom_level INTEGER,tile_column INTEGER,tile_row INTEGER,tile_data BLOB);INSERT INTO tiles VALUES (1,0,3,x'01')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = ConvertArchive(context.Background(), input, filepath.Join(t.TempDir(), "out.pmtiles"), ArchiveConvertOptions{})
	if err == nil || !strings.Contains(err.Error(), "invalid tile coordinate") {
		t.Fatalf("invalid MBTiles grid accepted: %v", err)
	}
}
