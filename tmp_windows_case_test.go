package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWindowsCompactMetadataCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires a case-insensitive Windows filesystem")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "cache")
	cache, err := compactOpen(ctx, root, map[string]string{
		"format": PNG, "bounds": "-180,-85,180,85", "minzoom": "0", "maxzoom": "0",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "conf.xml"), filepath.Join(root, "CONF.XML")); err != nil {
		t.Fatal(err)
	}
	files, err := planArchiveUploads(ctx, "cache", []string{root})
	if err != nil {
		return // Rejecting a noncanonical metadata name is safe.
	}
	for _, file := range files {
		if file.Key == "cache/conf.xml" {
			return
		}
	}
	t.Fatalf("Compact upload succeeded but omitted conf.xml after a case-only rename: %+v", files)
}
