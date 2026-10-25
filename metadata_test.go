package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/paulmach/orb"
)

func fileMetadataTask(dir string) *Task {
	return &Task{
		ID: "metadata-test", Name: "test-map", File: dir, Min: 3, Max: 8,
		outformat: "file",
		TileMap:   TileMap{Name: "test-map", Format: PNG},
		Layers: []Layer{
			{Zoom: 3, Collection: orb.Collection{orb.Bound{Min: orb.Point{10, 20}, Max: orb.Point{30, 40}}}},
			{Zoom: 8, Collection: orb.Collection{orb.Bound{Min: orb.Point{30, 40}, Max: orb.Point{50, 60}}}},
		},
	}
}

func TestSaveFileMetadata(t *testing.T) {
	task := fileMetadataTask(t.TempDir())
	if err := task.saveFileMetadata(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(task.File, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["version"]; exists {
		t.Error("download metadata invented an unspecified dataset version")
	}
	want := task.MetaItems()
	want["tile_path"] = defaultTilePath
	want["scheme"] = "xyz"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metadata = %v; want %v", got, want)
	}
	for key, want := range map[string]string{
		"bounds":  "10.000000,20.000000,50.000000,60.000000",
		"center":  "30.000000,40.000000,5",
		"minzoom": "3",
		"maxzoom": "8",
		"format":  "png",
	} {
		if got[key] != want {
			t.Errorf("metadata[%q] = %q; want %q", key, got[key], want)
		}
	}
	assertNoMetadataTemps(t, task.File)
}

func TestSaveFileMetadataReplacesExistingFile(t *testing.T) {
	task := fileMetadataTask(t.TempDir())
	path := filepath.Join(task.File, "metadata.json")
	original := []byte(`{"old":"metadata"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// A hard link retains the old file's bytes when the destination is replaced.
	// An in-place overwrite would also change this link's contents.
	oldLink := filepath.Join(task.File, "old-metadata.json")
	if err := os.Link(path, oldLink); err != nil {
		t.Skipf("filesystem does not support hard links: %v", err)
	}
	if err := task.saveFileMetadata(); err != nil {
		t.Fatal(err)
	}
	oldData, err := os.ReadFile(oldLink)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldData, original) {
		t.Error("metadata was overwritten in place instead of replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != task.ID {
		t.Errorf("replacement metadata id = %q; want %q", got["id"], task.ID)
	}
	assertNoMetadataTemps(t, task.File)
}

func TestSaveFileMetadataRejectsDirectoryBeforeWriting(t *testing.T) {
	task := fileMetadataTask(t.TempDir())
	path := filepath.Join(task.File, "metadata.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(path, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := task.saveFileMetadata(); err == nil {
		t.Fatal("replacing a directory should fail")
	}
	data, err := os.ReadFile(keep)
	if err != nil || string(data) != "keep" {
		t.Fatalf("failed replacement changed the existing target: %q, %v", data, err)
	}
	assertNoMetadataTemps(t, task.File)
}

func TestSaveFileMetadataMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	task := fileMetadataTask(dir)
	if err := task.saveFileMetadata(); err == nil {
		t.Fatal("writing metadata without an output directory should fail")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("metadata unexpectedly created the output directory: %v", err)
	}
	task.File = ""
	if err := task.saveFileMetadata(); err == nil {
		t.Fatal("empty output directory should fail")
	}
}

func TestSaveFileMetadataOnlyWritesFileOutput(t *testing.T) {
	for _, format := range []string{"s3", "mbtiles"} {
		t.Run(format, func(t *testing.T) {
			task := fileMetadataTask(t.TempDir())
			task.outformat = format
			if err := task.saveFileMetadata(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(task.File)
			if err != nil || len(entries) != 0 {
				t.Fatalf("%s output created metadata files: %v, %v", format, entries, err)
			}
		})
	}
}

func assertNoMetadataTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".metadata-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("metadata temporary files remain: %v, %v", files, err)
	}
}

func TestArchiveMetadataPreservesOnlyProvidedVersion(t *testing.T) {
	var extent archiveExtent
	extent.add(0, 0, 0)
	for _, version := range []string{"", "2026.10.21"} {
		metadata := map[string]string{}
		if version != "" {
			metadata["version"] = version
		}
		if err := archiveCompleteMetadata(metadata, PNG, "map", extent); err != nil {
			t.Fatal(err)
		}
		got, exists := metadata["version"]
		if got != version || exists != (version != "") {
			t.Errorf("dataset version = %q (present %t); provided %q", got, exists, version)
		}
	}
}
