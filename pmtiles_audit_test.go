package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pm "tiler/internal/pmtiles"
)

func auditPMFixture(t *testing.T, root []pm.EntryV3, leaves []byte, count uint64) string {
	t.Helper()
	rootBytes := pm.SerializeEntries(root)
	metadata, err := archiveGzip([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	tile := tileDataPNG(t)
	header := pm.HeaderV3{RootOffset: 127, RootLength: uint64(len(rootBytes)), MetadataOffset: 127 + uint64(len(rootBytes)), MetadataLength: uint64(len(metadata)), LeafDirectoryOffset: 127 + uint64(len(rootBytes)+len(metadata)), LeafDirectoryLength: uint64(len(leaves)), TileDataOffset: 127 + uint64(len(rootBytes)+len(metadata)+len(leaves)), TileDataLength: uint64(len(tile)), AddressedTilesCount: count, InternalCompression: pm.Gzip, TileCompression: pm.NoCompression, TileType: pm.Png, MinZoom: 0, MaxZoom: 2, MinLonE7: -1800000000, MinLatE7: -850000000, MaxLonE7: 1800000000, MaxLatE7: 850000000}
	body := append(pm.SerializeHeader(header), rootBytes...)
	body = append(body, metadata...)
	body = append(body, leaves...)
	body = append(body, tile...)
	filename := filepath.Join(t.TempDir(), "fixture.pmtiles")
	if err := os.WriteFile(filename, body, 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestPMAuditUnknownAddressedCountIsValid(t *testing.T) {
	tile := tileDataPNG(t)
	filename := auditPMFixture(t, []pm.EntryV3{{TileId: 5, Length: uint32(len(tile)), RunLength: 1}}, nil, 0)
	count := 0
	_, err := archiveWalkPM(context.Background(), filename, func(z int, x, y uint32, body []byte, format string) error { count++; return nil })
	if err != nil || count != 1 {
		t.Fatalf("valid unknown-count PMTiles rejected: visits=%d err=%v", count, err)
	}
}

func TestPMAuditRejectsUnreachableLeafTiles(t *testing.T) {
	tile := tileDataPNG(t)
	t.Run("wrong first ID", func(t *testing.T) {
		leaf := pm.SerializeEntries([]pm.EntryV3{{TileId: 5, Length: uint32(len(tile)), RunLength: 1}})
		root := []pm.EntryV3{{TileId: 6, Length: uint32(len(leaf))}}
		filename := auditPMFixture(t, root, leaf, 1)
		_, err := archiveWalkPM(context.Background(), filename, func(z int, x, y uint32, body []byte, format string) error { return nil })
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "director") {
			t.Fatalf("unreachable/inconsistent leaf accepted: %v", err)
		}
	})
	first := pm.SerializeEntries([]pm.EntryV3{{TileId: 5, Length: uint32(len(tile)), RunLength: 3}})
	second := pm.SerializeEntries([]pm.EntryV3{{TileId: 7, Length: uint32(len(tile)), RunLength: 1}})
	root := []pm.EntryV3{{TileId: 5, Length: uint32(len(first))}, {TileId: 7, Offset: uint64(len(first)), Length: uint32(len(second))}}
	filename := auditPMFixture(t, root, append(first, second...), 4)
	visited := 0
	_, err := archiveWalkPM(context.Background(), filename, func(z int, x, y uint32, body []byte, format string) error {
		visited++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "tile run crosses its directory entry range") || visited != 0 {
		t.Fatalf("leaf run crossed its parent directory boundary: visited=%d, err=%v", visited, err)
	}
}

// Gzip integrity belongs to the actual archive reader, not a codec API that
// only tests call. The raw codec separately validates directory fields.
func TestPMAuditDirectoryGzipIntegrity(t *testing.T) {
	valid := pm.SerializeEntries([]pm.EntryV3{{TileId: 0, Offset: 0, Length: 10, RunLength: 1}})
	entries, _, err := archiveReadPMDirectory(valid, pm.Gzip, 64<<20, 1<<20)
	if err != nil || len(entries) != 1 {
		t.Fatalf("valid directory rejected: %v", err)
	}
	crc := append([]byte(nil), valid...)
	crc[len(crc)-8] ^= 1
	for name, data := range map[string][]byte{
		"empty":          nil,
		"not gzip":       []byte("directory"),
		"bad CRC":        crc,
		"truncated gzip": valid[:len(valid)-3],
		"trailing gzip":  append(append([]byte(nil), valid...), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := archiveReadPMDirectory(data, pm.Gzip, 64<<20, 1<<20); err == nil {
				t.Fatal("accepted malformed gzip directory")
			}
		})
	}
}
