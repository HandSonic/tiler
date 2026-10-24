package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func appendReadMetadata(t *testing.T, kind, path string) map[string]string {
	t.Helper()
	if kind == "file" {
		metadata, err := readDownloadFileMetadata(filepath.Join(path, "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metadata, err := readDownloadMBMetadata(db)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func appendReadTile(t *testing.T, task *Task, tile maptile.Tile) []byte {
	t.Helper()
	if task.outformat == "mbtiles" {
		return repairIntegrationReadTile(t, "mbtiles", task.File, tile)
	}
	path, err := task.fileTilePath(tile)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDownloadAppendRegionsPreservesTilesAndExtendsCoverage(t *testing.T) {
	for _, testcase := range []struct {
		name, kind, pattern string
		ordinaryResume      bool
	}{
		{name: "file", kind: "file"},
		{name: "file-custom", kind: "file", pattern: "{z}/{y}/{x}.{format}"},
		{name: "mbtiles", kind: "mbtiles"},
		{name: "ordinary-resume", kind: "mbtiles", ordinaryResume: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", testcase.kind)
			viper.Set("output.name", "all-regions")
			viper.Set("output.append", true)
			viper.Set("output.tile_path", testcase.pattern)
			red := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
			blue := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
			var second atomic.Bool
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if second.Load() {
					w.Write(blue)
				} else {
					w.Write(red)
				}
			}))
			defer server.Close()
			template := server.URL + "/{z}/{x}/{y}"
			regionA := Layer{Zoom: 1, URL: template, Collection: orb.Collection{orb.Point{-90, 45}}}
			first := NewTask([]Layer{regionA}, TileMap{Name: "region A", Format: PNG, JSON: `{"center":[-90,45,1],"custom":"keep","custom_id":9007199254740993,"nested":{"id":9007199254740993}}`})
			t.Cleanup(first.Cancel)
			if !first.resume {
				t.Fatal("append did not enable tile resume")
			}
			if err := first.Download(); err != nil {
				t.Fatal(err)
			}
			before := appendReadMetadata(t, testcase.kind, first.File)
			if before["minzoom"] != "1" || before["maxzoom"] != "1" {
				t.Fatalf("initial metadata=%v", before)
			}
			second.Store(true)
			if testcase.ordinaryResume {
				viper.Set("output.append", false)
				viper.Set("output.resume", true)
			}
			regionB := Layer{Zoom: 2, URL: template, Collection: orb.Collection{orb.Point{90, -45}}}
			next := NewTask([]Layer{regionA, regionB}, TileMap{Name: "region B", Format: PNG})
			t.Cleanup(next.Cancel)
			if err := next.Download(); err != nil {
				t.Fatal(err)
			}
			if next.File != first.File || calls.Load() != 2 {
				t.Fatalf("append path/requests=%s/%d, original=%s", next.File, calls.Load(), first.File)
			}
			if next.Stats() != (DownloadStats{Saved: 1, Skipped: 1}) {
				t.Fatalf("append stats=%+v", next.Stats())
			}
			if !bytes.Equal(appendReadTile(t, next, maptile.Tile{Z: 1, X: 0, Y: 0}), red) {
				t.Fatal("region A was replaced while appending region B")
			}
			if !bytes.Equal(appendReadTile(t, next, maptile.Tile{Z: 2, X: 3, Y: 2}), blue) {
				t.Fatal("region B was not added")
			}
			metadata := appendReadMetadata(t, testcase.kind, next.File)
			if metadata["name"] != "region A" || metadata["id"] != first.ID || metadata["minzoom"] != "1" || metadata["maxzoom"] != "2" || metadata["bounds"] != "-90.000000,-45.000000,90.000000,45.000000" {
				t.Fatalf("merged metadata=%v", metadata)
			}
			var tileJSON struct {
				Center []float64 `json:"center"`
				Custom string    `json:"custom"`
			}
			if err := json.Unmarshal([]byte(metadata["json"]), &tileJSON); err != nil || len(tileJSON.Center) != 3 || tileJSON.Center[0] != 0 || tileJSON.Center[1] != 0 || tileJSON.Center[2] != 1 || tileJSON.Custom != "keep" {
				t.Fatalf("TileJSON center/custom field not merged: %+v, %v", tileJSON, err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(metadata["json"]), &raw); err != nil || string(raw["custom_id"]) != "9007199254740993" || string(raw["nested"]) != `{"id":9007199254740993}` {
				t.Errorf("custom integers rounded: %s, %v", metadata["json"], err)
			}
		})
	}
}

func TestDownloadAppendRequiresStableLocalName(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles", "s3"} {
		t.Run(kind, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", kind)
			viper.Set("output.append", true)
			task := newRegressionTask(t, "http://example.test/{z}/{x}/{y}")
			if kind == "s3" {
				if task.initErr != nil || !task.resume {
					t.Fatalf("S3 prefix already identifies target: %v", task.initErr)
				}
			} else if task.initErr == nil || !strings.Contains(task.initErr.Error(), "output.name") {
				t.Fatalf("unstable local append accepted: %v", task.initErr)
			}
		})
	}
}

