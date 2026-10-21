package main

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
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
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/spf13/viper"
)

func archiveUploadTestOutput(t *testing.T, handler http.HandlerFunc) *s3Output {
	t.Helper()
	configureS3Test(t)
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	viper.Set("output.s3.endpoint", server.URL)
	viper.Set("output.s3.prefix", "/archives/")
	out, err := newS3Output(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func archiveUploadFixture(t *testing.T, filename, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return filename
}

func archiveUploadSparseFixture(t *testing.T, filename string, size int64) string {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestUploadArchivesSingleAndDirectoryKeys(t *testing.T) {
	type received struct{ path, contentType, encoding, authorization, body string }
	var requests []received
	var requestsMu sync.Mutex
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requestsMu.Lock()
		requests = append(requests, received{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get("Authorization"), string(body)})
		requestsMu.Unlock()
		if r.Method != http.MethodPut || r.URL.Query().Has("uploadId") || r.URL.Query().Has("uploads") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("ETag", `"archive-etag"`)
	})
	root := t.TempDir()
	explicit := archiveUploadFixture(t, filepath.Join(root, "single.PMTILES"), "pmtiles-data")
	dir := filepath.Join(root, "collection")
	archiveUploadFixture(t, filepath.Join(dir, "base.mbtiles"), "sqlite-data")
	archiveUploadFixture(t, filepath.Join(dir, "nested", "map.pmtiles"), "nested-data")
	archiveUploadFixture(t, filepath.Join(dir, "README.txt"), "ignored")
	got, err := UploadArchives(context.Background(), out, []string{explicit, dir}, ArchiveUploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Uploaded != 3 || got.Bytes != int64(len("pmtiles-datasqlite-datanested-data")) || len(got.Objects) != 3 {
		t.Fatalf("result=%+v", got)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	wantPaths := []string{"/test-bucket/archives/single.PMTILES", "/test-bucket/archives/base.mbtiles", "/test-bucket/archives/nested/map.pmtiles"}
	wantTypes := []string{"application/vnd.pmtiles", "application/vnd.sqlite3", "application/vnd.pmtiles"}
	wantBodies := []string{"pmtiles-data", "sqlite-data", "nested-data"}
	if len(requests) != len(wantPaths) {
		t.Fatalf("requests=%+v", requests)
	}
	for i, req := range requests {
		if req.path != wantPaths[i] || req.contentType != wantTypes[i] || req.body != wantBodies[i] || req.encoding != "" {
			t.Errorf("request[%d]=%+v", i, req)
		}
		if !strings.HasPrefix(req.authorization, "AWS4-HMAC-SHA256 Credential=test-access-key/") {
			t.Error("missing SigV4 authorization")
		}
		if got.Objects[i].Key != strings.TrimPrefix(wantPaths[i], "/test-bucket/") {
			t.Errorf("object key=%q", got.Objects[i].Key)
		}
	}
}

func TestUploadArchivesPreflightRejectsAllBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	first := archiveUploadFixture(t, filepath.Join(root, "one", "same.pmtiles"), "archive")
	second := archiveUploadFixture(t, filepath.Join(root, "two", "same.pmtiles"), "another")
	empty := archiveUploadFixture(t, filepath.Join(root, "empty.mbtiles"), "")
	text := archiveUploadFixture(t, filepath.Join(root, "notes.txt"), "notes")
	emptyDir := filepath.Join(root, "empty-dir")
	if err := os.Mkdir(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		inputs []string
		opts   ArchiveUploadOptions
	}{
		{"no inputs", nil, ArchiveUploadOptions{}},
		{"missing after valid", []string{first, filepath.Join(root, "missing.pmtiles")}, ArchiveUploadOptions{}},
		{"duplicate keys", []string{first, second}, ArchiveUploadOptions{}},
		{"empty archive", []string{first, empty}, ArchiveUploadOptions{}},
		{"wrong extension", []string{first, text}, ArchiveUploadOptions{}},
		{"empty directory", []string{emptyDir}, ArchiveUploadOptions{}},
		{"negative timeout", []string{first}, ArchiveUploadOptions{Timeout: -time.Second}},
		{"small parts", []string{first}, ArchiveUploadOptions{PartSize: manager.MinUploadPartSize - 1}},
		{"negative concurrency", []string{first}, ArchiveUploadOptions{Concurrency: -1}},
		{"excess concurrency", []string{first}, ArchiveUploadOptions{Concurrency: 33}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
			result, err := UploadArchives(context.Background(), out, tt.inputs, tt.opts)
			if err == nil || result.Uploaded != 0 || requests.Load() != 0 {
				t.Fatalf("preflight result=%+v error=%v requests=%d", result, err, requests.Load())
			}
		})
	}
}

func TestUploadArchivesRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := archiveUploadFixture(t, filepath.Join(dir, "target.pmtiles"), "archive")
	link := filepath.Join(dir, "link.pmtiles")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	for _, input := range []string{link, dir} {
		var requests atomic.Int32
		out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
		if _, err := UploadArchives(context.Background(), out, []string{input}, ArchiveUploadOptions{}); err == nil || requests.Load() != 0 {
			t.Fatalf("symlink accepted: err=%v requests=%d", err, requests.Load())
		}
	}
}

