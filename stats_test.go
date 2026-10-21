package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestStatsCountCompletedSavesAndResumeSkips(t *testing.T) {
	for _, format := range []string{"file", "mbtiles"} {
		t.Run(format, func(t *testing.T) {
			configureRegressionOutput(t)
			viper.Set("output.format", format)
			viper.Set("output.resume", true)
			body := tileDataPNG(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
			defer server.Close()
			task := newRegressionTask(t, server.URL)
			if err := task.Download(); err != nil {
				t.Fatal(err)
			}
			if got := task.Stats(); got != (DownloadStats{Saved: 1}) {
				t.Fatalf("first pass stats = %+v", got)
			}
			task = newRegressionTask(t, server.URL)
			if format == "file" {
				// Existing tiles must bypass the request interval entirely.
				task.timeDelay = 3600000
				ctx, cancel := context.WithTimeout(task.ctx, 3*time.Second)
				defer cancel()
				task.ctx = ctx
			}
			if err := task.Download(); err != nil {
				t.Fatal(err)
			}
			if got := task.Stats(); got != (DownloadStats{Skipped: 1}) {
				t.Fatalf("resume stats = %+v", got)
			}
		})
	}
}

func TestStatsDoNotCountFailedFetchAsSaved(t *testing.T) {
	configureRegressionOutput(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	task := newRegressionTask(t, server.URL)
	if err := task.Download(); err == nil {
		t.Fatal("403 did not fail the task")
	}
	if got := task.Stats(); got != (DownloadStats{Failed: 1}) {
		t.Fatalf("failed fetch stats = %+v", got)
	}
}