func TestDownloadAppendFormatMismatchDoesNotChangeOutput(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", kind)
			viper.Set("output.name", "dataset")
			viper.Set("output.append", true)
			body := tileDataPNG(t)
			firstFormat, nextFormat := PNG, JPG
			if kind == "file" {
				body = tileDataJPEG(t)
				firstFormat, nextFormat = JPG, "jpeg"
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Write(body)
			}))
			defer server.Close()
			first := NewTask([]Layer{{Zoom: 0, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "format-check", URL: server.URL + "/{z}/{x}/{y}", Format: firstFormat})
			t.Cleanup(first.Cancel)
			if err := first.Download(); err != nil {
				t.Fatal(err)
			}
			metadata := appendReadMetadata(t, kind, first.File)
			next := NewTask(first.Layers, TileMap{Name: "different map", Format: nextFormat})
			t.Cleanup(next.Cancel)
			if err := next.Download(); err == nil || !strings.Contains(err.Error(), "format") {
				t.Fatalf("format mismatch result=%v", err)
			}
			if calls.Load() != 1 || !reflect.DeepEqual(metadata, appendReadMetadata(t, kind, first.File)) {
				t.Fatal("format mismatch made a request or changed metadata")
			}
			if !bytes.Equal(appendReadTile(t, first, maptile.Tile{}), body) {
				t.Fatal("format mismatch changed original tile")
			}
		})
	}
}

func TestDownloadMetadataMergesVectorLayersAndRejectsConflictingTypes(t *testing.T) {
	previous := map[string]string{"name": "A", "format": "pbf", "bounds": "1,2,3,4", "minzoom": "1", "maxzoom": "3", "json": `{"vector_layers":[{"id":"roads","fields":{"class":"String"},"minzoom":1,"maxzoom":3}]}`}
	current := map[string]string{"name": "B", "format": "pbf", "bounds": "-10,-20,2,3", "minzoom": "2", "maxzoom": "8", "json": `{"vector_layers":[{"id":"roads","fields":{"elevation":"Number"},"minzoom":2,"maxzoom":8},{"id":"pois","fields":{}}]}`}
	merged, err := mergeDownloadMetadata(previous, current, false)
	if err != nil {
		t.Fatal(err)
	}
	var tileJSON struct {
		Layers []struct {
			ID     string            `json:"id"`
			Fields map[string]string `json:"fields"`
			Min    int               `json:"minzoom"`
			Max    int               `json:"maxzoom"`
		} `json:"vector_layers"`
	}
	if err := json.Unmarshal([]byte(merged["json"]), &tileJSON); err != nil {
		t.Fatal(err)
	}
	if len(tileJSON.Layers) != 2 || tileJSON.Layers[0].ID != "roads" || tileJSON.Layers[0].Min != 1 || tileJSON.Layers[0].Max != 8 || len(tileJSON.Layers[0].Fields) != 2 || merged["bounds"] != "-10.000000,-20.000000,3.000000,4.000000" {
		t.Fatalf("merge=%v, layers=%+v", merged, tileJSON.Layers)
	}
	current["json"] = `{"vector_layers":[{"id":"roads","fields":{"class":"Number"}}]}`
	if _, err := mergeDownloadMetadata(previous, current, false); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("incompatible vector schema accepted: %v", err)
	}
}

