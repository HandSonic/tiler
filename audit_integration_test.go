package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/orb"
	"github.com/spf13/viper"
)

func auditRealMVT() []byte {
	// Tile.layer(name="test", extent=4096, version=2).
	return []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
}

func TestAuditMBTilesPBFEncodingMatchesStandard(t *testing.T) {
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	viper.Set("output.name", "vector")
	viper.Set("output.gzip_pbf", false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(auditRealMVT()) }))
	defer server.Close()
	task := NewTask([]Layer{{Zoom: 0, URL: server.URL, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "vector", Format: PBF, JSON: `{"vector_layers":[{"id":"test","fields":{}}]}`})
	t.Cleanup(task.Cancel)
	if err := task.Download(); err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("uncompressed PBF MBTiles must be rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vector.mbtiles")); !os.IsNotExist(err) {
		t.Fatalf("invalid encoding created a destination: %v", err)
	}
}

func TestAuditMBTilesPBFRequiresVectorMetadata(t *testing.T) {
	input := filepath.Join(t.TempDir(), "tiles")
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.pbf"), auditRealMVT())
	output := filepath.Join(t.TempDir(), "vector.mbtiles")
	if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{}); err == nil || !strings.Contains(err.Error(), "vector_layers") {
		t.Fatalf("PBF MBTiles without layer metadata must be rejected: %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("missing vector metadata created a destination: %v", err)
	}
}

