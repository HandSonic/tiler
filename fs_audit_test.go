package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func TestAuditConfigRejectsNamedPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX named pipe reproduction")
	}
	pipe := filepath.Join(t.TempDir(), "config.toml")
	if err := exec.Command("mkfifo", pipe).Run(); err != nil {
		t.Skipf("named pipe unavailable: %v", err)
	}
	viper.Reset()
	t.Cleanup(viper.Reset)
	done := make(chan error, 1)
	go func() { done <- initConf(pipe) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("named pipe configuration should be rejected")
		}
	case <-time.After(3 * time.Second):
		// O_RDWR also opens when the reader has already returned; cleanup must
		// not wait for a peer after a scheduling delay on a loaded test runner.
		writer, err := os.OpenFile(pipe, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("[output]\nformat = 'file'\n"))
		writer.Close()
		_ = awaitRegressionError(t, done)
		t.Fatal("configuration reader blocks on a named pipe")
	}
}

func TestAuditConfigRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(16<<20 + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	viper.Reset()
	t.Cleanup(viper.Reset)
	if err := initConf(path); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("oversized configuration was not rejected before decoding: %v", err)
	}
}

func TestAuditFileOutputRejectsSymlinkEscapes(t *testing.T) {
	for _, location := range []string{"root", "directory", "tile"} {
		t.Run(location, func(t *testing.T) {
			parent, external := t.TempDir(), t.TempDir()
			root := filepath.Join(parent, "tiles")
			if err := os.Mkdir(root, 0755); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(root, "2", "3", "1.png")
			switch location {
			case "root":
				if err := os.Remove(root); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, root); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.Symlink(external, filepath.Join(root, "2")); err != nil {
					t.Skip(err)
				}
			case "tile":
				if err := os.MkdirAll(filepath.Dir(leaf), 0755); err != nil {
					t.Fatal(err)
				}
				externalFile := filepath.Join(external, "foreign.png")
				if err := os.WriteFile(externalFile, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(externalFile, leaf); err != nil {
					t.Skip(err)
				}
			}
			task := &Task{File: root, outformat: "file", tilePath: defaultTilePath, TileMap: TileMap{Format: PNG}}
			tile := maptile.Tile{Z: 2, X: 3, Y: 1}
			if _, err := task.tileExists(context.Background(), tile); err == nil {
				t.Error("resume accepted a symbolic link in the output path")
			}
			if err := saveToFiles(Tile{T: tile, C: tileDataPNG(t)}, task); err == nil {
				t.Error("write accepted a symbolic link in the output path")
			}
		})
	}
}

func TestAuditFileOutputRejectsLinkedConfiguredRoot(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	parent := t.TempDir()
	root := filepath.Join(parent, "output")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Skip(err)
	}
	viper.Set("output.directory", root)
	task := &Task{File: filepath.Join(root, "dataset"), outformat: "file"}
	if err := task.validateFileOutputSafety(); err == nil {
		t.Fatal("file output accepted a linked configured root although repair and MBTiles reject it")
	}
}

func TestAuditOutputNamesUseWindowsRules(t *testing.T) {
	for label, name := range map[string]string{"spaced device": "CON .txt", "superscript COM": "COM¹", "superscript LPT": "LPT².log", "long segment": strings.Repeat("a", 256)} {
		t.Run(label, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			viper.Set("output.name", name)
			viper.Set("output.directory", t.TempDir())
			if _, err := (&Task{outformat: "file"}).outputPath(); err == nil {
				t.Fatal("accepted an invalid Windows output filename")
			}
		})
	}
}

func TestAuditGeoJSONRejectsInvalidShapesAndExcessiveNesting(t *testing.T) {
	for _, data := range []string{
		`{"type":"LineString","coordinates":[[0,0]]}`,
		`{"type":"MultiLineString","coordinates":[[[0,0]]]}`,
		`{"type":"Polygon","coordinates":[[[0,0],[1,0],[0,0]]]}`,
		`{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1]]]}`,
		`{"type":"MultiPolygon","coordinates":[[[[0,0],[1,0],[0,0]]]]}`,
	} {
		if _, err := readCollection(writeGeoJSONTestFile(t, data)); err == nil {
			t.Errorf("accepted invalid geometry: %s", data)
		}
	}
	data := `{"type":"Point","coordinates":[0,0]}`
	for i := 0; i < 64; i++ {
		data = `{"type":"GeometryCollection","geometries":[` + data + `]}`
	}
	if _, err := readCollection(writeGeoJSONTestFile(t, data)); err == nil {
		t.Error("accepted unbounded geometry nesting")
	}
}

func TestAuditGeoJSONRejectsOversizedFilesBeforeDecode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.geojson")
	file, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64<<20 + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readCollection(p); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversize file was not rejected before decoding: %v", err)
	}
}

func TestAuditTilePathRejectsOversizedTemplate(t *testing.T) {
	pattern := strings.Repeat("a/", 17000) + "{z}/{x}/{y}"
	if _, err := normalizeTilePath(pattern); err == nil {
		t.Fatal("accepted a tile path exceeding the parser budget")
	}
}

func TestAuditAtomicFileReplacementPreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode preservation")
	}
	root := t.TempDir()
	task := &Task{File: root, outformat: "file", tilePath: defaultTilePath, TileMap: TileMap{Format: PNG}}
	tile := maptile.Tile{Z: 0, X: 0, Y: 0}
	path := filepath.Join(root, "0", "0", "0.png")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := saveToFiles(Tile{T: tile, C: tileDataPNG(t)}, task); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("replacement changed readable tile permissions: %o", info.Mode().Perm())
	}
}

func TestAuditArchiveReplacementPreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode preservation")
	}
	input := t.TempDir()
	archiveTestWrite(t, filepath.Join(input, "0", "0", "0.png"), tileDataPNG(t))
	for _, tt := range []struct {
		kind   string
		append bool
	}{{"file", true}, {"mbtiles", false}, {"pmtiles", true}} {
		t.Run(tt.kind, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output."+tt.kind)
			if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{OutputKind: tt.kind}); err != nil {
				t.Fatal(err)
			}
			path := output
			if tt.kind == "file" {
				path = filepath.Join(output, "metadata.json")
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			if _, err := ConvertArchive(context.Background(), input, output, ArchiveConvertOptions{OutputKind: tt.kind, Append: tt.append, Overwrite: !tt.append}); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0640 {
				t.Fatalf("conversion changed existing permissions: %o", info.Mode().Perm())
			}
			if tt.kind == "mbtiles" {
				configureRegressionOutput(t)
				viper.Set("output.format", "mbtiles")
				viper.Set("output.directory", filepath.Dir(output))
				viper.Set("output.name", "output")
				redownload := newRegressionTask(t, "https://tiles.example/{z}/{x}/{y}")
				if err := redownload.SetupMBTileTables(); err != nil {
					t.Fatal(err)
				}
				if err := redownload.db.Close(); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(output)
				if err != nil || info.Mode().Perm() != 0640 {
					t.Fatalf("download recreation changed existing database permissions: info=%v err=%v", info, err)
				}
			}
		})
	}
	// File and Compact cache replacement share this directory publisher.
	destination, staged := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staged, 0755); err != nil {
		t.Fatal(err)
	}
	if err := archivePublishDirectory(staged, destination, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory replacement changed existing permissions: %o", info.Mode().Perm())
	}
}