func TestDownloadMetadataResumeDoesNotRoundCoverageInward(t *testing.T) {
	previous := map[string]string{"format": "png", "bounds": "-90.0000001,-45.0000001,90.0000001,45.0000001", "minzoom": "1", "maxzoom": "20", "pixel_scale": "512"}
	current := map[string]string{"format": "png", "bounds": "-10,-20,10,20", "minzoom": "5", "maxzoom": "8"}
	merged, err := mergeDownloadMetadata(previous, current, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged["bounds"] != previous["bounds"] || merged["minzoom"] != "1" || merged["maxzoom"] != "20" {
		t.Fatalf("narrow resume shrank previous coverage: %v", merged)
	}
	if merged["pixel_scale"] != previous["pixel_scale"] {
		t.Fatalf("resume changed the supplied tile size: %v", merged)
	}
}

func TestDownloadFileResumeRejectsChangedLayoutAndAcceptsLegacyXYZ(t *testing.T) {
	task := fileMetadataTask(t.TempDir())
	task.resume = true
	if err := os.WriteFile(filepath.Join(task.File, "metadata.json"), []byte(`{"format":"png","bounds":[-90,-45,90,45],"center":[0,0,4],"minzoom":1,"maxzoom":10,"custom_bool":true,"custom_array":[1,false],"custom_obj":{"id":9007199254740993},"custom_id":9007199254740993,"custom_null":null,"custom_empty":"","Bounds":[1,2,3,4]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := task.validateFileResumeMetadata(); err != nil {
		t.Fatalf("legacy XYZ metadata rejected: %v", err)
	}
	if err := task.saveFileMetadata(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(task.File, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(before, &metadata); err != nil {
		t.Fatal(err)
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata["json"]), &extensions); err != nil {
		t.Fatalf("resume lost typed TileJSON metadata: %v", err)
	}
	for key, want := range map[string]string{"custom_bool": "true", "custom_array": "[1,false]", "custom_obj": `{"id":9007199254740993}`, "custom_id": "9007199254740993", "custom_null": "null", "Bounds": "[1,2,3,4]"} {
		if string(extensions[key]) != want {
			t.Errorf("resume changed %s: got %s, want %s", key, extensions[key], want)
		}
		if _, exists := metadata[key]; exists {
			t.Errorf("resume flattened typed metadata %s", key)
		}
	}
	if value, exists := metadata["custom_empty"]; !exists || value != "" {
		t.Error("resume dropped the existing empty extension")
	}
	if metadata["bounds"] != "-90.000000,-45.000000,90.000000,60.000000" || metadata["center"] != "0.000000,7.500000,5" {
		t.Fatalf("legacy coordinate arrays changed during resume: %v", metadata)
	}
	task.tilePath = "{z}/{y}/{x}.{format}"
	if err := task.validateFileResumeMetadata(); err == nil || !strings.Contains(err.Error(), "tile_path") {
		t.Fatalf("changed tile layout accepted: %v", err)
	}
	if err := task.saveFileMetadata(); err == nil {
		t.Fatal("save bypassed resume layout validation")
	}
	after, _ := os.ReadFile(filepath.Join(task.File, "metadata.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("invalid resume configuration changed original metadata")
	}
	for _, testcase := range []struct {
		name, metadata, pattern, scheme string
		wantErr                         bool
	}{
		{"implicit TMS rejects XYZ", `{"format":"png","scheme":"tms"}`, defaultTilePath, "", true},
		{"implicit TMS accepts TMS", `{"format":"png","scheme":"tms"}`, "{z}/{x}/{-y}.{format}", "tms", false},
		{"explicit layout determines scheme", `{"format":"png","scheme":"tms","tile_path":"{z}/{x}/{y}.{format}"}`, defaultTilePath, "xyz", false},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			task := fileMetadataTask(t.TempDir())
			task.resume, task.tilePath = true, testcase.pattern
			filename := filepath.Join(task.File, "metadata.json")
			if err := os.WriteFile(filename, []byte(testcase.metadata), 0600); err != nil {
				t.Fatal(err)
			}
			if err := task.saveFileMetadata(); (err != nil) != testcase.wantErr {
				t.Fatalf("resume result: %v, want error=%v", err, testcase.wantErr)
			}
			body, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			if testcase.wantErr {
				if string(body) != testcase.metadata {
					t.Fatal("rejected TMS resume changed original metadata")
				}
				return
			}
			var metadata map[string]string
			if err := json.Unmarshal(body, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["tile_path"] != testcase.pattern || metadata["scheme"] != testcase.scheme {
				t.Fatalf("resume published contradictory layout metadata: %v", metadata)
			}
		})
	}
}

func TestDownloadFileMetadataRejectsNullCoordinateArrays(t *testing.T) {
	for _, key := range []string{"bounds", "center"} {
		t.Run(key, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "metadata.json")
			for _, value := range []string{`[null,0,1,1]`, `null`} {
				body := fmt.Sprintf(`{"%s":%s}`, key, value)
				if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := readDownloadFileMetadata(filename); err == nil {
					t.Fatal("null metadata coordinate was silently converted to zero or omitted")
				}
			}
		})
	}
}

func TestDownloadMetadataRejectsReversedExistingZoomIntervals(t *testing.T) {
	incoming := map[string]string{"format": "png", "bounds": "-10,-10,10,10", "minzoom": "5", "maxzoom": "10"}
	previous := map[string]string{"format": "png", "bounds": "-10,-10,10,10", "minzoom": "15", "maxzoom": "1"}
	if _, err := mergeDownloadMetadata(previous, incoming, false); err == nil {
		t.Error("reversed existing dataset zooms were silently hidden by the incoming range")
	}
	incoming["json"] = `{"vector_layers":[{"id":"test","fields":{},"minzoom":15,"maxzoom":1}]}`
	if _, err := mergeDownloadMetadata(nil, incoming, false); err == nil {
		t.Error("reversed vector layer zooms were accepted")
	}
}

func TestDownloadVectorLayerMergePreservesOmittedZoomLimits(t *testing.T) {
	for _, oldJSON := range []string{
		`{"vector_layers":[{"id":"test","fields":{}}]}`,
		`{"vector_layers":[{"id":"test","fields":{},"minzoom":0,"maxzoom":0}]}`,
	} {
		newJSON := `{"vector_layers":[{"id":"test","fields":{},"minzoom":3,"maxzoom":3}]}`
		if strings.Contains(oldJSON, "minzoom") {
			newJSON = `{"vector_layers":[{"id":"test","fields":{}}]}`
		}
		old := map[string]string{"format": PBF, "bounds": "0,0,1,1", "minzoom": "0", "maxzoom": "0", "json": oldJSON}
		incoming := map[string]string{"format": PBF, "bounds": "0,0,1,1", "minzoom": "3", "maxzoom": "3", "json": newJSON}
		merged, err := mergeDownloadMetadata(old, incoming, false)
		if err != nil {
			t.Fatal(err)
		}
		var description map[string]interface{}
		if err := json.Unmarshal([]byte(merged["json"]), &description); err != nil {
			t.Fatal(err)
		}
		layer := description["vector_layers"].([]interface{})[0].(map[string]interface{})
		if _, exists := layer["minzoom"]; exists {
			t.Error("merging a bounded layer narrowed an omitted minzoom")
		}
		if _, exists := layer["maxzoom"]; exists {
			t.Error("merging a bounded layer narrowed an omitted maxzoom")
		}
	}
}

func TestDownloadMBMetadataChecksSizeBeforeLoadingValues(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("create table metadata (name text,value text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("insert into metadata (name,value) values ('large',zeroblob(?))", maxMetadataBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readDownloadMBMetadata(db); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("oversized MBTiles metadata was loaded before its size was checked: %v", err)
	}
}
