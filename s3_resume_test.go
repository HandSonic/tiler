package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/viper"
)

func TestS3ResumeChecksObjectsBeforeDownloading(t *testing.T) {
	configureS3Test(t)
	viper.Set("output.format", "s3")
	viper.Set("output.resume", true)
	viper.Set("task.workers", 1)
	body := tileDataPNG(t)
	var downloads, uploads, checks int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Write(body)
	}))
	defer source.Close()
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
