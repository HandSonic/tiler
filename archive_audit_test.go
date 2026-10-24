package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"image/color"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAuditArchiveRejectsNullJSONMetadata(t *testing.T) {
	_, err := archiveMergeMetadata(map[string]string{"json": "null", "vector_layers": `[]`}, map[string]string{})
	if err == nil {
		t.Fatal("null vector JSON metadata was accepted")
	}
}

func TestAuditArchiveAliasedOutputCannotDeleteInput(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "maps")
	output := filepath.Join(physical, "tiles")
	input := filepath.Join(output, "source.mbtiles")
	source := filepath.Join(root, "original")
	archiveTestWrite(t, filepath.Join(source, "0", "0", "0.png"), archiveTestPNG(t, color.RGBA{R: 255, A: 255}))
	if _, err := convertLocalArchive(context.Background(), source, input, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertLocalArchive(context.Background(), input, filepath.Join(alias, "tiles"), ArchiveConvertOptions{OutputKind: "file", Overwrite: true}); err == nil {
		t.Fatal("aliased destination containing the source was replaced")
	}
	after, err := os.ReadFile(input)
	if err != nil || string(before) != string(after) {
		t.Fatalf("source was removed or changed through an aliased destination: %v", err)
	}
}

func TestAuditArchivePreservesNarrowBoundsAndRoundsPMOutward(t *testing.T) {
	input := filepath.Join(t.TempDir(), "tiles")
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.RGBA{B: 255, A: 255}))
	archiveTestWrite(t, filepath.Join(input, "metadata.json"), []byte(`{"bounds":"0.000000001,0.000000001,0.000000004,0.000000004"}`))
	mb := filepath.Join(t.TempDir(), "narrow.mbtiles")
	if _, err := convertLocalArchive(context.Background(), input, mb, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	metadata, err := archiveWalkMB(context.Background(), mb, func(int, uint32, uint32, []byte, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := archiveNumbers(metadata["bounds"], 4)
	if err != nil || bounds[0] > 1e-9 || bounds[1] > 1e-9 || bounds[2] < 4e-9 || bounds[3] < 4e-9 {
		t.Fatalf("narrow bounds shrank or collapsed: %v, %v", bounds, err)
	}
	center, err := archiveNumbers(metadata["center"], 3)
	if err != nil || center[0] < bounds[0] || center[0] > bounds[2] || center[1] < bounds[1] || center[1] > bounds[3] {
		t.Fatalf("derived center was rounded outside narrow bounds: %v %v", center, err)
	}
	pm := filepath.Join(t.TempDir(), "narrow.pmtiles")
	if _, err := convertLocalArchive(context.Background(), mb, pm, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	header, _, _ := archiveTestReadPM(t, pm)
	if header.MinLonE7 > 0 || header.MinLatE7 > 0 || header.MaxLonE7 < 1 || header.MaxLatE7 < 1 {
		t.Fatalf("PM E7 bounds round inward: %+v", header)
	}
	merged, err := archiveMergeMetadata(map[string]string{"bounds": "1.000000004,1.000000004,1.000000009,1.000000009"}, map[string]string{"bounds": "2.000000004,2.000000004,2.000000009,2.000000009"})
	if err != nil {
		t.Fatal(err)
	}
	bounds, err = archiveNumbers(merged["bounds"], 4)
	if err != nil || bounds[2] < 2.000000009 || bounds[3] < 2.000000009 || math.IsNaN(bounds[0]) {
		t.Fatalf("merged bounds round inward: %v, %v", bounds, err)
	}
}

func TestAuditArchiveAppendRejectsUnreadableMetadataBeforeTiles(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	archiveTestWrite(t, filepath.Join(first, "1", "0", "0.png"), archiveTestPNG(t, color.RGBA{R: 255, A: 255}))
	archiveTestWrite(t, filepath.Join(second, "1", "1", "1.png"), archiveTestPNG(t, color.RGBA{B: 255, A: 255}))
	destination := filepath.Join(t.TempDir(), "all")
	if _, err := convertLocalArchive(context.Background(), first, destination, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(destination, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []map[string]string{
		{"description": strings.Repeat("\x00", 3<<20)},
		{"bounds": "invalid"},
		{"bounds": "20,10,10,20"},
	} {
		_, err = convertLocalArchive(context.Background(), second, destination, ArchiveConvertOptions{Append: true, Metadata: metadata})
		if err == nil {
			t.Fatal("append accepted invalid or unreadable explicit metadata")
		}
		after, readErr := os.ReadFile(filepath.Join(destination, "metadata.json"))
		if readErr != nil || string(before) != string(after) {
			t.Fatalf("metadata changed before preflight: %v", readErr)
		}
		if _, err := os.Stat(filepath.Join(destination, "1", "1", "1.png")); !os.IsNotExist(err) {
			t.Fatalf("new tile was written before metadata validation: %v", err)
		}
	}
}

func TestAuditArchiveDirectoryMetadataRejectsNullAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "metadata.json")
	archiveTestWrite(t, metadata, []byte("null"))
	if _, err := archiveReadDirectoryMetadata(root); err == nil {
		t.Error("metadata.json null was accepted as an object")
	}
	if runtime.GOOS == "windows" {
		return
	}
	command, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(command, metadata).Run(); err != nil {
		t.Skipf("cannot create test pipe: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := archiveReadDirectoryMetadata(root)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("named-pipe metadata was accepted")
		}
	case <-time.After(3 * time.Second):
		// Release the old implementation's blocking read before failing, so the
		// reproduction itself never leaves a stuck goroutine or filesystem pipe.
		writer, err := os.OpenFile(metadata, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writer.Write([]byte(`{}`))
		writer.Close()
		_ = awaitRegressionError(t, done)
		t.Fatal("metadata reader blocked on an untrusted named pipe")
	}
}

func TestAuditArchiveRejectsOversizedMBMetadata(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE metadata(name TEXT,value TEXT); INSERT INTO metadata VALUES('description',zeroblob(?))", maxMetadataBytes+1); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := archiveReadMBMetadata(context.Background(), tx); err == nil {
		t.Fatal("oversized MBTiles metadata was loaded without a size limit")
	}
}

func TestAuditArchiveRejectsNullCoordinateValues(t *testing.T) {
	for _, text := range []string{`[null,0,1,1]`, `[0,0,null,1]`} {
		if values, err := archiveNumbers(text, 4); err == nil {
			t.Errorf("null coordinate silently became zero: %s => %v", text, values)
		}
	}
}

func TestAuditArchiveTypedOutputCanBeReadAsInput(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		t.Run(kind, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "archive.data")
			if _, err := convertLocalArchive(context.Background(), input, archive, ArchiveConvertOptions{OutputKind: kind}); err != nil {
				t.Fatal(err)
			}
			if _, err := convertLocalArchive(context.Background(), archive, filepath.Join(t.TempDir(), "tiles"), ArchiveConvertOptions{}); err != nil {
				t.Fatalf("converter cannot read its explicitly typed output: %v", err)
			}
		})
	}
}

