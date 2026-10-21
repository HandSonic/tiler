package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func TestAuditS3RejectsPrefixesThatChangeObjectKeys(t *testing.T) {
	for _, prefix := range []string{"maps/../other", "maps/./other", "maps//other", `maps\other`} {
		t.Run(prefix, func(t *testing.T) {
			configureS3Test(t)
			viper.Set("output.s3.prefix", prefix)
			if _, err := newS3Output(context.Background()); err == nil {
				t.Fatal("invalid prefix would be silently rewritten by path.Join")
			}
		})
	}
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"uploaded"`)
	})
	out.prefix = "archives/../elsewhere"
	filename := archiveUploadFixture(t, filepath.Join(t.TempDir(), "map.pmtiles"), "archive")
	if _, err := UploadArchives(context.Background(), out, []string{filename}, ArchiveUploadOptions{}); err == nil || requests.Load() != 0 {
		t.Fatalf("raw upload prefix validation failed: error=%v requests=%d", err, requests.Load())
	}
}

func TestAuditUploadRejectsActiveRollbackJournal(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "active.mbtiles")
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=DELETE; PRAGMA cache_size=5; CREATE TABLE data(body BLOB); INSERT INTO data VALUES(zeroblob(1048576));"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE data SET body=randomblob(1048576)"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filename + "-journal")
	if err != nil || len(journal) < 8 || string(journal[:8]) == string(make([]byte, 8)) {
		t.Fatalf("expected a spilled rollback journal: bytes=%d err=%v", len(journal), err)
	}
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"uploaded"`)
	})
	if _, err := UploadArchives(context.Background(), out, []string{filename}, ArchiveUploadOptions{}); err == nil || requests.Load() != 0 {
		t.Fatalf("uploaded an incomplete rollback-mode database: err=%v requests=%d", err, requests.Load())
	}
}

func TestAuditUploadAllowsInactivePersistentJournal(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "committed.mbtiles")
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=PERSIST; CREATE TABLE data(value INTEGER); INSERT INTO data VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filename + "-journal")
	if err != nil || len(journal) < 8 || string(journal[:8]) != string(make([]byte, 8)) {
		t.Fatalf("expected inactive persistent journal: %d %v", len(journal), err)
	}
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"uploaded"`)
	})
	if _, err := UploadArchives(context.Background(), out, []string{filename}, ArchiveUploadOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestAuditS3AppendValidatesEveryExistingTileIdentity(t *testing.T) {
	store, out := archiveS3Store(t)
	body := tileDataPNG(t)
	identity := func(pattern, xyz string) map[string]string {
		return map[string]string{"tiler-path": fmt.Sprintf("%x", sha256.Sum256([]byte(pattern))), "tiler-xyz": xyz, "tiler-format": "png"}
	}
	store.objects["dest/tiles/2/0/0.png"] = archiveS3TestObject{body: body, metadata: identity(defaultTilePath, "2/0/0")}
	store.objects["dest/tiles/2/1/2.png"] = archiveS3TestObject{body: body, metadata: identity("{z}/{y}/{x}.{format}", "2/2/1")}
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "3", "4", "4.png"), body)
	_, err := ConvertArchive(context.Background(), input, "s3://dest/tiles/", ArchiveConvertOptions{S3: out, Append: true})
	if err == nil || (!strings.Contains(err.Error(), "tile_path") && !strings.Contains(err.Error(), "layout")) || store.count(http.MethodPut) != 0 {
		t.Fatalf("mixed existing tile identities changed dataset: err=%v puts=%d", err, store.count(http.MethodPut))
	}
}

func TestAuditS3ImportRejectsObjectChangedSinceListing(t *testing.T) {
	body := tileDataPNG(t)
	var match string
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("list-type") == "2":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<ListBucketResult><Contents><Key>tiles/0/0/0.png</Key><Size>%d</Size><ETag>"before"</ETag></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`, len(body))
		case r.URL.Path == "/source/tiles/metadata.json":
			testS3Error(w, http.StatusNotFound, "NoSuchKey")
		case r.URL.Path == "/source/tiles/0/0/0.png":
			match = r.Header.Get("If-Match")
			if match == `"before"` {
				testS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
			w.Header().Set("ETag", `"after"`)
			w.Write(body)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	destination := filepath.Join(t.TempDir(), "snapshot.mbtiles")
	_, err := ConvertArchive(context.Background(), "s3://source/tiles/", destination, ArchiveConvertOptions{S3: out})
	if err == nil || match != `"before"` {
		t.Fatalf("import accepted a different object version: error=%v If-Match=%q", err, match)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("source changed but a destination archive was published")
	}
}

func TestAuditS3MergePreservesNarrowMetadataBounds(t *testing.T) {
	before := archiveExtent{count: 1, minZ: 30, maxZ: 30, west: 1.000000011, south: 2.000000011, east: 1.000000019, north: 2.000000019}
	added := archiveExtent{count: 1, minZ: 30, maxZ: 30, west: 1.000000131, south: 2.000000131, east: 1.000000139, north: 2.000000139}
	metadata, err := archiveS3MergeMetadata(nil, nil, before, added, "png")
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := archiveNumbers(metadata["bounds"], 4)
	if err != nil {
		t.Fatal(err)
	}
	if bounds[0] != before.west || bounds[1] != before.south || bounds[2] != added.east || bounds[3] != added.north {
		t.Fatalf("S3 append rounded dataset bounds: %s", metadata["bounds"])
	}
}

