package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func TestAuditCanceledTaskDoesNotMutateExistingOutput(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			dir := configureRegressionOutput(t)
			viper.Set("output.format", kind)
			task := newRegressionTask(t, "https://example.test/{z}/{x}/{y}")
			task.File = filepath.Join(dir, "protected.mbtiles")
			protected := task.File
			if kind == "file" {
				if err := os.Mkdir(task.File, 0700); err != nil {
					t.Fatal(err)
				}
				protected = filepath.Join(task.File, "metadata.json")
			}
			original := []byte("existing output must survive a canceled task")
			if err := os.WriteFile(protected, original, 0600); err != nil {
				t.Fatal(err)
			}
			task.Cancel()
			if err := task.Download(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled task error=%v", err)
			}
			got, err := os.ReadFile(protected)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("canceled task changed its existing output: %v", err)
			}
		})
	}
}

func TestAuditTaskCannotRunTwiceOrRecreateDatabase(t *testing.T) {
	configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	body := tileDataPNG(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer source.Close()
	task := newRegressionTask(t, source.URL+"/{z}/{x}/{y}")
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(task.File)
	if err != nil {
		t.Fatal(err)
	}
	var secondErr error
	var panicValue interface{}
	func() {
		defer func() { panicValue = recover() }()
		secondErr = task.Download()
	}()
	if panicValue != nil {
		t.Errorf("running a task twice panicked: %v", panicValue)
	}
	if secondErr == nil {
		t.Error("running a task twice returned success")
	}
	after, err := os.ReadFile(task.File)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("second run recreated the original database: %v", err)
	}
}

func TestAuditRepairRejectsChangedTargetFormat(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			dir := configureRegressionOutput(t)
			viper.Set("output.format", kind)
			var calls atomic.Int32
			body := tileDataPNG(t)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(body) }))
			defer source.Close()
			template := source.URL + "/{z}/{x}/{y}"
			task := newRegressionTask(t, template)
			if err := task.Download(); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				metadata := task.MetaItems()
				metadata["format"] = JPG
				encoded, _ := json.Marshal(metadata)
				if err := os.WriteFile(filepath.Join(task.File, "metadata.json"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite3", task.File)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("update metadata set value=? where name='format'", JPG)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			manifest := newFailureManifest(task.Name, PNG)
			if err := manifest.SetTarget(FailureTarget{Kind: kind, Path: task.File}); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Record(maptile.Tile{Z: 0, X: 0, Y: 0}, template, fmt.Errorf("failed")); err != nil {
				t.Fatal(err)
			}
			report := filepath.Join(dir, "repair.json")
			if err := manifest.Save(report); err != nil {
				t.Fatal(err)
			}
			repair, err := NewRepairTask(task.Layers, task.TileMap, manifest, report)
			if err == nil {
				err = repair.Download()
				repair.Cancel()
			}
			if err == nil {
				t.Error("repair accepted a different target format and resolved its report")
			}
			loaded, err := LoadFailureManifest(report)
			if err != nil || loaded.Len() != 1 || calls.Load() != 1 {
				t.Fatalf("rejected repair changed report or requested source: %v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestAuditFailureErrorsDoNotLeakQuotedOrBearerCredentials(t *testing.T) {
	for _, message := range []string{
		`Authorization: Bearer private-bearer-token`,
		`{"access_token":"private-json-token"}`,
		`api_key="private quoted key"`,
		`password='private quoted password'`,
	} {
		got := safeFailureError(errors.New(message))
		if strings.Contains(got, "private") {
			t.Errorf("credential leaked: %s", got)
		}
	}
}

func TestAuditFailureReportCannotReplaceUnknownJSON(t *testing.T) {
	dir := configureRegressionOutput(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	report := filepath.Join(dir, "important.json")
	original := []byte(`{"important":"existing unrelated data"}`)
	if err := os.WriteFile(report, original, 0600); err != nil {
		t.Fatal(err)
	}
	viper.Set("output.failure_file", report)
	task := newRegressionTask(t, server.URL+"/{z}/{x}/{y}")
	if err := task.Download(); err == nil {
		t.Error("unknown existing failure_file was accepted")
	}
	got, err := os.ReadFile(report)
	if err != nil || !bytes.Equal(got, original) || requests.Load() != 0 {
		t.Fatalf("unknown JSON report was overwritten or network started: err=%v requests=%d", err, requests.Load())
	}
}

func TestAuditRepairCannotRecreateMissingTilesTable(t *testing.T) {
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	var requests atomic.Int32
	body := tileDataPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(body)
	}))
	defer server.Close()
	template := server.URL + "/{z}/{x}/{y}"
	task := newRegressionTask(t, template)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", task.File)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("drop table tiles")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(task.File)
	if err != nil {
		t.Fatal(err)
	}
	manifest := newFailureManifest(task.Name, PNG)
	manifest.SetTarget(FailureTarget{Kind: "mbtiles", Path: task.File})
	manifest.Record(maptile.Tile{Z: 0}, template, errors.New("missing tile"))
	report := filepath.Join(dir, "repair.json")
	if err := manifest.Save(report); err != nil {
		t.Fatal(err)
	}
	repair, err := NewRepairTask(task.Layers, task.TileMap, manifest, report)
	if err == nil {
		err = repair.Download()
		repair.Cancel()
	}
	if err == nil {
		t.Error("repair recreated a missing tile table and hid lost dataset tiles")
	}
	after, readErr := os.ReadFile(task.File)
	loaded, loadErr := LoadFailureManifest(report)
	if readErr != nil || loadErr != nil || !bytes.Equal(before, after) || loaded.Len() != 1 || requests.Load() != 1 {
		t.Fatalf("rejected repair changed database/report or fetched data: read=%v load=%v requests=%d", readErr, loadErr, requests.Load())
	}
}

func TestAuditOverlappingLayerSourcesCannotReplaceOrResolveEachOther(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume-%v", kind, resume), func(t *testing.T) {
				configureRegressionOutput(t)
				viper.Set("output.format", kind)
				viper.Set("output.resume", resume)
				body := tileDataPNG(t)
				var secondRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/second/") {
						secondRequests.Add(1)
					}
					w.Write(body)
				}))
				defer server.Close()
				layers := []Layer{
					{Zoom: 0, URL: server.URL + "/first/{z}/{x}/{y}", Collection: orb.Collection{orb.Point{0, 0}}},
					{Zoom: 0, URL: server.URL + "/second/{z}/{x}/{y}", Collection: orb.Collection{orb.Point{0, 0}}},
				}
				task := NewTask(layers, TileMap{Name: "conflict", Format: PNG})
				t.Cleanup(task.Cancel)
				if err := task.Download(); err == nil {
					t.Error("overlapping distinct source layers returned success")
				}
				if secondRequests.Load() != 0 || task.manifest.Len() != 0 {
					t.Fatalf("conflicting source was fetched or recorded as a repairable failure: requests=%d failures=%d", secondRequests.Load(), task.manifest.Len())
				}
				if got := repairIntegrationReadTile(t, kind, task.File, maptile.Tile{Z: 0}); !bytes.Equal(got, body) {
					t.Error("source conflict changed the first source's saved tile")
				}
			})
		}
	}
}