func TestAuditArchiveAllowsInactivePersistentJournal(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	destination := filepath.Join(t.TempDir(), "output.mbtiles")
	if _, err := convertLocalArchive(context.Background(), input, destination, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	db, err := archiveOpenSQLite(destination, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=PERSIST; INSERT OR REPLACE INTO metadata(name,value) VALUES('description','committed')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(destination + "-journal")
	if err != nil || len(journal) < 8 || string(journal[:8]) != string(make([]byte, 8)) {
		t.Fatalf("expected inactive PERSIST journal: bytes=%d err=%v", len(journal), err)
	}
	if _, err := convertLocalArchive(context.Background(), input, destination, ArchiveConvertOptions{Overwrite: true}); err != nil {
		t.Fatalf("committed inactive journal prevents replacement: %v", err)
	}
}

func TestAuditArchiveGeneratedMetadataRespectsReaderLimits(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	for _, tc := range []struct{ kind, description string }{
		{"mbtiles", strings.Repeat("x", (16<<20)+1)},
		{"pmtiles", strings.Repeat("\x00", 3<<20)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "out."+tc.kind)
			_, err := convertLocalArchive(context.Background(), input, output, ArchiveConvertOptions{Metadata: map[string]string{"description": tc.description}})
			if err == nil || !strings.Contains(err.Error(), "16 MiB") {
				t.Fatalf("oversized generated metadata was not rejected: %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("unreadable archive was published: %v", err)
			}
		})
	}
}

func TestAuditArchiveMBVectorMetadataNormalizationAndAppend(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	raw := []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
	archiveTestWrite(t, filepath.Join(first, "5", "0", "0.pbf"), raw)
	archiveTestWrite(t, filepath.Join(first, "metadata.json"), []byte(`{"vector_layers":[{"id":"test","fields":{"a":"Number"},"minzoom":0,"maxzoom":14}]}`))
	archiveTestWrite(t, filepath.Join(second, "6", "0", "0.pbf"), raw)
	archiveTestWrite(t, filepath.Join(second, "metadata.json"), []byte(`{"vector_layers":[{"id":"test","fields":{"b":"String"},"minzoom":0,"maxzoom":14}]}`))
	output := filepath.Join(t.TempDir(), "all.mbtiles")
	for i, input := range []string{first, second} {
		if _, err := convertLocalArchive(context.Background(), input, output, ArchiveConvertOptions{Append: i > 0}); err != nil {
			t.Fatal(err)
		}
		metadata, err := archiveWalkMB(context.Background(), output, func(int, uint32, uint32, []byte, string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]interface{}
		if err := json.Unmarshal([]byte(metadata["json"]), &object); err != nil {
			t.Fatalf("required metadata json entry missing: %v", err)
		}
		layers, ok := object["vector_layers"].([]interface{})
		if !ok || len(layers) != 1 {
			t.Fatalf("required vector layer schema lost: %v", object)
		}
		layer := layers[0].(map[string]interface{})
		if layer["minzoom"] != float64(5) || layer["maxzoom"] != float64(5+i) {
			t.Fatalf("layer zooms do not describe the exported set: %v", layer)
		}
		fields := layer["fields"].(map[string]interface{})
		if fields["a"] != "Number" || (i > 0 && fields["b"] != "String") {
			t.Fatalf("appending a region lost its vector fields: %v", fields)
		}
	}
}

func TestAuditArchiveRejectsKindAndKnownExtensionConflict(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	for _, tc := range []struct{ kind, extension string }{{"mbtiles", "pmtiles"}, {"pmtiles", "mbtiles"}} {
		t.Run(tc.kind, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "out."+tc.extension)
			if _, err := convertLocalArchive(context.Background(), input, output, ArchiveConvertOptions{OutputKind: tc.kind}); err == nil {
				t.Fatal("explicit archive type contradicted the filename used to detect it on input")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("conflicting archive was published: %v", err)
			}
		})
	}
}