func TestPlanArchiveUploadsAcceptsLargeSparseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file extension may allocate physical space without an explicit sparse-file control")
	}
	size := int64(6) * 1024 * 1024 * 1024
	file := archiveUploadSparseFixture(t, filepath.Join(t.TempDir(), "large.pmtiles"), size)
	plan, err := planArchiveUploads(context.Background(), "archives", []string{file})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Size != size || plan[0].Key != "archives/large.pmtiles" {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestUploadArchivesMultipartCompletesOrderedParts(t *testing.T) {
	var mu sync.Mutex
	sizes := map[int]int64{}
	var completed []int
	var created, aborted int
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			mu.Lock()
			created++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>archives/multi.pmtiles</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && query.Get("uploadId") == "test-upload":
			number, _ := strconv.Atoi(query.Get("partNumber"))
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			sizes[number] = n
			mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, number))
		case r.Method == http.MethodPost && query.Get("uploadId") == "test-upload":
			var body struct {
				Parts []struct {
					Number int `xml:"PartNumber"`
				} `xml:"Part"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			for _, part := range body.Parts {
				completed = append(completed, part.Number)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>test-bucket</Bucket><Key>archives/multi.pmtiles</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete:
			mu.Lock()
			aborted++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected multipart request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	size := 2*manager.MinUploadPartSize + 17
	file := archiveUploadSparseFixture(t, filepath.Join(t.TempDir(), "multi.pmtiles"), size)
	result, err := UploadArchives(context.Background(), out, []string{file}, ArchiveUploadOptions{PartSize: manager.MinUploadPartSize, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if result.Uploaded != 1 || result.Bytes != size || created != 1 || aborted != 0 || sizes[1] != manager.MinUploadPartSize || sizes[2] != manager.MinUploadPartSize || sizes[3] != 17 {
		t.Fatalf("result=%+v create=%d abort=%d sizes=%v", result, created, aborted, sizes)
	}
	if fmt.Sprint(completed) != "[1 2 3]" {
		t.Fatalf("completed parts=%v", completed)
	}
}

func TestUploadArchivesMultipartFailureAbortsWithoutCompleting(t *testing.T) {
	var creates, aborts, completes atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			creates.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>failed-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Denied part</Message></Error>`)
		case r.Method == http.MethodDelete && query.Get("uploadId") == "failed-upload":
			aborts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			completes.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	file := archiveUploadSparseFixture(t, filepath.Join(t.TempDir(), "failed.pmtiles"), manager.MinUploadPartSize+1)
	result, err := UploadArchives(context.Background(), out, []string{file}, ArchiveUploadOptions{PartSize: manager.MinUploadPartSize, Concurrency: 1})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || result.Uploaded != 0 || creates.Load() != 1 || aborts.Load() != 1 || completes.Load() != 0 {
		t.Fatalf("result=%+v err=%v create=%d abort=%d complete=%d", result, err, creates.Load(), aborts.Load(), completes.Load())
	}
}

