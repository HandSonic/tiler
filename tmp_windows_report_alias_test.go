package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/spf13/viper"
)

func TestWindowsCompactReportAlias(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires ordinary Win32 path normalization")
	}
	dir := configureRegressionOutput(t)
	viper.Set("output.format", "compactv2")
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	task := newRegressionTask(t, server.URL+"/{z}/{x}/{y}")
	task.File = filepath.Join(dir, "cache")
	report := filepath.Join(task.File, "notes.bundle.", "errors.json")
	// Keep this reproduction on Go 1.20's ordinary Win32 path branch.
	if len(report) >= 248 {
		t.Fatalf("reproduction requires a short temporary path: %s", report)
	}
	viper.Set("output.failure_file", report)
	err := task.Download()
	entries, readErr := os.ReadDir(task.File)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if got := atomic.LoadInt32(&requests); got != 0 || !os.IsNotExist(readErr) || err == nil {
		t.Fatalf("invalid Windows report path reached output/download: requests=%d, download=%v, cache entries=%v, read=%v", got, err, names, readErr)
	}
}