func TestAuditArchivePMOverwriteProtectsExistingSQLiteWAL(t *testing.T) {
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.White))
	mb := filepath.Join(t.TempDir(), "source.mbtiles")
	if _, err := convertLocalArchive(context.Background(), input, mb, ArchiveConvertOptions{}); err != nil {
		t.Fatal(err)
	}
	// Existing content can have an unexpected name even after conflicting
	// output types are rejected: external tools may rename an archive.
	destination := filepath.Join(t.TempDir(), "existing.pmtiles")
	if err := os.Rename(mb, destination); err != nil {
		t.Fatal(err)
	}
	db, err := archiveOpenSQLite(destination, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO metadata(name,value) VALUES('live','wal-only')"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	_, err = convertLocalArchive(context.Background(), input, destination, ArchiveConvertOptions{Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("PMTiles replacement ignored an existing SQLite WAL: %v", err)
	}
	after, readErr := os.ReadFile(destination)
	if readErr != nil || string(before) != string(after) {
		t.Fatalf("SQLite destination changed before WAL rejection: %v", readErr)
	}
}

func TestAuditArchiveMergedLayerKeepsUnspecifiedZoomBounds(t *testing.T) {
	unbounded := map[string]string{"json": `{"vector_layers":[{"id":"test","fields":{}}]}`}
	bounded := map[string]string{"json": `{"vector_layers":[{"id":"test","fields":{},"minzoom":5,"maxzoom":5}]}`}
	for _, pair := range [][2]map[string]string{{unbounded, bounded}, {bounded, unbounded}} {
		merged, err := archiveMergeMetadata(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		var layers []map[string]interface{}
		if err := json.Unmarshal([]byte(merged["vector_layers"]), &layers); err != nil {
			t.Fatal(err)
		}
		if _, exists := layers[0]["minzoom"]; exists {
			t.Errorf("unbounded source layer was narrowed to incoming zooms: %v", layers[0])
		}
		if _, exists := layers[0]["maxzoom"]; exists {
			t.Errorf("unbounded source layer was narrowed to incoming zooms: %v", layers[0])
		}
	}
}

func TestAuditArchiveMetadataSizeCountsNULLAdjacentValues(t *testing.T) {
	for _, values := range []string{"NULL,zeroblob(?)", "zeroblob(?),NULL"} {
		t.Run(values, func(t *testing.T) {
			db, err := sql.Open("sqlite3", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec("CREATE TABLE metadata(name TEXT,value TEXT); INSERT INTO metadata VALUES("+values+")", maxMetadataBytes+1); err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := archiveReadMBMetadata(context.Background(), tx); err == nil || !strings.Contains(err.Error(), "exceeds 16 MiB") {
				t.Fatalf("oversized metadata must be rejected before loading values: %v", err)
			}
		})
	}
}
