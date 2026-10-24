package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmach/orb"
	"github.com/spf13/viper"
)

func TestS3ResumeChecksObjectsBeforeDownloading(t *testing.T) {
	configureS3Test(t)
	viper.Set("output.format", "s3")
	viper.Set("output.resume", true)
	viper.Set("task.workers", 1)
	body := tileDataPNG(t)
	var downloads, uploads, checks int32
	var metadataBody atomic.Value
	metadataBody.Store("")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Write(body)
	}))
	defer source.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/conf.xml") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/metadata.json") {
			if metadata := metadataBody.Load().(string); metadata == "" {
				w.WriteHeader(http.StatusNotFound)
			} else {
				io.WriteString(w, metadata)
			}
			return
		}
		switch r.Method {
		case http.MethodHead:
			atomic.AddInt32(&checks, 1)
			if atomic.LoadInt32(&uploads) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != string(body) {
				t.Errorf("uploaded data mismatch: %v", err)
			}
			atomic.AddInt32(&uploads, 1)
		default:
			t.Errorf("unexpected S3 method %s", r.Method)
		}
	}))
	defer destination.Close()
	viper.Set("output.s3.endpoint", destination.URL)
	for i := 0; i < 2; i++ {
		task := newRegressionTask(t, source.URL)
		if err := task.Download(); err != nil {
			t.Fatal(err)
		}
	}
	if downloads != 1 || uploads != 1 || checks != 2 {
		t.Fatalf("downloads/uploads/checks = %d/%d/%d", downloads, uploads, checks)
	}
	// A converted prefix can declare a layout before a download appends a
	// disjoint region. Validate that declaration before checking new tiles.
	metadataBody.Store(`{"format":"png","tile_path":"{z}/{x}/{y}.{format}"}`)
	for _, changed := range []struct{ pattern, format, message string }{
		{"{z}/{y}/{x}.{format}", PNG, "tile_path"},
		{defaultTilePath, JPG, "tile format"},
	} {
		viper.Set("output.tile_path", changed.pattern)
		task := taskForTest(source.URL, changed.format, orb.Collection{orb.Point{45, -45}})
		err := task.Download()
		if err == nil || !strings.Contains(err.Error(), changed.message) {
			t.Fatalf("resume ignored declared prefix identity: %v", err)
		}
	}
	if downloads != 1 || uploads != 1 || checks != 2 {
		t.Fatal("mismatching prefix metadata reached tile checks, source downloads or PUT")
	}
	viper.Set("output.tile_path", defaultTilePath)
	if err := newRegressionTask(t, source.URL).Download(); err != nil {
		t.Fatalf("matching prefix metadata prevented resume: %v", err)
	}
	if downloads != 1 || uploads != 1 || checks != 3 {
		t.Fatal("matching prefix metadata did not preserve the existing tile")
	}
	metadataBody.Store(`{"format":"png","scheme":"tms"}`)
	if err := newRegressionTask(t, source.URL).Download(); err == nil || !strings.Contains(err.Error(), "tile_path") {
		t.Fatalf("XYZ resume accepted a scheme-only TMS cache: %v", err)
	}
	if downloads != 1 || uploads != 1 || checks != 3 {
		t.Fatal("mismatching TMS declaration reached tile requests")
	}
	viper.Set("output.tile_path", "{z}/{x}/{-y}.{format}")
	if err := newRegressionTask(t, source.URL).Download(); err != nil {
		t.Fatalf("matching TMS declaration rejected an unmarked legacy tile: %v", err)
	}
	if downloads != 1 || uploads != 1 || checks != 4 {
		t.Fatal("matching TMS declaration did not skip the existing tile")
	}
	metadataBody.Store(`{"scheme":"tms"}`)
	if err := newRegressionTask(t, source.URL).Download(); err == nil || !strings.Contains(err.Error(), "unmarked") {
		t.Fatalf("incomplete declaration accepted an unmarked custom-layout tile: %v", err)
	}
	if downloads != 1 || uploads != 1 {
		t.Fatal("incomplete declaration reached source downloads or PUT")
	}
}

func TestS3ResumeDoesNotTreatAccessDeniedAsMissing(t *testing.T) {
	configureS3Test(t)
	viper.Set("output.format", "s3")
	viper.Set("output.resume", true)
	viper.Set("task.workers", 1)
	var downloads int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&downloads, 1) }))
	defer source.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer destination.Close()
	viper.Set("output.s3.endpoint", destination.URL)
	err := newRegressionTask(t, source.URL).Download()
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want HEAD 403, got %v", err)
	}
	if downloads != 0 {
		t.Fatal("downloaded before confirming whether S3 object exists")
	}
}