func TestAuditTaskRejectsTileCountOverflow(t *testing.T) {
	configureRegressionOutput(t)
	layers := make([]Layer, 9)
	for i := range layers {
		layers[i] = Layer{Zoom: 30, Collection: orb.Collection{orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}}}
	}
	task := NewTask(layers, TileMap{Name: "overflow", URL: "https://example.test/{z}/{x}/{y}", Format: PNG})
	t.Cleanup(task.Cancel)
	if task.initErr == nil {
		t.Fatalf("overflowing total accepted: %d", task.Total)
	}
}

func TestAuditTaskRejectsExcessiveResourceConfiguration(t *testing.T) {
	for _, key := range []string{"task.workers", "task.savepipe", "task.timedelay"} {
		t.Run(key, func(t *testing.T) {
			configureRegressionOutput(t)
			if key == "task.timedelay" {
				if strconv.IntSize < 64 {
					t.Skip("32-bit delays already fit time.Duration")
				}
				viper.Set(key, int64(math.MaxInt64))
			} else {
				viper.Set(key, 1025)
			}
			task := newRegressionTask(t, "https://example.test/{z}/{x}/{y}")
			if task.initErr == nil {
				t.Fatalf("excessive %s accepted", key)
			}
		})
	}
}

func TestAuditDisjointSourceLayersRemainValidAndReleaseTheirIndex(t *testing.T) {
	configureRegressionOutput(t)
	body := tileDataPNG(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write(body)
	}))
	defer server.Close()
	layers := []Layer{
		{Zoom: 1, URL: server.URL + "/first/{z}/{x}/{y}", Collection: orb.Collection{orb.Point{-90, 45}}},
		{Zoom: 1, URL: server.URL + "/second/{z}/{x}/{y}", Collection: orb.Collection{orb.Point{90, -45}}},
		{Zoom: 1, URL: server.URL + "/empty/{z}/{x}/{y}"},
	}
	task := NewTask(layers, TileMap{Name: "disjoint", Format: PNG})
	t.Cleanup(task.Cancel)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || task.sourceTiles != 0 || len(task.sourceChecks) != 0 {
		t.Fatalf("disjoint source output/index requests=%d tracked=%d zooms=%d", requests.Load(), task.sourceTiles, len(task.sourceChecks))
	}
}

func TestAuditEmptyLayersDoNotEnableSourceIndex(t *testing.T) {
	configureRegressionOutput(t)
	layers := []Layer{
		{Zoom: 0, URL: "https://example.test/first", Collection: orb.Collection{orb.Point{0, 0}}},
		{Zoom: 0, URL: "https://example.test/empty"},
	}
	task := NewTask(layers, TileMap{Name: "empty-layer", Format: PNG})
	t.Cleanup(task.Cancel)
	if task.initErr != nil || len(task.sourceChecks) != 0 {
		t.Fatalf("empty layer enabled source tracking: err=%v zooms=%d", task.initErr, len(task.sourceChecks))
	}
}

func TestAuditEmptyLayersDoNotExpandDownloadZoomMetadata(t *testing.T) {
	configureRegressionOutput(t)
	layers := []Layer{
		{Zoom: 0},
		{Zoom: 5, Collection: orb.Collection{orb.Point{0, 0}}},
		{Zoom: 30},
	}
	task := NewTask(layers, TileMap{Name: "selected-zooms", URL: "https://example.test/{z}/{x}/{y}", Format: PNG})
	t.Cleanup(task.Cancel)
	if task.initErr != nil || task.Min != 5 || task.Max != 5 {
		t.Fatalf("empty layers changed selected zoom metadata: err=%v min=%d max=%d", task.initErr, task.Min, task.Max)
	}
}
