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
	for _, tt := range []struct {
		name  string
		root  []pm.EntryV3
		child []pm.EntryV3
		count uint64
	}{
		{"wrong first ID", []pm.EntryV3{{TileId: 6, RunLength: 0}}, []pm.EntryV3{{TileId: 5, Length: uint32(len(tile)), RunLength: 1}}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			leaf := pm.SerializeEntries(tt.child)
			tt.root[0].Length = uint32(len(leaf))
			filename := auditPMFixture(t, tt.root, leaf, tt.count)
			_, err := archiveWalkPM(context.Background(), filename, func(z int, x, y uint32, body []byte, format string) error { return nil })
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "director") {
				t.Fatalf("unreachable/inconsistent leaf accepted: %v", err)
			}
		})
	}
	first := pm.SerializeEntries([]pm.EntryV3{{TileId: 5, Length: uint32(len(tile)), RunLength: 3}})
	second := pm.SerializeEntries([]pm.EntryV3{{TileId: 8, Length: uint32(len(tile)), RunLength: 1}})
	root := []pm.EntryV3{{TileId: 5, Length: uint32(len(first))}, {TileId: 7, Offset: uint64(len(first)), Length: uint32(len(second))}}
	filename := auditPMFixture(t, root, append(first, second...), 4)
	_, err := archiveWalkPM(context.Background(), filename, func(z int, x, y uint32, body []byte, format string) error { return nil })
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "director") {
		t.Fatalf("leaf run crossed its parent directory boundary: %v", err)
	}
}
