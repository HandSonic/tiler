package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestCompactEmptyResumeAndCorruptIndex(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "cache")
	metadata := map[string]string{
		"format": PNG, "bounds": "-180,-85,180,85", "minzoom": "9", "maxzoom": "30",
		"json": `{"revision":7,"source":{"cached":true}}`,
	}
	cache, err := compactOpen(root, metadata, false)
	if err != nil {
		t.Fatal(err)
	}
	// A cancelled first request must not fix an empty cache to 256 pixels.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := cache.Save(cancelled, 9, 130, 127, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Save: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "_alllayers", "L09"), 0755); err != nil {
		t.Fatal(err)
	}
	cache, err = compactOpen(root, metadata, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	var body bytes.Buffer
	if err := png.Encode(&body, image.NewRGBA(image.Rect(0, 0, 512, 512))); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(ctx, 9, 130, 127, body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(ctx, 9, 131, 127, body.Bytes()); err != nil {
		t.Fatal(err)
	}
	path := compactPath(root, 9, 130, 127)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "R0000C0080.bundle" {
		t.Fatalf("wrong bundle row/column: %s", path)
	}
	index := compactIndexOffset(130, 127)
	entry := binary.LittleEndian.Uint64(original[index:])
	offset := entry & compactOffsetMask
	if entry>>40 != uint64(body.Len()) || !bytes.Equal(original[offset:offset+uint64(body.Len())], body.Bytes()) {
		t.Fatal("index did not point to the original image bytes")
	}
	if _, err := archiveWalkCompact(ctx, root, func(_ int, _, _ uint32, _ []byte, _ string) error {
		return os.ErrNotExist
	}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("visitor error was swallowed: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func([]byte)
	}{
		{"outside bundle", func(data []byte) {
			binary.LittleEndian.PutUint64(data[index:], uint64(len(data)+1)|uint64(1)<<40)
		}},
		{"prefix mismatch", func(data []byte) {
			binary.LittleEndian.PutUint32(data[offset-4:], uint32(body.Len()+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := append([]byte(nil), original...)
			test.change(data)
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
			if exists, err := cache.Exists(ctx, 9, 130, 127); err == nil || exists {
				t.Fatalf("damaged tile was treated as valid: exists=%v err=%v", exists, err)
			}
			if _, err := archiveWalkCompact(ctx, root, func(_ int, _, _ uint32, _ []byte, _ string) error { return nil }); err == nil {
				t.Fatal("damaged index accepted by archive reader")
			}
		})
	}
	// Empty index entries may contain a nonzero offset in external caches.
	binary.LittleEndian.PutUint64(original[index:], compactDataStart)
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if exists, err := cache.Exists(ctx, 9, 130, 127); err != nil || exists {
		t.Fatalf("empty index entry: exists=%v err=%v", exists, err)
	}
	seen := 0
	if _, err := archiveWalkCompact(ctx, root, func(z int, x, y uint32, actual []byte, format string) error {
		seen++
		if z != 9 || x != 131 || y != 127 || format != PNG || !bytes.Equal(actual, body.Bytes()) {
			t.Fatal("readback changed the surviving tile")
		}
		return nil
	}); err != nil || seen != 1 {
		t.Fatalf("readback: tiles=%d error=%v", seen, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["revision"] != float64(7) || decoded["pixel_scale"] != "512" {
		t.Fatalf("typed metadata or actual image size lost: %s", data)
	}
	// External TileJSON metadata can use a coordinate array instead of text.
	decoded["bounds"] = []float64{-180, -85, 180, 85}
	data, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := compactReadMetadata(root); err != nil {
		t.Fatalf("valid bounds array rejected: %v", err)
	}
	config, _, err := compactReadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*compactXML)
		valid  bool
	}{
		{"rounded Esri grid", func(config *compactXML) {
			// Official sample_cache/conf.xml uses rounded decimal values, not
			// exact binary equality with pi * 6378137. Keep that cache readable.
			// https://github.com/Esri/raster-tiles-compactcache/blob/master/sample_cache/conf.xml
			config.Cache.Origin.X, config.Cache.Origin.Y = -20037508.342787001, 20037508.342787001
			config.Cache.Width, config.Cache.Height = 256, 256
			config.Cache.LODs = config.Cache.LODs[:20]
			for i := range config.Cache.LODs {
				config.Cache.LODs[i].Resolution *= 2 // the original cache uses 512 pixels
			}
			config.Cache.LODs[0].Resolution = 156543.03392800014
			config.Cache.LODs[19].Resolution = 0.29858214164761665
		}, true},
		{"displaced origin", func(config *compactXML) {
			config.Cache.Origin.X += 0.01 // centimetres become many pixels at z30
		}, false},
		{"accumulated resolution error", func(config *compactXML) {
			config.Cache.LODs[30].Resolution *= 1 + 5e-10
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := config
			changed.Cache.LODs = append([]compactLOD(nil), config.Cache.LODs...)
			test.change(&changed)
			conf, err := xml.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "conf.xml"), conf, 0644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := compactReadConfig(root); (err == nil) != test.valid {
				t.Fatalf("grid valid=%v, want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}