func TestUploadArchivesCancellationStillAbortsMultipart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	var aborts, completes atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>cancel-upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case r.Method == http.MethodDelete && query.Get("uploadId") == "cancel-upload":
			aborts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			completes.Add(1)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	file := archiveUploadSparseFixture(t, filepath.Join(t.TempDir(), "cancel.pmtiles"), manager.MinUploadPartSize+1)
	done := make(chan error, 1)
	go func() {
		_, err := UploadArchives(ctx, out, []string{file}, ArchiveUploadOptions{PartSize: manager.MinUploadPartSize, Concurrency: 1})
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(3 * time.Second):
		t.Fatal("multipart upload did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || aborts.Load() != 1 || completes.Load() != 0 {
			t.Fatalf("err=%v abort=%d complete=%d", err, aborts.Load(), completes.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled upload did not finish and abort")
	}
}

func TestUploadArchivesReturnsCompletedFilesBeforeFailure(t *testing.T) {
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "second.mbtiles") {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		w.Header().Set("ETag", `"uploaded"`)
	})
	root := t.TempDir()
	first := archiveUploadFixture(t, filepath.Join(root, "first.pmtiles"), "first")
	second := archiveUploadFixture(t, filepath.Join(root, "second.mbtiles"), "second")
	third := archiveUploadFixture(t, filepath.Join(root, "third.pmtiles"), "third")
	result, err := UploadArchives(context.Background(), out, []string{first, second, third}, ArchiveUploadOptions{})
	if err == nil || result.Uploaded != 1 || result.Bytes != 5 || len(result.Objects) != 1 || result.Objects[0].Key != "archives/first.pmtiles" || requests.Load() != 2 {
		t.Fatalf("result=%+v err=%v requests=%d", result, err, requests.Load())
	}
}

func archiveUploadWALDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "live.mbtiles")
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE tiles (tile_data blob); INSERT INTO tiles VALUES (x'01');"); err != nil {
		t.Fatal(err)
	}
	return db, filename
}

func TestUploadArchivesRejectsUncheckpointedMBTilesBeforeNetwork(t *testing.T) {
	_, filename := archiveUploadWALDatabase(t)
	wal, err := os.Stat(filename + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("expected live WAL: %v %v", wal, err)
	}
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	result, err := UploadArchives(context.Background(), out, []string{filename}, ArchiveUploadOptions{})
	if err == nil || !strings.Contains(err.Error(), "checkpoint") || !strings.Contains(err.Error(), "close") || result.Uploaded != 0 || requests.Load() != 0 {
		t.Fatalf("result=%+v error=%v requests=%d", result, err, requests.Load())
	}
}

func TestUploadArchivesRechecksWALAfterPlanning(t *testing.T) {
	db, filename := archiveUploadWALDatabase(t)
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	plan, err := planArchiveUploads(context.Background(), "archives", []string{filename})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tiles VALUES (x'02')"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != plan[0].Size || !info.ModTime().Equal(plan[0].info.ModTime()) {
		t.Fatal("test must leave the main SQLite file unchanged")
	}
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	err = uploadArchiveFile(context.Background(), out, manager.NewUploader(out.client), plan[0], time.Minute)
	if err == nil || !strings.Contains(err.Error(), "checkpoint") || requests.Load() != 0 {
		t.Fatalf("error=%v requests=%d", err, requests.Load())
	}
}

func TestUploadArchivesAcceptsEmptyWAL(t *testing.T) {
	filename := archiveUploadFixture(t, filepath.Join(t.TempDir(), "checkpointed.mbtiles"), "checkpointed archive")
	archiveUploadFixture(t, filename+"-wal", "")
	var requests atomic.Int32
	out := archiveUploadTestOutput(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"uploaded"`)
	})
	result, err := UploadArchives(context.Background(), out, []string{filename}, ArchiveUploadOptions{})
	if err != nil || result.Uploaded != 1 || requests.Load() != 1 {
		t.Fatalf("result=%+v error=%v requests=%d", result, err, requests.Load())
	}
}
