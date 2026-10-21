package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/paulmach/orb/maptile"
)

type archiveS3TestObject struct {
	body     []byte
	metadata map[string]string
	encoding string
}

type archiveS3TestStore struct {
	mu       sync.Mutex
	objects  map[string]archiveS3TestObject
	requests []string
	failures map[string]int
	pageSize int
}

func (store *archiveS3TestStore) serve(w http.ResponseWriter, r *http.Request) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/")
	store.requests = append(store.requests, r.Method+" "+key)
	if status := store.failures[r.Method+" "+key]; status != 0 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>test rejection</Message></Error>")
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		prefix := key + "/" + r.URL.Query().Get("prefix")
		var keys []string
		for candidate := range store.objects {
			if strings.HasPrefix(candidate, prefix) {
				keys = append(keys, candidate)
			}
		}
		sort.Strings(keys)
		start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
		if start < 0 || start > len(keys) {
			testS3Error(w, http.StatusBadRequest, "InvalidToken")
			return
		}
		end := len(keys)
		if store.pageSize > 0 && end > start+store.pageSize {
			end = start + store.pageSize
		}
		type item struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
			ETag string `xml:"ETag"`
		}
		response := struct {
			XMLName xml.Name `xml:"ListBucketResult"`
			Items   []item   `xml:"Contents"`
			More    bool     `xml:"IsTruncated"`
			Next    string   `xml:"NextContinuationToken,omitempty"`
		}{More: end < len(keys)}
		if response.More {
			response.Next = strconv.Itoa(end)
		}
		for _, name := range keys[start:end] {
			response.Items = append(response.Items, item{Key: strings.TrimPrefix(name, key+"/"), Size: len(store.objects[name].body), ETag: `"test-etag"`})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(response)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		object, ok := store.objects[key]
		if !ok {
			testS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && match != `"test-etag"` {
			testS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		w.Header().Set("ETag", "\"test-etag\"")
		if object.encoding != "" {
			w.Header().Set("Content-Encoding", object.encoding)
		}
		for name, value := range object.metadata {
			w.Header().Set("X-Amz-Meta-"+name, value)
		}
		if r.Method == http.MethodGet {
			w.Write(object.body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			testS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		metadata := map[string]string{}
		for name, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
				metadata[strings.TrimPrefix(strings.ToLower(name), "x-amz-meta-")] = values[0]
			}
		}
		store.objects[key] = archiveS3TestObject{body: body, metadata: metadata, encoding: r.Header.Get("Content-Encoding")}
		w.Header().Set("ETag", "\"test-etag\"")
	default:
		testS3Error(w, http.StatusBadRequest, "UnexpectedMethod")
	}
}

func testS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
}

func archiveS3Store(t *testing.T) (*archiveS3TestStore, *s3Output) {
	t.Helper()
	store := &archiveS3TestStore{objects: map[string]archiveS3TestObject{}, failures: map[string]int{}, pageSize: 2}
	return store, archiveUploadTestOutput(t, store.serve)
}

func (store *archiveS3TestStore) put(key string, body []byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = archiveS3TestObject{body: append([]byte(nil), body...)}
}

func (store *archiveS3TestStore) putEncoded(key string, body []byte, encoding string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = archiveS3TestObject{body: append([]byte(nil), body...), encoding: encoding}
}

func (store *archiveS3TestStore) get(key string) []byte {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]byte(nil), store.objects[key].body...)
}

func (store *archiveS3TestStore) count(method string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for _, request := range store.requests {
		if strings.HasPrefix(request, method+" ") {
			count++
		}
	}
	return count
}

func (store *archiveS3TestStore) fail(request string, status int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failures[request] = status
}

func (store *archiveS3TestStore) totalRequests() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.requests)
}