// At high zoom a nonempty selected polygon can be much narrower than six
// decimal degrees. Metadata must not collapse it to a point or exclude it.
func TestAuditDownloadMetadataRetainsFineRegionBounds(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("output.name", "fine-region")
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	west, south, east, north := 10.00000001, 20.00000001, 10.00000049, 20.00000049
	region := orb.Polygon{{{west, south}, {east, south}, {east, north}, {west, north}, {west, south}}}
	task := NewTask([]Layer{{Zoom: 24, URL: server.URL + "/{z}/{x}/{y}.png", Collection: orb.Collection{region}}}, TileMap{Name: "fine", Format: PNG})
	if task == nil || task.initErr != nil {
		t.Fatalf("init: %+v", task)
	}
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	metadata, err := readDownloadFileMetadata(filepath.Join(task.File, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := downloadMetadataBounds(metadata["bounds"])
	if err != nil {
		t.Fatal(err)
	}
	if bounds[0] > west || bounds[1] > south || bounds[2] < east || bounds[3] < north || bounds[0] == bounds[2] || bounds[1] == bounds[3] {
		t.Fatalf("nonempty selected region collapsed or was clipped by metadata: got %q, want bounds covering %.8f,%.8f,%.8f,%.8f", metadata["bounds"], west, south, east, north)
	}
}

func TestAuditDownloadMBTilesMetadataUsesDatasetType(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", task.File)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("select value from metadata where name='type'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "overlay" && value != "baselayer" {
		t.Fatalf("MBTiles dataset type must be overlay or baselayer; source coordinate scheme leaked as %q", value)
	}
}

// A successful MBTiles export must validate layer field types and the optional
// zoom range against the dataset actually exported, not a source's full range.
func TestAuditMBTilesVectorSchemaMatchesExportedDataset(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		invalid      bool
	}{
		{"invalid-field-type", `{"vector_layers":[{"id":"test","fields":{"name":"varchar"}}]}`, true},
		{"source-wide-zooms", `{"vector_layers":[{"id":"test","fields":{},"minzoom":0,"maxzoom":14}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "tiles")
			archiveTestWrite(t, filepath.Join(input, "5", "0", "0.pbf"), auditRealMVT())
			output := filepath.Join(t.TempDir(), "vector.mbtiles")
			_, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{Metadata: map[string]string{"json": tc.schema}})
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "field") {
					t.Fatalf("invalid vector field must be rejected: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid source-wide layer zooms were rejected: %v", err)
			}
			auditCheckMBVectorSchema(t, output)
		})
		t.Run("download-"+tc.name, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", "mbtiles")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(auditRealMVT()) }))
			defer server.Close()
			task := NewTask([]Layer{{Zoom: 5, URL: server.URL, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "vector", Format: PBF, JSON: tc.schema})
			t.Cleanup(task.Cancel)
			err := task.Download()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "field") {
					t.Fatalf("invalid vector field must be rejected: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid source-wide layer zooms were rejected: %v", err)
			}
			auditCheckMBVectorSchema(t, task.File)
		})
	}
}

func auditCheckMBVectorSchema(t *testing.T, filename string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text string
	var min, max int
	if err := db.QueryRow("select value from metadata where name='json'").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select value from metadata where name='minzoom'").Scan(&min); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select value from metadata where name='maxzoom'").Scan(&max); err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		VectorLayers []struct {
			ID      string            `json:"id"`
			Fields  map[string]string `json:"fields"`
			MinZoom *int              `json:"minzoom"`
			MaxZoom *int              `json:"maxzoom"`
		} `json:"vector_layers"`
	}
	if err := json.Unmarshal([]byte(text), &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.VectorLayers) != 1 || metadata.VectorLayers[0].ID != "test" {
		t.Fatalf("export lost the input vector layer: %s", text)
	}
	for _, layer := range metadata.VectorLayers {
		for name, typ := range layer.Fields {
			if typ != "Number" && typ != "Boolean" && typ != "String" {
				t.Errorf("successful MBTiles export has nonstandard vector field %s.%s type %q", layer.ID, name, typ)
			}
		}
		if layer.MinZoom != nil && *layer.MinZoom < min || layer.MaxZoom != nil && *layer.MaxZoom > max {
			t.Errorf("successful MBTiles export has source layer zoom range outside dataset %d..%d: %s", min, max, text)
		}
	}
}

func TestAuditLooseRawPBFConversionAndAppendWorkflow(t *testing.T) {
	for _, kind := range []string{"file", "s3"} {
		t.Run(kind, func(t *testing.T) {
			store, out := archiveS3Store(t)
			viper.Set("output.name", "raw-vector")
			viper.Set("output.format", kind)
			viper.Set("output.gzip_pbf", false)
			viper.Set("output.tile_path", "{z}/{-y}/{x}")
			viper.Set("task.workers", 1)
			body := auditRealMVT()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
			defer server.Close()
			task := NewTask([]Layer{{Zoom: 2, URL: server.URL, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "raw-vector", Format: PBF})
			t.Cleanup(task.Cancel)
			if err := task.Download(); err != nil {
				t.Fatalf("raw PBF should remain supported for %s output: %v", kind, err)
			}
			input := task.File
			var saved []byte
			if kind == "s3" {
				input = "s3://test-bucket/archives/"
				saved = store.get("test-bucket/archives/2/1/2")
			} else {
				var err error
				saved, err = os.ReadFile(filepath.Join(input, "2", "1", "2"))
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(saved, body) {
				t.Fatal("explicit raw encoding was changed")
			}
			metadata := map[string]string{"json": `{"vector_layers":[{"id":"test","fields":{}}]}`}
			mb := filepath.Join(t.TempDir(), "a.mbtiles")
			if _, err := ConvertArchive(context.Background(), input, mb, ArchiveConvertOptions{S3: out, InputTilePath: "{z}/{-y}/{x}", Metadata: metadata}); err != nil {
				t.Fatalf("convert downloaded raw PBF to MBTiles: %v", err)
			}
			auditCheckMBVectorSchema(t, mb)
			remote := "s3://maps/a.pmtiles"
			if _, err := ConvertArchive(context.Background(), mb, remote, ArchiveConvertOptions{S3: out}); err != nil {
				t.Fatal(err)
			}
			regionB := filepath.Join(t.TempDir(), "b")
			archiveTestWrite(t, filepath.Join(regionB, "3", "1", "2.pbf"), body)
			if _, err := ConvertArchive(context.Background(), regionB, remote, ArchiveConvertOptions{S3: out, Append: true, Metadata: metadata}); err != nil {
				t.Fatalf("append raw PBF region B to remote PMTiles: %v", err)
			}
			files := filepath.Join(t.TempDir(), "unpacked")
			if _, err := ConvertArchive(context.Background(), remote, files, ArchiveConvertOptions{S3: out, OutputKind: "file", OutputTilePath: "{z}_{y}_{x}.pbf"}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"2_2_2.pbf", "3_2_1.pbf"} {
				got, err := os.ReadFile(filepath.Join(files, name))
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("workflow lost coordinate or bytes at %s: %v", name, err)
				}
			}
		})
	}
}

func TestAuditCLIRejectsNamedPipeMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX named pipe reproduction")
	}
	root := t.TempDir()
	pipe := filepath.Join(root, "metadata.json")
	if err := exec.Command("mkfifo", pipe).Run(); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}
	input := filepath.Join(root, "tiles")
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), tileDataPNG(t))
	done := make(chan error, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		done <- runCLI([]string{"convert", "-input", input, "-output", filepath.Join(root, "out.mbtiles"), "-metadata", pipe}, &stdout, &stderr)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("named pipe metadata should be rejected")
		}
	case <-time.After(3 * time.Second):
		// Release the reader before reporting failure so the test leaves no
		// blocked goroutine or open pipe behind.
		writer, err := os.OpenFile(pipe, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(`{}`))
		writer.Close()
		<-done
		t.Fatal("CLI metadata reader blocks on a named pipe before cancellation is initialized")
	}
}

func TestAuditCanceledRemoteAppendPreservesExistingArchives(t *testing.T) {
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &archiveS3TestStore{objects: map[string]archiveS3TestObject{}, failures: map[string]int{}}
			out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/2/3/3.png") {
					cancel()
				}
				store.serve(w, r)
			})
			root := t.TempDir()
			seed := filepath.Join(root, "a")
			body := tileDataPNG(t)
			archiveTestWrite(t, filepath.Join(seed, "1", "0", "0.png"), body)
			destination := filepath.Join(root, "existing."+kind)
			if _, err := ConvertArchive(context.Background(), seed, destination, ArchiveConvertOptions{}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			store.put("source/b/2/3/3.png", body)
			if _, err := ConvertArchive(ctx, "s3://source/b/", destination, ArchiveConvertOptions{S3: out, Append: true}); err == nil {
				t.Fatal("canceling an active input read returned success")
			}
			if ctx.Err() == nil {
				t.Fatal("test did not reach and cancel the input tile request")
			}
			after, err := os.ReadFile(destination)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("canceled remote append changed existing %s: %v", kind, err)
			}
		})
	}
}

func TestAuditDownloadDoesNotReplaceLiveWALDatabase(t *testing.T) {
	root := configureRegressionOutput(t)
	viper.Set("output.name", "live")
	viper.Set("output.format", "mbtiles")
	filename := filepath.Join(root, "live.mbtiles")
	seed := filepath.Join(t.TempDir(), "seed")
	archiveTestWrite(t, filepath.Join(seed, "1", "0", "0.png"), tileDataPNG(t))
	if _, err := ConvertArchive(context.Background(), seed, filename, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "UPDATE metadata SET value='live writer' WHERE name='description'", "INSERT INTO metadata(name,value) VALUES('audit-live','uncheckpointed writer data')"} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(filename + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("test requires nonempty live WAL: %v", err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer server.Close()
	if err := newRegressionTask(t, server.URL).Download(); err == nil {
		t.Fatal("download replaced a database while another SQLite writer still owns its WAL")
	}
	after, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("download changed a live WAL database before refusing replacement: %v", err)
	}
}

func TestAuditS3AppendRejectsUnreadableMetadataBeforePublishing(t *testing.T) {
	store, out := archiveS3Store(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	body := tileDataPNG(t)
	archiveTestWrite(t, filepath.Join(a, "1", "0", "0.png"), body)
	archiveTestWrite(t, filepath.Join(b, "1", "1", "1.png"), body)
	const prefix = "s3://maps/region/"
	if _, err := ConvertArchive(context.Background(), a, prefix, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	before := store.get("maps/region/metadata.json")
	puts := store.count(http.MethodPut)
	if _, err := ConvertArchive(context.Background(), b, prefix, ArchiveConvertOptions{S3: out, Append: true, Metadata: map[string]string{"description": strings.Repeat("\x00", 3<<20)}}); err == nil {
		t.Fatal("S3 append published JSON metadata larger than the S3 reader accepts")
	}
	if store.count(http.MethodPut) != puts || !bytes.Equal(before, store.get("maps/region/metadata.json")) {
		t.Fatal("S3 append wrote tiles or metadata before validating the serialized metadata size")
	}
}

func TestAuditPBFDownloadAppendDoesNotNarrowImplicitLayerZooms(t *testing.T) {
	for _, firstImplicit := range []bool{true, false} {
		t.Run(fmt.Sprintf("first-implicit-%t", firstImplicit), func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.name", "regions")
			viper.Set("output.format", "mbtiles")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(auditRealMVT()) }))
			defer server.Close()
			var filename string
			for i, zoom := range []int{0, 3} {
				viper.Set("output.append", i > 0)
				layer := map[string]interface{}{"id": "test", "fields": map[string]string{}}
				if (i == 0) != firstImplicit {
					layer["minzoom"], layer["maxzoom"] = zoom, zoom
				}
				description, err := json.Marshal(map[string]interface{}{"vector_layers": []interface{}{layer}})
				if err != nil {
					t.Fatal(err)
				}
				task := NewTask([]Layer{{Zoom: zoom, URL: server.URL, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "vector", Format: PBF, JSON: string(description)})
				t.Cleanup(task.Cancel)
				if err := task.Download(); err != nil {
					t.Fatal(err)
				}
				filename = task.File
			}
			db, err := sql.Open("sqlite3", filename)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow("SELECT count(*) FROM tiles").Scan(&count); err != nil || count != 2 {
				t.Fatalf("append lost a region: count=%d err=%v", count, err)
			}
			var text string
			if err := db.QueryRow("SELECT value FROM metadata WHERE name='json'").Scan(&text); err != nil {
				t.Fatal(err)
			}
			var description struct {
				Layers []struct {
					Min *int `json:"minzoom"`
					Max *int `json:"maxzoom"`
				} `json:"vector_layers"`
			}
			if err := json.Unmarshal([]byte(text), &description); err != nil || len(description.Layers) != 1 {
				t.Fatalf("bad merged description %q: %v", text, err)
			}
			layer := description.Layers[0]
			if layer.Min != nil && *layer.Min > 0 || layer.Max != nil && *layer.Max < 3 {
				t.Fatalf("merged layer zoom metadata hides existing real-MVT region A or B: %s", text)
			}
		})
	}
}
