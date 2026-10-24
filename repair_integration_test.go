package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func repairIntegrationLayers(template string) []Layer {
	return []Layer{{Zoom: 1, URL: template, Collection: orb.Collection{orb.MultiPoint{{-90, 45}, {90, -45}}}}}
}

func repairIntegrationMap(template string) TileMap {
	return TileMap{Name: "repair-map", URL: template, Format: PNG}
}

func repairIntegrationReadTile(t *testing.T, kind, path string, tile maptile.Tile) []byte {
	t.Helper()
	if kind == "file" {
		data, err := os.ReadFile(filepath.Join(path, fmt.Sprint(tile.Z), fmt.Sprint(tile.X), fmt.Sprintf("%d.png", tile.Y)))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var data []byte
	if err := db.QueryRow("select tile_data from tiles where zoom_level=? and tile_column=? and tile_row=?", tile.Z, tile.X, (Tile{T: tile}).flipY()).Scan(&data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestResumeAcceptsExistingDatabasePathAlias(t *testing.T) {
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	viper.Set("output.name", "original")
	manifestPath := filepath.Join(dir, "failures.json")
	viper.Set("output.failure_file", manifestPath)
	var available atomic.Bool
	var requests atomic.Int32
	body := tileDataJPEG(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !available.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(body)
	}))
	defer source.Close()
	first := NewTask([]Layer{{Zoom: 0, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{Name: "alias-jpeg", URL: source.URL, Format: "jpeg"})
	t.Cleanup(first.Cancel)
	if err := first.Download(); err == nil {
		t.Fatal("initial unavailable tile was accepted")
	}
	// Legacy MBTiles and failure reports can use either JPEG spelling.
	setLegacyFormat := func() {
		t.Helper()
		db, err := archiveOpenSQLite(first.File, false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("UPDATE metadata SET value='jpeg' WHERE name='format'")
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	setLegacyFormat()
	manifest, err := LoadFailureManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	available.Store(true)
	tm := first.TileMap
	tm.Format = JPG
	repair, err := NewRepairTask(first.Layers, tm, manifest, manifestPath)
	if err != nil {
		t.Fatalf("repair rejected the same JPEG encoding: %v", err)
	}
	t.Cleanup(repair.Cancel)
	if err := repair.Download(); err != nil {
		t.Fatal(err)
	}
	setLegacyFormat()
	alias := filepath.Join(dir, "alias.mbtiles")
	if err := os.Link(first.File, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	viper.Set("output.resume", true)
	viper.Set("output.name", "alias")
	resumed := NewTask(first.Layers, tm)
	t.Cleanup(resumed.Cancel)
	if err := resumed.Download(); err != nil {
		t.Fatalf("resume rejected the same existing database through another name: %v", err)
	}
	if requests.Load() != 2 || !bytes.Equal(repairIntegrationReadTile(t, "mbtiles", first.File, maptile.Tile{Z: 0}), body) {
		t.Fatalf("resume did not repair the original database: requests=%d", requests.Load())
	}
	manifest, err = LoadFailureManifest(manifestPath)
	if err != nil || manifest.Len() != 0 {
		t.Fatalf("resume did not clear the failure report: %v", err)
	}
	if metadata := appendReadMetadata(t, "mbtiles", first.File); metadata["format"] != JPG {
		t.Fatalf("MBTiles JPEG format was not canonicalized: %v", metadata)
	}
	// Equal contents in a different file do not establish target identity.
	copyPath := filepath.Join(dir, "copy.mbtiles")
	encoded, err := os.ReadFile(first.File)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	viper.Set("output.name", "copy")
	different := NewTask(first.Layers, tm)
	t.Cleanup(different.Cancel)
	if err := different.Download(); err == nil || !strings.Contains(err.Error(), "different output target") || requests.Load() != 2 {
		t.Fatalf("different database was accepted as an alias: %v, requests=%d", err, requests.Load())
	}
}

func TestResumeRejectsDatabaseAliasWithActiveSidecar(t *testing.T) {
	_, original := archiveUploadWALDatabase(t)
	alias := filepath.Join(filepath.Dir(original), "alias.mbtiles")
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	for _, paths := range [][2]string{{original, alias}, {alias, original}} {
		if sameFailureTarget(FailureTarget{Kind: "mbtiles", Path: paths[0]}, FailureTarget{Kind: "mbtiles", Path: paths[1]}) {
			t.Fatal("database alias accepted although its other name has a live WAL")
		}
	}
}

func TestRepairAfterConversionAppendKeepsZoomCoverage(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", kind)
			body := tileDataPNG(t)
			var available atomic.Bool
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/2/") && !available.Load() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Write(body)
			}))
			defer source.Close()
			layers := []Layer{
				{Zoom: 0, Collection: orb.Collection{orb.Point{-90, 45}}},
				{Zoom: 2, Collection: orb.Collection{orb.Point{-90, 45}}},
			}
			first := NewTask(layers, TileMap{Name: "world", Format: PNG, URL: source.URL + "/{z}/{x}/{y}"})
			t.Cleanup(first.Cancel)
			if err := first.Download(); err == nil {
				t.Fatal("expected initial missing zoom 2 tile")
			}
			addition := t.TempDir()
			archiveTestWrite(t, filepath.Join(addition, "1", "1", "1.png"), body)
			if _, err := ConvertArchive(context.Background(), addition, first.File, ArchiveConvertOptions{Append: true, Metadata: map[string]string{"maxzoom": "1"}}); err != nil {
				t.Fatal(err)
			}
			manifest, err := LoadFailureManifest(first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			available.Store(true)
			repair, err := NewRepairTask(first.Layers, first.TileMap, manifest, first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(repair.Cancel)
			if err := repair.Download(); err != nil {
				t.Fatal(err)
			}
			if actual := repairIntegrationReadTile(t, kind, first.File, maptile.Tile{Z: 2, X: 1, Y: 1}); !bytes.Equal(actual, body) || manifest.Len() != 0 {
				t.Fatalf("failed tile was not repaired: remaining=%d", manifest.Len())
			}
			metadata := appendReadMetadata(t, kind, first.File)
			if metadata["minzoom"] != "0" || metadata["maxzoom"] != "2" {
				t.Fatalf("append lost the repaired download's declared zoom range: %v", metadata)
			}
		})
	}
}

func TestRepairLocalOutputOnlyRetriesManifestAndPreservesOriginalTarget(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", kind)
			// Keep the original non-resume/random-ID output mode: repair must
			// recover this exact target instead of creating a new output.
			viper.Set("output.resume", false)
			body := tileDataPNG(t)
			var repaired atomic.Bool
			var mu sync.Mutex
			requests := map[string]int{}
			lastToken := map[string]string{}
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests[r.URL.Path]++
				lastToken[r.URL.Path] = r.URL.Query().Get("token")
				mu.Unlock()
				if r.URL.Path == "/1/1/1" && !repaired.Load() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Write(body)
			}))
			defer source.Close()
			template := source.URL + "/{z}/{x}/{y}?style=base&token=original-token"
			first := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
			t.Cleanup(first.Cancel)
			if err := first.Download(); err == nil {
				t.Fatal("initial partial failure returned success")
			}
			if first.Stats() != (DownloadStats{Saved: 1, Failed: 1}) {
				t.Fatalf("initial stats=%+v", first.Stats())
			}
			originalPath := first.File
			if !strings.Contains(filepath.Base(originalPath), first.ID) {
				t.Fatalf("fixture did not exercise a random-ID path: %s", originalPath)
			}
			successful := maptile.Tile{Z: 1, X: 0, Y: 0}
			failed := maptile.Tile{Z: 1, X: 1, Y: 1}
			before := repairIntegrationReadTile(t, kind, originalPath, successful)
			if !bytes.Equal(before, body) {
				t.Fatal("successful tile was not saved before failure")
			}
			if kind == "file" {
				if err := os.WriteFile(filepath.Join(originalPath, "metadata.json"), []byte(`{"bounds":"-180,-85,180,85","minzoom":"0","maxzoom":"18","repair_sentinel":"keep"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(originalPath, "keep.bin"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite3", originalPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("insert into metadata(name,value) values ('repair_sentinel','keep')"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				for name, value := range map[string]string{"bounds": "-180,-85,180,85", "minzoom": "0", "maxzoom": "18"} {
					if _, err := db.Exec("update metadata set value=? where name=?", value, name); err != nil {
						db.Close()
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var originalMetadata []byte
			if kind == "file" {
				var err error
				originalMetadata, err = os.ReadFile(filepath.Join(originalPath, "metadata.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "original-token") {
				t.Fatal("failure report exposed the source token")
			}
			manifest, err := LoadFailureManifest(first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Len() != 1 || manifest.Snapshot().Tiles[0].X != failed.X || manifest.Snapshot().Tiles[0].Y != failed.Y {
				t.Fatalf("unexpected failures=%+v", manifest.Snapshot().Tiles)
			}
			repaired.Store(true)
			current := strings.Replace(template, "original-token", "rotated-token", 1)
			// A repair can narrow the geometry to the failed tile, but must keep
			// the original complete output's bounds and zoom metadata intact.
			repairLayers := []Layer{{Zoom: 1, URL: current, Collection: orb.Collection{orb.Point{90, -45}}}}
			repair, err := NewRepairTask(repairLayers, repairIntegrationMap(current), manifest, first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(repair.Cancel)
			if !repair.resume || repair.File != originalPath || repair.Total != 1 {
				t.Fatalf("repair target/count/resume=%s/%d/%v", repair.File, repair.Total, repair.resume)
			}
			if err := repair.Download(); err != nil {
				t.Fatal(err)
			}
			if repair.Stats() != (DownloadStats{Saved: 1}) {
				t.Fatalf("repair stats=%+v", repair.Stats())
			}
			mu.Lock()
			successRequests, failureRequests, token := requests["/1/0/0"], requests["/1/1/1"], lastToken["/1/1/1"]
			mu.Unlock()
			if successRequests != 1 || failureRequests != 2 || token != "rotated-token" {
				t.Fatalf("requests success/failed/token=%d/%d/%q", successRequests, failureRequests, token)
			}
			if !bytes.Equal(repairIntegrationReadTile(t, kind, originalPath, successful), before) || !bytes.Equal(repairIntegrationReadTile(t, kind, originalPath, failed), body) {
				t.Fatal("repair lost an existing tile or did not fill the missing tile")
			}
			empty, err := LoadFailureManifest(first.failurePath)
			if err != nil || empty.Len() != 0 {
				t.Fatalf("completed repair report=%v, %v", empty, err)
			}
			if kind == "file" {
				data, err := os.ReadFile(filepath.Join(originalPath, "keep.bin"))
				if err != nil || string(data) != "keep" {
					t.Fatal("repair replaced the original output directory")
				}
				metadata, err := os.ReadFile(filepath.Join(originalPath, "metadata.json"))
				if err != nil || !bytes.Equal(metadata, originalMetadata) {
					t.Fatalf("repair replaced original XYZ metadata: %s, %v", metadata, err)
				}
			} else {
				db, err := sql.Open("sqlite3", originalPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var value string
				if err := db.QueryRow("select value from metadata where name='repair_sentinel'").Scan(&value); err != nil || value != "keep" {
					t.Fatalf("repair recreated the database: value=%q err=%v", value, err)
				}
				for name, want := range map[string]string{"bounds": "-180,-85,180,85", "minzoom": "0", "maxzoom": "18"} {
					if err := db.QueryRow("select value from metadata where name=?", name).Scan(&value); err != nil || value != want {
						t.Fatalf("repair replaced original %s: value=%q err=%v", name, value, err)
					}
				}
			}
		})
	}
}

func TestRepairExistingLocalTileClearsFailureWithoutNetwork(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		t.Run(kind, func(t *testing.T) {
			dir := configureRegressionOutput(t)
			viper.Set("output.format", kind)
			body := tileDataPNG(t)
			var requests atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write(body) }))
			defer source.Close()
			template := source.URL + "/{z}/{x}/{y}"
			tile := maptile.Tile{Z: 1, X: 1, Y: 1}
			task := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
			t.Cleanup(task.Cancel)
			if err := task.Download(); err != nil {
				t.Fatal(err)
			}
			manifest := newFailureManifest(task.Name, PNG)
			if err := manifest.SetTarget(FailureTarget{Kind: kind, Path: task.File}); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Record(tile, template, fmt.Errorf("stale failure")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "stale-failures.json")
			if err := manifest.Save(path); err != nil {
				t.Fatal(err)
			}
			before := requests.Load()
			repair, err := NewRepairTask(repairIntegrationLayers(template), repairIntegrationMap(template), manifest, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(repair.Cancel)
			if err := repair.Download(); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != before || repair.Stats() != (DownloadStats{Skipped: 1}) {
				t.Fatalf("existing tile was fetched or counted incorrectly: %+v", repair.Stats())
			}
			loaded, err := LoadFailureManifest(path)
			if err != nil || loaded.Len() != 0 {
				t.Fatalf("stale failure was not resolved: %v", err)
			}
		})
	}
}

func TestRepairS3RetriesFailedPutOrResolvesExistingObject(t *testing.T) {
	for _, alreadyPresent := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-present-%v", alreadyPresent), func(t *testing.T) {
			configureS3Test(t)
			viper.Set("output.format", "s3")
			viper.Set("output.directory", t.TempDir())
			viper.Set("output.s3.prefix", "basemap")
			viper.Set("task.workers", 2)
			viper.Set("task.savepipe", 1)
			viper.Set("task.timeout_seconds", 1)
			viper.Set("task.retries", 0)
			body := tileDataPNG(t)
			var mu sync.Mutex
			sourceRequests := map[string]int{}
			objects := map[string][]byte{}
			putRequests := map[string]int{}
			var repaired atomic.Bool
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sourceRequests[r.URL.Path]++
				mu.Unlock()
				w.Write(body)
			}))
			defer source.Close()
			badKey := "/test-bucket/basemap/1/1/1.png"
			goodKey := "/test-bucket/basemap/1/0/0.png"
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/test-bucket/basemap/metadata.json" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				switch r.Method {
				case http.MethodPut:
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					mu.Lock()
					putRequests[r.URL.Path]++
					mu.Unlock()
					if r.URL.Path == badKey && !repaired.Load() {
						w.Header().Set("Content-Type", "application/xml")
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Denied</Message></Error>`)
						return
					}
					mu.Lock()
					objects[r.URL.Path] = data
					mu.Unlock()
					w.Header().Set("ETag", `"test"`)
				case http.MethodHead:
					mu.Lock()
					data, ok := objects[r.URL.Path]
					mu.Unlock()
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				default:
					t.Errorf("unexpected S3 method %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer destination.Close()
			viper.Set("output.s3.endpoint", destination.URL)
			template := source.URL + "/{z}/{x}/{y}"
			first := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
			t.Cleanup(first.Cancel)
			if err := first.Download(); err == nil {
				t.Fatal("failed PutObject returned success")
			}
			manifest, err := LoadFailureManifest(first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Len() != 1 {
				t.Fatalf("S3 failed tiles=%+v", manifest.Snapshot().Tiles)
			}
			repaired.Store(true)
			if alreadyPresent {
				mu.Lock()
				objects[badKey] = append([]byte(nil), body...)
				mu.Unlock()
			}
			repair, err := NewRepairTask(repairIntegrationLayers(template), repairIntegrationMap(template), manifest, first.failurePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(repair.Cancel)
			if err := repair.Download(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			goodSource, badSource, goodPuts, badPuts := sourceRequests["/1/0/0"], sourceRequests["/1/1/1"], putRequests[goodKey], putRequests[badKey]
			goodObject, badObject := objects[goodKey], objects[badKey]
			mu.Unlock()
			wantBadRequests := 2
			wantStats := DownloadStats{Saved: 1}
			if alreadyPresent {
				wantBadRequests = 1
				wantStats = DownloadStats{Skipped: 1}
			}
			if goodSource != 1 || goodPuts != 1 || badSource != wantBadRequests || badPuts != wantBadRequests {
				t.Fatalf("source/put counts good=%d/%d bad=%d/%d", goodSource, goodPuts, badSource, badPuts)
			}
			if repair.Stats() != wantStats {
				t.Fatalf("repair stats=%+v want %+v", repair.Stats(), wantStats)
			}
			if !bytes.Equal(goodObject, body) || !bytes.Equal(badObject, body) {
				t.Fatal("repair did not preserve/fill the expected S3 objects")
			}
			empty, err := LoadFailureManifest(first.failurePath)
			if err != nil || empty.Len() != 0 {
				t.Fatalf("repair report not cleared: %v", err)
			}
			if first.File != "" || repair.File != "" {
				t.Fatal("S3 repair acquired a local tile destination")
			}
		})
	}
}

func TestRepairRejectsMismatchedConfigurationBeforeNetwork(t *testing.T) {
	configureRegressionOutput(t)
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer source.Close()
	template := source.URL + "/{z}/{x}/{y}?style=base"
	tm := repairIntegrationMap(template)
	layers := repairIntegrationLayers(template)
	for _, test := range []string{"map", "format", "source", "output kind", "outside target", "S3 bucket", "S3 prefix"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			viper.Set("output.directory", dir)
			viper.Set("output.format", "file")
			currentMap := tm
			currentLayers := append([]Layer(nil), layers...)
			manifest := newFailureManifest(tm.Name, PNG)
			target := FailureTarget{Kind: "file", Path: filepath.Join(dir, "original-output")}
			switch test {
			case "map":
				currentMap.Name = "other-map"
			case "format":
				currentMap.Format = JPG
			case "source":
				currentLayers[0].URL = strings.Replace(template, "style=base", "style=other", 1)
			case "output kind":
				viper.Set("output.format", "mbtiles")
			case "outside target":
				target.Path = filepath.Join(filepath.Dir(dir), "outside-output")
			case "S3 bucket", "S3 prefix":
				viper.Set("output.format", "s3")
				viper.Set("output.s3.bucket", "current-bucket")
				viper.Set("output.s3.prefix", "current-prefix")
				fingerprint, err := FailureS3EndpointFingerprint("", "us-east-1")
				if err != nil {
					t.Fatal(err)
				}
				target = FailureTarget{Kind: "s3", Bucket: "current-bucket", Prefix: "current-prefix", EndpointFingerprint: fingerprint}
				if test == "S3 bucket" {
					target.Bucket = "other-bucket"
				} else {
					target.Prefix = "other-prefix"
				}
			}
			if target.Kind == "file" && test != "outside target" {
				if err := os.MkdirAll(target.Path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := manifest.SetTarget(target); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Record(maptile.Tile{Z: 1, X: 1, Y: 1}, template, fmt.Errorf("failed")); err != nil {
				t.Fatal(err)
			}
			task, err := NewRepairTask(currentLayers, currentMap, manifest, filepath.Join(dir, "failures.json"))
			if task != nil {
				task.Cancel()
			}
			if err == nil {
				t.Fatalf("%s mismatch was accepted", test)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("configuration validation made %d HTTP requests", requests.Load())
	}
}

func TestRepairSuccessfulMBTilesLeavesOnlyDatabase(t *testing.T) {
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "mbtiles")
	body := tileDataPNG(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer source.Close()
	template := source.URL + "/{z}/{x}/{y}"
	task := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
	t.Cleanup(task.Cancel)
	if err := task.Download(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || filepath.Join(dir, files[0].Name()) != task.File {
		t.Fatalf("successful MBTiles created extra files: %v", files)
	}
	if _, err := os.Stat(task.failurePath); !os.IsNotExist(err) {
		t.Fatalf("unexpected failure report: %v", err)
	}
}

func TestRepairRejectsMissingOrWrongKindOriginalTarget(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		for _, state := range []string{"missing", "wrong kind", "removed after construction"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				dir := configureRegressionOutput(t)
				viper.Set("output.format", kind)
				var requests atomic.Int32
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
				defer source.Close()
				template := source.URL + "/{z}/{x}/{y}"
				target := filepath.Join(dir, "original")
				if state != "missing" {
					isDirectory := kind == "file"
					if state == "wrong kind" {
						isDirectory = !isDirectory
					}
					if isDirectory {
						if err := os.Mkdir(target, 0700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(target, []byte("original database"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				manifest := newFailureManifest("repair-map", PNG)
				if err := manifest.SetTarget(FailureTarget{Kind: kind, Path: target}); err != nil {
					t.Fatal(err)
				}
				if err := manifest.Record(maptile.Tile{Z: 1, X: 1, Y: 1}, template, fmt.Errorf("failed")); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "failures.json")
				if err := manifest.Save(path); err != nil {
					t.Fatal(err)
				}
				repair, err := NewRepairTask(repairIntegrationLayers(template), repairIntegrationMap(template), manifest, path)
				if state == "removed after construction" {
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(repair.Cancel)
					if err := os.RemoveAll(target); err != nil {
						t.Fatal(err)
					}
					err = repair.Download()
				} else if repair != nil {
					repair.Cancel()
				}
				if err == nil || !strings.Contains(err.Error(), "repair") {
					t.Fatalf("unsafe repair target accepted: %v", err)
				}
				if state != "wrong kind" {
					if _, err := os.Stat(target); !os.IsNotExist(err) {
						t.Fatalf("repair recreated a missing original target: %v", err)
					}
				}
				loaded, err := LoadFailureManifest(path)
				if err != nil || loaded.Len() != 1 || requests.Load() != 0 {
					t.Fatalf("rejected repair changed its report or used the network: %v, requests=%d", err, requests.Load())
				}
			})
		}
	}
}

func TestRepairManifestValidationPrecedesOutputMutation(t *testing.T) {
	for _, kind := range []string{"file", "mbtiles"} {
		for _, invalid := range []string{"extension", "output collision", "directory", "parent file"} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				dir := configureRegressionOutput(t)
				viper.Set("output.format", kind)
				viper.Set("output.resume", false)
				var requests atomic.Int32
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
				defer source.Close()
				template := source.URL + "/{z}/{x}/{y}"
				task := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
				t.Cleanup(task.Cancel)
				task.File = filepath.Join(dir, "original.json")
				protectedPath := task.File
				if kind == "file" {
					if err := os.Mkdir(task.File, 0700); err != nil {
						t.Fatal(err)
					}
					protectedPath = filepath.Join(task.File, "metadata.json")
				}
				original := []byte("original output must survive an invalid report path")
				if err := os.WriteFile(protectedPath, original, 0600); err != nil {
					t.Fatal(err)
				}
				var report string
				switch invalid {
				case "extension":
					report = filepath.Join(dir, "failures.txt")
				case "output collision":
					report = protectedPath
				case "directory":
					report = filepath.Join(dir, "failures.json")
					if err := os.Mkdir(report, 0700); err != nil {
						t.Fatal(err)
					}
				case "parent file":
					parent := filepath.Join(dir, "blocked")
					if err := os.WriteFile(parent, []byte("blocked"), 0600); err != nil {
						t.Fatal(err)
					}
					report = filepath.Join(parent, "failures.json")
				}
				viper.Set("output.failure_file", report)
				if err := task.Download(); err == nil || !strings.Contains(err.Error(), "failure") {
					t.Fatalf("invalid manifest configuration was not rejected before output setup: %v", err)
				}
				after, err := os.ReadFile(protectedPath)
				if err != nil || !bytes.Equal(after, original) || requests.Load() != 0 {
					t.Fatalf("manifest validation mutated the existing output or used the network: %q, %v, requests=%d", after, err, requests.Load())
				}
			})
		}
	}
}

func TestRepairManifestValidationDoesNotCreateOutput(t *testing.T) {
	for _, testcase := range []struct{ kind, report string }{
		{"file", "invalid.txt"}, {"mbtiles", "invalid.txt"},
		{"file", "metadata.json/errors.json"}, {"mbtiles", "errors.json"},
		{"mbtiles", "../tiles.json-wal/errors.json"}, {"mbtiles", "../tiles.json-journal/errors.json"},
		{"compactv2", "conf.xml/errors.json"}, {"compactv2", "CONF.CDI/errors.json"},
		{"compactv2", "_ALLLAYERS/l00/r0000c0000.BUNDLE/errors.json"},
		{"compactv2", "notes.bundle/errors.json"}, {"compactv2", "notes/archive.BUNDLE/errors.json"},
		{"compactv2", "notes.bundle /errors.json"},
		{"compactv2", "."}, {"file", ".."},
	} {
		t.Run(testcase.kind+"/"+testcase.report, func(t *testing.T) {
			dir := configureRegressionOutput(t)
			viper.Set("output.format", testcase.kind)
			template := "https://example.test/{z}/{x}/{y}"
			task := NewTask(repairIntegrationLayers(template), repairIntegrationMap(template))
			t.Cleanup(task.Cancel)
			parent := filepath.Join(dir, "new-output.json")
			task.File = filepath.Join(parent, "tiles.json")
			viper.Set("output.failure_file", filepath.Join(task.File, filepath.FromSlash(testcase.report)))
			if err := task.Download(); err == nil || !strings.Contains(err.Error(), "failure") {
				t.Fatalf("invalid manifest path accepted: %v", err)
			}
			if _, err := os.Stat(parent); !os.IsNotExist(err) {
				t.Fatalf("invalid report configuration created output directories: %v", err)
			}
		})
	}
	for _, testcase := range []struct {
		name, kind, value string
		unixOnly          bool
	}{
		{"s3-ascii", "s3", strings.Repeat("a", maxWindowsComponentUnits), false},
		{"s3-unicode", "s3", strings.Repeat("中", maxWindowsComponentUnits/2), true},
		{"mbtiles-suffix", "mbtiles", strings.Repeat("a", maxWindowsComponentUnits-len(".mbtiles.failures.json")+1), false},
	} {
		t.Run("default-report-name/"+testcase.name, func(t *testing.T) {
			if testcase.unixOnly && runtime.GOOS == "windows" {
				t.Skip("Windows permits this UTF-16 filename length")
			}
			dir := configureTaskTest(t, testcase.kind)
			if testcase.kind == "mbtiles" {
				viper.Set("output.name", testcase.value)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			viper.Set("output.s3.endpoint", server.URL)
			task := NewTask([]Layer{{Zoom: 0, Collection: orb.Collection{orb.Point{0, 0}}}}, TileMap{
				Name: testcase.value, Format: PNG, URL: server.URL,
			})
			t.Cleanup(task.Cancel)
			if err := task.Download(); err == nil || !strings.Contains(err.Error(), "output.failure_file") {
				t.Fatalf("invalid default report filename was not rejected: %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) || requests.Load() != 0 {
				t.Fatalf("invalid report filename created output or used the network: %v, requests=%d", err, requests.Load())
			}
		})
	}
}

func TestRepairRejectsChangedS3EndpointOrRegionBeforeNetwork(t *testing.T) {
	for _, changed := range []string{"endpoint", "region", "environment region", "environment endpoint"} {
		t.Run(changed, func(t *testing.T) {
			configureS3Test(t)
			viper.Set("output.format", "s3")
			var requests atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
			defer destination.Close()
			otherDestination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
			defer otherDestination.Close()
			if changed == "environment endpoint" {
				t.Setenv("AWS_ENDPOINT_URL_S3", destination.URL)
			} else {
				viper.Set("output.s3.endpoint", destination.URL)
			}
			out, err := newS3Output(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			template := destination.URL + "/{z}/{x}/{y}"
			manifest := newFailureManifest("repair-map", PNG)
			if err := manifest.SetTarget(FailureTarget{Kind: "s3", Bucket: out.bucket, Prefix: out.prefix, EndpointFingerprint: out.endpointFingerprint}); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Record(maptile.Tile{Z: 1, X: 1, Y: 1}, template, fmt.Errorf("failed")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(viper.GetString("output.directory"), "failures.json")
			if err := manifest.Save(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(before), destination.URL) || len(out.endpointFingerprint) != 64 {
				t.Fatal("failure report exposed the endpoint instead of its fingerprint")
			}
			switch changed {
			case "endpoint":
				viper.Set("output.s3.endpoint", otherDestination.URL)
			case "region":
				viper.Set("output.s3.region", "us-west-2")
			case "environment region":
				t.Setenv("AWS_REGION", "us-west-2")
			case "environment endpoint":
				t.Setenv("AWS_ENDPOINT_URL_S3", otherDestination.URL)
			}
			repair, err := NewRepairTask(repairIntegrationLayers(template), repairIntegrationMap(template), manifest, path)
			if repair != nil {
				repair.Cancel()
			}
			if err == nil || !strings.Contains(err.Error(), "endpoint and region") {
				t.Fatalf("changed S3 service was accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) || manifest.Len() != 1 || requests.Load() != 0 {
				t.Fatalf("rejected S3 repair changed its report or used the network: %v, requests=%d", err, requests.Load())
			}
		})
	}
}