func archiveS3ReadMBTile(t *testing.T, filename string, z, x, y int) []byte {
	t.Helper()
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var body []byte
	if err := db.QueryRow("SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?", z, x, (1<<uint(z))-1-y).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestS3ConversionLooseRoundTripWithPaginationAndCustomLayouts(t *testing.T) {
	store, out := archiveS3Store(t)
	red := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
	blue := archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	store.put("source/region/2/2/1.png", red)
	store.put("source/region/2/1/2.png", blue)
	store.put("source/region/metadata.json", []byte(`{"format":"png","tile_path":"{z}/{y}/{x}.{format}","name":"source-region","bounds":[-180,-85,180,85],"center":[0,0,2]}`))
	store.put("source/region/README.txt", []byte("not a tile"))
	root := t.TempDir()
	mbtiles := filepath.Join(root, "local.mbtiles")
	result, err := ConvertArchive(context.Background(), "s3://source/region/", mbtiles, ArchiveConvertOptions{S3: out})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tiles != 2 || !bytes.Equal(archiveS3ReadMBTile(t, mbtiles, 2, 1, 2), red) || !bytes.Equal(archiveS3ReadMBTile(t, mbtiles, 2, 2, 1), blue) {
		t.Fatalf("incorrect S3->MBTiles result %+v", result)
	}
	result, err = ConvertArchive(context.Background(), mbtiles, "s3://destination/published/", ArchiveConvertOptions{S3: out, OutputTilePath: "tiles/{z}/{x}/{-y}.{format}"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tiles != 2 || !bytes.Equal(store.get("destination/published/tiles/2/1/1.png"), red) || !bytes.Equal(store.get("destination/published/tiles/2/2/2.png"), blue) {
		t.Fatalf("incorrect MBTiles->S3 result %+v", result)
	}
	var metadata map[string]string
	if err := json.Unmarshal(store.get("destination/published/metadata.json"), &metadata); err != nil || metadata["tile_path"] != "tiles/{z}/{x}/{-y}.{format}" || metadata["scheme"] != "tms" {
		t.Fatalf("S3 metadata = %+v, %v", metadata, err)
	}
	pmtiles := filepath.Join(root, "from-s3.pmtiles")
	if _, err := ConvertArchive(context.Background(), "s3://destination/published/", pmtiles, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(root, "files")
	if _, err := ConvertArchive(context.Background(), pmtiles, files, ArchiveConvertOptions{OutputKind: "file", OutputTilePath: "{z}_{y}_{x}.{format}"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(files, "2_2_1.png"))
	if err != nil || !bytes.Equal(got, red) {
		t.Fatalf("PMTiles->files output mismatch %v", err)
	}
	if store.count(http.MethodGet) < 8 {
		t.Fatal("test did not exercise paginated S3 reads")
	}
}

func TestS3ConversionChecksDirectDownloadMarkers(t *testing.T) {
	_, out := archiveS3Store(t)
	out.bucket, out.prefix, out.tilePath = "tiles", "direct", "{z}/{y}/{x}.{format}"
	tile := maptile.Tile{Z: 2, X: 3, Y: 1}
	body := tileDataPNG(t)
	if err := out.saveContext(context.Background(), Tile{T: tile, C: body}, PNG); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "checked.mbtiles")
	if _, err := ConvertArchive(context.Background(), "s3://tiles/direct/", destination, ArchiveConvertOptions{S3: out}); err == nil || !strings.Contains(err.Error(), "input-tile-path") {
		t.Fatalf("custom layout was silently read as XYZ: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("mismatched source created a destination archive")
	}
	if _, err := ConvertArchive(context.Background(), "s3://tiles/direct/", destination, ArchiveConvertOptions{S3: out, InputTilePath: out.tilePath}); err != nil {
		t.Fatal(err)
	}
	if got := archiveS3ReadMBTile(t, destination, 2, 3, 1); !bytes.Equal(got, body) {
		t.Fatal("correctly configured direct S3 tile changed coordinates or bytes")
	}
}

func TestS3ConversionArchiveExactKeyAndAppend(t *testing.T) {
	store, out := archiveS3Store(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	red, blue := archiveTestPNG(t, color.RGBA{R: 255, A: 255}), archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	archiveTestWrite(t, filepath.Join(a, "2", "0", "1.png"), red)
	archiveTestWrite(t, filepath.Join(b, "2", "0", "1.png"), blue)
	archiveTestWrite(t, filepath.Join(b, "3", "6", "5.png"), blue)
	const destination = "s3://maps/exact/final-name.pmtiles"
	if _, err := ConvertArchive(context.Background(), a, destination, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	first := store.get("maps/exact/final-name.pmtiles")
	if len(first) == 0 || len(store.get("maps/exact/final-name.pmtiles/s3-output.pmtiles")) != 0 {
		t.Fatal("archive was not uploaded to the exact requested key")
	}
	compressed, err := archiveGzip(first)
	if err != nil {
		t.Fatal(err)
	}
	store.putEncoded("maps/exact/final-name.pmtiles", compressed, "gzip")
	before := store.count(http.MethodPut)
	if _, err := ConvertArchive(context.Background(), b, destination, ArchiveConvertOptions{S3: out}); err == nil || store.count(http.MethodPut) != before {
		t.Fatal("existing remote archive was overwritten without an explicit option")
	}
	result, err := ConvertArchive(context.Background(), b, destination, ArchiveConvertOptions{S3: out, Append: true})
	if err != nil || result.Tiles != 2 {
		t.Fatalf("append result %+v: %v", result, err)
	}
	local := filepath.Join(root, "merged.mbtiles")
	if _, err := ConvertArchive(context.Background(), destination, local, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archiveS3ReadMBTile(t, local, 2, 0, 1), red) || !bytes.Equal(archiveS3ReadMBTile(t, local, 3, 6, 5), blue) {
		t.Fatal("append replaced region A or omitted region B")
	}
	if _, err := ConvertArchive(context.Background(), destination, "s3://second/archives/merged.mbtiles", ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(root, "remote-copy.mbtiles")
	archiveTestWrite(t, copy, store.get("second/archives/merged.mbtiles"))
	if !bytes.Equal(archiveS3ReadMBTile(t, copy, 2, 0, 1), red) {
		t.Fatal("S3->S3 archive conversion lost tiles")
	}
}

func TestS3ConversionGzipContentEncoding(t *testing.T) {
	store, out := archiveS3Store(t)
	body := tileDataPNG(t)
	compressed, err := archiveGzip(body)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := archiveGzip([]byte(`{"name":"compressed input","format":"png"}`))
	if err != nil {
		t.Fatal(err)
	}
	store.putEncoded("source/tiles/metadata.json", metadata, "gzip")
	store.putEncoded("source/tiles/0/0/0.png", compressed, "gzip")
	output := filepath.Join(t.TempDir(), "out.mbtiles")
	if _, err := ConvertArchive(context.Background(), "s3://source/tiles/", output, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	if got := archiveS3ReadMBTile(t, output, 0, 0, 0); !bytes.Equal(got, body) {
		t.Fatal("HTTP gzip changed the raster tile")
	}
	corrupt := append([]byte(nil), compressed...)
	corrupt[len(corrupt)-8] ^= 0xff
	for _, bad := range []struct {
		body          []byte
		encoding, err string
	}{{corrupt, "gzip", "checksum"}, {compressed[:len(compressed)-4], "gzip", "unexpected EOF"}, {body, "br", "unsupported"}} {
		store.putEncoded("source/tiles/0/0/0.png", bad.body, bad.encoding)
		if _, err := ConvertArchive(context.Background(), "s3://source/tiles/", output, ArchiveConvertOptions{S3: out, Overwrite: true}); err == nil || !strings.Contains(err.Error(), bad.err) {
			t.Fatalf("invalid encoded response was accepted (%s): %v", bad.err, err)
		}
		if got := archiveS3ReadMBTile(t, output, 0, 0, 0); !bytes.Equal(got, body) {
			t.Fatal("failed encoded read replaced the previous archive")
		}
	}
	large, err := archiveGzip([]byte(strings.Repeat("a", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	store.putEncoded(out.bucket+"/limited", large, "gzip")
	if _, _, _, err := archiveS3ReadObjectVersion(context.Background(), out, "limited", 128, ""); err == nil || !strings.Contains(err.Error(), "exceeds 128") {
		t.Fatalf("decoded response bypassed its size budget: %v", err)
	}
}

func TestS3ConversionLooseAppendPreservesOverlapAndRejectsLayoutChange(t *testing.T) {
	store, out := archiveS3Store(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	red, blue := archiveTestPNG(t, color.RGBA{R: 255, A: 255}), archiveTestPNG(t, color.RGBA{B: 255, A: 255})
	archiveTestWrite(t, filepath.Join(a, "2", "0", "1.png"), red)
	archiveTestWrite(t, filepath.Join(b, "2", "0", "1.png"), blue)
	archiveTestWrite(t, filepath.Join(b, "3", "6", "5.png"), blue)
	const prefix = "s3://maps/tiles/"
	if _, err := ConvertArchive(context.Background(), a, prefix, ArchiveConvertOptions{S3: out, OutputTilePath: "{z}/{y}/{x}.{format}"}); err != nil {
		t.Fatal(err)
	}
	result, err := ConvertArchive(context.Background(), b, prefix, ArchiveConvertOptions{S3: out, Append: true, OutputTilePath: "{z}/{y}/{x}.{format}"})
	if err != nil || result.Tiles != 2 {
		t.Fatalf("append result %+v: %v", result, err)
	}
	if !bytes.Equal(store.get("maps/tiles/2/1/0.png"), red) || !bytes.Equal(store.get("maps/tiles/3/5/6.png"), blue) {
		t.Fatal("loose append did not preserve original overlap")
	}
	var metadata map[string]string
	if err := json.Unmarshal(store.get("maps/tiles/metadata.json"), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["minzoom"] != "2" || metadata["maxzoom"] != "3" {
		t.Fatalf("append metadata zoom range was narrowed: %+v", metadata)
	}
	before := store.count(http.MethodPut)
	if _, err := ConvertArchive(context.Background(), b, prefix, ArchiveConvertOptions{S3: out, Append: true}); err == nil || store.count(http.MethodPut) != before {
		t.Fatal("changed layout was not rejected before writing")
	}
	if _, err := ConvertArchive(context.Background(), b, prefix, ArchiveConvertOptions{S3: out, Append: true, OutputTilePath: "{z}/{y}/{x}.{format}", Format: "jpg"}); err == nil || store.count(http.MethodPut) != before {
		t.Fatal("changed tile format was not rejected before writing")
	}
}

func TestS3ConversionErrorsPreserveDestinationAndCleanScratch(t *testing.T) {
	store, out := archiveS3Store(t)
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	input := filepath.Join(root, "source")
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), archiveTestPNG(t, color.RGBA{R: 255, A: 255}))
	store.put("maps/existing.mbtiles", []byte("original remote data"))
	store.fail("HEAD maps/existing.mbtiles", http.StatusForbidden)
	if _, err := ConvertArchive(context.Background(), input, "s3://maps/existing.mbtiles", ArchiveConvertOptions{S3: out, Append: true}); err == nil {
		t.Fatal("forbidden destination was treated as missing")
	}
	if !bytes.Equal(store.get("maps/existing.mbtiles"), []byte("original remote data")) || store.count(http.MethodPut) != 0 {
		t.Fatal("read failure changed the original remote archive")
	}
	store.fail("PUT maps/new.pmtiles", http.StatusForbidden)
	if _, err := ConvertArchive(context.Background(), input, "s3://maps/new.pmtiles", ArchiveConvertOptions{S3: out}); err == nil {
		t.Fatal("failed upload returned success")
	}
	if len(store.get("maps/new.pmtiles")) != 0 {
		t.Fatal("failed upload published a partial archive")
	}
	store.fail("GET maps/private.pmtiles", http.StatusForbidden)
	privateOutput := filepath.Join(root, "private.mbtiles")
	if _, err := ConvertArchive(context.Background(), "s3://maps/private.pmtiles", privateOutput, ArchiveConvertOptions{S3: out}); err == nil {
		t.Fatal("forbidden source returned success")
	}
	if _, err := os.Stat(privateOutput); !os.IsNotExist(err) {
		t.Fatal("failed remote input created a local destination")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("conversion scratch was not cleaned: %v, %v", entries, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := store.totalRequests()
	if _, err := ConvertArchive(ctx, input, "s3://maps/canceled.mbtiles", ArchiveConvertOptions{S3: out}); err == nil || store.totalRequests() != before {
		t.Fatal("canceled conversion made network requests")
	}
}

func TestS3ConversionPBFAndExtensionlessTiles(t *testing.T) {
	store, out := archiveS3Store(t)
	vector := []byte{0x1a, 0x0b, 0x0a, 0x04, 't', 'e', 's', 't', 0x28, 0x80, 0x20, 0x78, 0x02}
	packed, err := archiveGzip(vector)
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]string{"vector_layers": `[{"id":"test","fields":{}}]`}
	t.Run("legacy gzip source", func(t *testing.T) {
		store.putEncoded("source/legacy/0/0/0", packed, "gzip")
		output := filepath.Join(t.TempDir(), "legacy.mbtiles")
		if _, err := ConvertArchive(context.Background(), "s3://source/legacy/", output, ArchiveConvertOptions{S3: out, InputTilePath: "{z}/{x}/{y}", Metadata: layers}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("legacy retained gzip", func(t *testing.T) {
		store.putEncoded("maps/legacy/0/0/0", packed, "gzip")
		store.put("maps/legacy/metadata.json", []byte(`{"tile_path":"{z}/{x}/{y}"}`))
		input := t.TempDir()
		archiveTestWrite(t, filepath.Join(input, "0", "0", "0.pbf"), vector)
		if _, err := ConvertArchive(context.Background(), input, "s3://maps/legacy/", ArchiveConvertOptions{S3: out, OutputTilePath: "{z}/{x}/{y}", Append: true, Metadata: layers}); err != nil {
			t.Fatal(err)
		}
		if got := store.get("maps/legacy/0/0/0"); !bytes.Equal(got, packed) {
			t.Fatal("append replaced the legacy vector object")
		}
		bad, err := archiveGzip([]byte("<html>access denied</html>"))
		if err != nil {
			t.Fatal(err)
		}
		store.putEncoded("maps/legacy/0/0/0", bad, "gzip")
		store.put("maps/legacy/metadata.json", []byte(`{"tile_path":"{z}/{x}/{y}"}`))
		before := store.count(http.MethodPut)
		if _, err := ConvertArchive(context.Background(), input, "s3://maps/legacy/", ArchiveConvertOptions{S3: out, OutputTilePath: "{z}/{x}/{y}", Append: true, Metadata: layers}); err == nil || !strings.Contains(err.Error(), "HTML") || store.count(http.MethodPut) != before {
			t.Fatalf("gzip format hint admitted an error response: %v", err)
		}
	})
	store.put("source/vector/0/0/0", vector)
	store.put("source/vector/metadata.json", []byte(`{"format":"pbf","tile_path":"{z}/{x}/{y}","json":{"vector_layers":[{"id":"test","fields":{}}],"source":true}}`))
	if _, err := ConvertArchive(context.Background(), "s3://source/vector/", "s3://maps/vector.pmtiles", ArchiveConvertOptions{S3: out, Metadata: map[string]string{"json": `{"custom":false}`}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertArchive(context.Background(), "s3://maps/vector.pmtiles", "s3://maps/extracted/", ArchiveConvertOptions{S3: out, OutputTilePath: "{z}/{y}/{x}"}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "raw")
	if _, err := ConvertArchive(context.Background(), "s3://maps/extracted/", local, ArchiveConvertOptions{S3: out, OutputKind: "file", OutputTilePath: "{z}/{x}/{y}"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(local, "0", "0", "0"))
	if err != nil || !bytes.Equal(body, vector) {
		t.Fatalf("PBF/gzip round trip mismatch: %x, %v", body, err)
	}
	metadata, err := archiveReadDirectoryMetadata(local)
	var object map[string]json.RawMessage
	if err != nil || json.Unmarshal([]byte(metadata["json"]), &object) != nil || string(object["source"]) != "true" || string(object["custom"]) != "false" || len(object["vector_layers"]) == 0 {
		t.Fatalf("S3 JSON override lost source schema or extensions: %v, %v", metadata, err)
	}
}

func TestS3ConversionExistingMetadataValidationPrecedesWrites(t *testing.T) {
	store, out := archiveS3Store(t)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	body := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
	archiveTestWrite(t, filepath.Join(input, "1", "1", "1.png"), body)
	store.put("maps/tiles/1/0/0.png", body)
	store.put("maps/tiles/metadata.json", []byte(`{"format":"png","tile_path":"{z}/{x}/{y}.{format}","bounds":"bad"}`))
	if _, err := ConvertArchive(context.Background(), input, "s3://maps/tiles/", ArchiveConvertOptions{S3: out, Append: true}); err == nil || store.count(http.MethodPut) != 0 {
		t.Fatal("malformed old bounds were not rejected before writing")
	}
	store.put("maps/tiles/metadata.json", []byte(`{"format":"png","tile_path":"{z}/{x}/{y}.{format}","json":"{bad"}`))
	if _, err := ConvertArchive(context.Background(), input, "s3://maps/tiles/", ArchiveConvertOptions{S3: out, Append: true}); err == nil || store.count(http.MethodPut) != 0 {
		t.Fatal("malformed old vector metadata were not rejected before writing")
	}
	store.put("maps/tiles/metadata.json", []byte(`{"format":"png","tile_path":"{z}/{y}/{x}.{format}"}`))
	if _, err := ConvertArchive(context.Background(), input, "s3://maps/tiles/", ArchiveConvertOptions{S3: out, Append: true, OutputTilePath: "{z}/{y}/{x}.{format}"}); err != nil {
		t.Fatalf("metadata-verified unmarked custom-layout tiles could not be extended: %v", err)
	}
}

func TestS3ConversionRejectsExistingMixedFormatsBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		hasMetadata bool
		jpeg        string
	}{{false, "JPEG filename conflicts with existing PNG dataset"}, {true, "JPEG filename conflicts with existing PNG dataset"}, {false, ""}, {true, ""}} {
		t.Run(fmt.Sprintf("metadata=%t,empty=%t", tc.hasMetadata, tc.jpeg == ""), func(t *testing.T) {
			store, out := archiveS3Store(t)
			body := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
			store.put("maps/tiles/1/0/0.png", body)
			store.put("maps/tiles/1/1/1.jpg", []byte(tc.jpeg))
			if tc.hasMetadata {
				store.put("maps/tiles/metadata.json", []byte(`{"format":"png","tile_path":"{z}/{x}/{y}.{format}"}`))
			}
			input := filepath.Join(t.TempDir(), "input")
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), body)
			if _, err := ConvertArchive(context.Background(), input, "s3://maps/tiles/", ArchiveConvertOptions{S3: out, Append: true}); err == nil || store.count(http.MethodPut) != 0 {
				t.Fatal("existing mixed formats were accepted or changed")
			}
		})
	}
}

func TestS3ConversionRejectsNoncanonicalExistingKeysBeforeWrites(t *testing.T) {
	for _, relative := range []string{"1/0/0.jpeg", "1/0/0.PNG", "1/00/0.png"} {
		t.Run(relative, func(t *testing.T) {
			store, out := archiveS3Store(t)
			body := archiveTestPNG(t, color.RGBA{R: 255, A: 255})
			store.put("maps/tiles/"+relative, body)
			input := filepath.Join(t.TempDir(), "input")
			archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), body)
			_, err := ConvertArchive(context.Background(), input, "s3://maps/tiles/", ArchiveConvertOptions{S3: out, Append: true})
			if err == nil || !strings.Contains(err.Error(), "noncanonical") || store.count(http.MethodPut) != 0 {
				t.Fatalf("existing noncanonical key was accepted or changed: %v", err)
			}
		})
	}
}

func TestS3ConversionURIs(t *testing.T) {
	for _, uri := range []string{"s3://", "s3://u:p@bucket/key", "s3://bucket/key?secret=x", "s3://bucket/../key", "s3://bucket/path\\key", "s3://bucket:123/key", "s3://bucket/foo//bar/", "s3://bucket//foo/", "s3://bucket/foo//"} {
		if _, remote, err := parseArchiveS3Location(uri); !remote || err == nil {
			t.Fatalf("invalid URI accepted: %q", uri)
		}
	}
	location, remote, err := parseArchiveS3Location("s3://bucket/dataset.pmtiles/")
	if err != nil || !remote || location.kind != "file" {
		t.Fatalf("trailing slash did not select a tile prefix: %+v, %v", location, err)
	}
	if _, remote, err := parseArchiveS3Location("C:\\maps\\x.mbtiles"); err != nil || remote {
		t.Fatal("local Windows path was interpreted as S3")
	}
	for _, uri := range []string{"s3://bucket", "s3://bucket/", "s3://bucket/foo/"} {
		if _, remote, err := parseArchiveS3Location(uri); !remote || err != nil {
			t.Fatalf("root or trailing slash was rejected: %q: %v", uri, err)
		}
	}
	store, out := archiveS3Store(t)
	for _, pair := range [][2]string{{"s3://bucket/tiles", "s3://bucket/tiles/"}, {"s3://bucket/tiles/", "s3://bucket/tiles"}, {"s3://bucket/a.mbtiles", "s3://bucket/a.mbtiles"}} {
		if _, err := ConvertArchive(context.Background(), pair[0], pair[1], ArchiveConvertOptions{S3: out, Append: true}); err == nil || store.totalRequests() != 0 {
			t.Fatalf("same S3 location was not rejected before networking: %v", pair)
		}
	}
	for _, options := range []ArchiveConvertOptions{{Append: true, Overwrite: true}, {OutputKind: "unknown"}, {Format: "tiff"}} {
		options.S3 = out
		if _, err := ConvertArchive(context.Background(), "s3://source/tiles/", "s3://destination/tiles/", options); err == nil || store.totalRequests() != 0 {
			t.Fatalf("invalid options were not rejected before networking: %+v", options)
		}
	}
}

func TestS3ConversionRejectsCyclicListingTokens(t *testing.T) {
	requests := 0
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		next := "A"
		if r.URL.Query().Get("continuation-token") == "A" {
			next = "B"
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, "<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken></ListBucketResult>", next)
	})
	if err := archiveS3Walk(context.Background(), out, "tiles", func(string, string, int64) error { return nil }); err == nil || requests != 3 {
		t.Fatalf("cyclic pagination was not rejected promptly: requests=%d, error=%v", requests, err)
	}
}
