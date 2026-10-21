package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/viper"
)

func TestCLIConvertWritesRequestedFormat(t *testing.T) {
	input := filepath.Join(t.TempDir(), "xyz")
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.RGBA{G: 255, A: 255}))
	output := filepath.Join(t.TempDir(), "converted.mbtiles")
	var stdout, stderr bytes.Buffer
	if err := runCLI([]string{"convert", "-input", input, "-output", output, "-format", "jpg", "-name", "cli-conversion"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Converted 1 jpg tiles") {
		t.Fatalf("unexpected output: %s", &stdout)
	}
	db, err := sql.Open("sqlite3", output)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body []byte
	if err := db.QueryRow("select tile_data from tiles").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("CLI did not transcode PNG to JPEG: %v", err)
	}
	var name string
	if err := db.QueryRow("select value from metadata where name='name'").Scan(&name); err != nil || name != "cli-conversion" {
		t.Fatalf("archive name = %q, error %v", name, err)
	}
}

func TestCLILegacyDownloadThenRepair(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	directory := t.TempDir()
	body := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	var repair atomic.Bool
	var calls atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/1/1/1.png" && !repair.Load() {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(body)
	}))
	defer source.Close()
	geojson := filepath.Join(directory, "region.geojson")
	archiveTestWrite(t, geojson, []byte(`{"type":"MultiPoint","coordinates":[[-90,45],[90,-45]]}`))
	config := filepath.Join(directory, "conf.toml")
	text := fmt.Sprintf(`[output]
format = "mbtiles"
directory = %q
name = "cli"
resume = false
[task]
workers = 2
savepipe = 1
retries = 0
[tm]
name = "cli-map"
format = "png"
schema = "xyz"
url = %q
[[lrs]]
min = 1
max = 1
geojson = %q
`, directory, source.URL+"/{z}/{x}/{y}.png", geojson)
	archiveTestWrite(t, config, []byte(text))
	if err := runCLI([]string{"-c", config}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("partial download unexpectedly succeeded")
	}
	if calls.Load() != 2 {
		t.Fatalf("initial requests = %d", calls.Load())
	}
	report := filepath.Join(directory, "cli.mbtiles.failures.json")
	if _, err := os.Stat(report); err != nil {
		t.Fatal(err)
	}
	repair.Store(true)
	if err := runCLI([]string{"repair", "-c", config, "-failures", report}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("repair should request only one failed tile, total = %d", calls.Load())
	}
	manifest, err := LoadFailureManifest(report)
	if err != nil || manifest.Len() != 0 {
		t.Fatalf("manifest unresolved after repair: %v", err)
	}
	var stdout bytes.Buffer
	if err := runCLI([]string{"repair", "-c", config, "-failures", report}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !strings.Contains(stdout.String(), "empty") {
		t.Fatalf("empty manifest triggered a download or omitted result: %s", &stdout)
	}
}

func TestCLIHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"convert", "-h"}, {"upload", "-h"}, {"repair", "-h"}} {
		var stdout, stderr bytes.Buffer
		if err := runCLI(args, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if stdout.Len()+stderr.Len() == 0 {
			t.Fatalf("%v produced no help", args)
		}
	}
	for _, args := range [][]string{{"unknown"}, {"convert"}, {"repair"}, {"upload"}, {"upload", "-timeout", "0s", "file.mbtiles"}, {"upload", "-concurrency", "33", "file.mbtiles"}, {"download", "-failures", "a.json"}} {
		if err := runCLI(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
}

func TestCLIAppendAndPMTilesDirectoryRoundTrip(t *testing.T) {
	directory := t.TempDir()
	a, b := filepath.Join(directory, "a"), filepath.Join(directory, "b")
	first := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
	second := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	archiveTestWrite(t, filepath.Join(a, "0", "0", "0.png"), first)
	archiveTestWrite(t, filepath.Join(b, "2", "3", "1.png"), second)
	mb, pm := filepath.Join(directory, "all.mbtiles"), filepath.Join(directory, "all.pmtiles")
	files, roundtrip := filepath.Join(directory, "files"), filepath.Join(directory, "roundtrip.mbtiles")
	metadata := filepath.Join(directory, "extra.json")
	archiveTestWrite(t, metadata, []byte(`{"description":"two regions"}`))
	for _, args := range [][]string{
		{"convert", "-input", a, "-output", mb},
		{"convert", "-input", b, "-output", mb, "-append", "-metadata", metadata},
		{"convert", "-input", mb, "-output", pm},
		{"convert", "-input", pm, "-output", files, "-output-type", "file", "-tile-path", "{z}/{y}/{x}"},
		{"convert", "-input", files, "-output", roundtrip},
	} {
		if err := runCLI(args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(files, "2", "1", "3"))
	if err != nil || !bytes.Equal(got, second) {
		t.Fatalf("custom no-extension layout: %v", err)
	}
	db, err := sql.Open("sqlite3", roundtrip)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("select count(*) from tiles").Scan(&count); err != nil || count != 2 {
		t.Fatalf("A/B roundtrip lost tiles: count=%d err=%v", count, err)
	}
	if err := db.QueryRow("select tile_data from tiles where zoom_level=0 and tile_column=0 and tile_row=0").Scan(&got); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("append changed original region A: %v", err)
	}
}