func TestAuditS3VersionedReadChecksReturnedETag(t *testing.T) {
	for _, etag := range []string{`"before"`, `"after"`} {
		t.Run(etag, func(t *testing.T) {
			out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
				if match := r.Header.Get("If-Match"); match != `"before"` {
					t.Errorf("If-Match=%q", match)
				}
				// Even a noncompliant S3-compatible endpoint that ignores the
				// condition must not substitute a different returned version.
				w.Header().Set("ETag", etag)
				io.WriteString(w, "same bytes")
			})
			body, _, _, err := archiveS3ReadObjectVersion(context.Background(), out, "tile", 1024, `"before"`)
			if etag == `"before"` {
				if err != nil || string(body) != "same bytes" {
					t.Fatalf("stable object read failed: %q %v", body, err)
				}
			} else if err == nil {
				t.Fatal("ignored mismatched returned version")
			}
		})
	}
}

func TestAuditS3EncodedReadBoundsChunkedGzip(t *testing.T) {
	packed, err := archiveGzip([]byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	const limit = 20 // decoded payload fits; its gzip representation does not.
	if len(packed) <= limit {
		t.Fatal("gzip fixture must exceed the encoded budget")
	}
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.(http.Flusher).Flush() // Send chunked data without Content-Length.
		w.Write(packed)
	})
	if _, _, _, err := archiveS3ReadObjectVersion(context.Background(), out, "tile", limit, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("chunked gzip bypassed the encoded budget: %v", err)
	}
}

func TestAuditS3IdentityAllowsJPEGEncodingAlias(t *testing.T) {
	out := &s3Output{tilePath: "{z}/{x}/{y}", allowUnmarkedTiles: true}
	if err := out.validateTileIdentity(map[string]string{"tiler-format": "JPEG"}, maptile.Tile{Z: 0}, "jpg"); err != nil {
		t.Fatalf("JPEG alias rejected for extensionless tile: %v", err)
	}
	if err := out.validateTileIdentity(map[string]string{"tiler-format": "png"}, maptile.Tile{Z: 0}, "jpg"); err == nil {
		t.Fatal("different encodings accepted")
	}
}

func TestAuditS3ArchivesWithExplicitKindCanBeReadBack(t *testing.T) {
	for _, kind := range []string{"mbtiles", "pmtiles"} {
		t.Run(kind, func(t *testing.T) {
			store, out := archiveS3Store(t)
			input := t.TempDir()
			body := tileDataPNG(t)
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), body)
			location := "s3://maps/archive.data"
			if _, err := ConvertArchive(context.Background(), input, location, ArchiveConvertOptions{S3: out, OutputKind: kind}); err != nil {
				t.Fatal(err)
			}
			if kind == "mbtiles" {
				packed, err := archiveGzip(store.get("maps/archive.data"))
				if err != nil {
					t.Fatal(err)
				}
				store.putEncoded("maps/archive.data", packed, "gzip")
			}
			filename := filepath.Join(t.TempDir(), "roundtrip.mbtiles")
			if _, err := ConvertArchive(context.Background(), location, filename, ArchiveConvertOptions{S3: out}); err != nil {
				t.Fatalf("explicit %s output cannot be read back (%d objects): %v", kind, len(store.objects), err)
			}
			if got := archiveS3ReadMBTile(t, filename, 0, 0, 0); string(got) != string(body) {
				t.Fatal("archive tile changed")
			}
		})
	}
}

func TestAuditS3AppendRejectsMixedUnmarkedExtensionlessTiles(t *testing.T) {
	store, out := archiveS3Store(t)
	store.put("dest/tiles/metadata.json", []byte(`{"tile_path":"{z}/{x}/{y}"}`))
	store.put("dest/tiles/2/0/0", tileDataPNG(t))
	store.put("dest/tiles/2/1/1", tileDataJPEG(t))
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "2", "2", "2.png"), tileDataPNG(t))
	_, err := ConvertArchive(context.Background(), input, "s3://dest/tiles/", ArchiveConvertOptions{S3: out, Append: true, OutputTilePath: "{z}/{x}/{y}"})
	if err == nil || !strings.Contains(err.Error(), "format") || store.count(http.MethodPut) != 0 {
		t.Fatalf("assigned PNG metadata to a retained JPEG tile: err=%v puts=%d", err, store.count(http.MethodPut))
	}
}

func TestAuditS3RejectsArchiveKindSuffixMismatchBeforeRequests(t *testing.T) {
	for _, pair := range [][2]string{{"mbtiles", "wrong.pmtiles"}, {"pmtiles", "wrong.mbtiles"}} {
		t.Run(pair[0], func(t *testing.T) {
			store, out := archiveS3Store(t)
			input := t.TempDir()
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), tileDataPNG(t))
			_, err := ConvertArchive(context.Background(), input, "s3://maps/"+pair[1], ArchiveConvertOptions{S3: out, OutputKind: pair[0]})
			if err == nil || store.totalRequests() != 0 {
				t.Fatalf("conflicting kind was not rejected before S3 requests: err=%v requests=%d", err, store.totalRequests())
			}
		})
	}
}
