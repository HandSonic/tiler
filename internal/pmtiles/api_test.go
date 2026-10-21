package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"reflect"
	"testing"
)

func TestHilbertKnownIDsAndRoundTrips(t *testing.T) {
	cases := []struct {
		z    uint8
		x, y uint32
		id   uint64
	}{
		{0, 0, 0, 0}, {1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4},
		{2, 0, 0, 5}, {2, 1, 0, 6}, {2, 1, 1, 7}, {2, 0, 1, 8},
	}
	for _, tt := range cases {
		if got := ZxyToId(tt.z, tt.x, tt.y); got != tt.id {
			t.Errorf("ZxyToId(%d,%d,%d)=%d want %d", tt.z, tt.x, tt.y, got, tt.id)
		}
		z, x, y := IdToZxy(tt.id)
		if z != tt.z || x != tt.x || y != tt.y {
			t.Errorf("IdToZxy(%d)=%d/%d/%d", tt.id, z, x, y)
		}
	}
	for z := uint8(0); z <= 30; z++ {
		n := uint32(1) << z
		for _, point := range [][2]uint32{{0, 0}, {n - 1, n - 1}, {n / 2, n / 3}} {
			gotZ, gotX, gotY := IdToZxy(ZxyToId(z, point[0], point[1]))
			if gotZ != z || gotX != point[0] || gotY != point[1] {
				t.Fatalf("Hilbert round trip failed for %d/%d/%d", z, point[0], point[1])
			}
		}
	}
}

func TestHeaderV3KnownLayoutAndRoundTrip(t *testing.T) {
	h := HeaderV3{SpecVersion: 3, RootOffset: 127, RootLength: 52, MetadataOffset: 179, MetadataLength: 35, LeafDirectoryOffset: 214, LeafDirectoryLength: 99, TileDataOffset: 313, TileDataLength: 1000, AddressedTilesCount: 10, TileEntriesCount: 8, TileContentsCount: 6, Clustered: true, InternalCompression: Gzip, TileCompression: NoCompression, TileType: Png, MinZoom: 1, MaxZoom: 5, MinLonE7: -1800000000, MinLatE7: -850000000, MaxLonE7: 1800000000, MaxLatE7: 850000000, CenterZoom: 3, CenterLonE7: 123456789, CenterLatE7: -23456789}
	data := SerializeHeader(h)
	if len(data) != 127 || !bytes.Equal(data[:8], []byte{'P', 'M', 'T', 'i', 'l', 'e', 's', 3}) {
		t.Fatalf("invalid PMTiles v3 prefix/length: %d", len(data))
	}
	if binary.LittleEndian.Uint64(data[56:64]) != 313 || data[96] != 1 || data[97] != 2 || data[99] != 2 {
		t.Fatal("header does not match spec offsets")
	}
	got, err := DeserializeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("header round trip:\n%+v\n%+v", got, h)
	}
	for _, bad := range [][]byte{nil, data[:126], append([]byte("BADMAGI"), data[7:]...), append(append([]byte{}, data[:7]...), append([]byte{2}, data[8:]...)...)} {
		if _, err := DeserializeHeader(bad); err == nil {
			t.Errorf("accepted malformed header of length %d", len(bad))
		}
	}
	overflow := append([]byte{}, data...)
	binary.LittleEndian.PutUint64(overflow[8:16], ^uint64(0))
	binary.LittleEndian.PutUint64(overflow[16:24], 1)
	if _, err := DeserializeHeader(overflow); err == nil {
		t.Fatal("accepted header section overflow")
	}
}

func TestDirectoryMatchesSpecVarintsAndRoundTrips(t *testing.T) {
	entries := []EntryV3{{1, 0, 10, 1}, {3, 10, 12, 1}, {9, 99, 7, 0}}
	encoded := SerializeEntries(entries)
	r, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	// entry count, ID deltas, run lengths, lengths, offsets (zero denotes the
	// previous tile's end; explicit offsets are encoded as offset+1).
	want := []byte{3, 1, 2, 6, 1, 1, 0, 10, 12, 7, 1, 0, 100}
	if !bytes.Equal(raw, want) {
		t.Fatalf("directory varints=%v want %v", raw, want)
	}
	got, err := DeserializeEntriesRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("directory=%+v want %+v", got, entries)
	}
	empty, err := DeserializeEntriesRaw([]byte{0})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty directory: %+v %v", empty, err)
	}

}

func TestDirectoryRejectsMalformedInput(t *testing.T) {
	cases := map[string][]byte{
		"empty": nil, "invalid directory": []byte("directory"),
		"count allocation":       {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		"truncated fields":       {1, 0, 1, 10},
		"duplicate IDs":          {2, 0, 0, 1, 1, 10, 10, 1, 0},
		"first offset zero":      {1, 0, 1, 10, 0},
		"empty tile":             {1, 0, 1, 0, 1},
		"trailing decoded bytes": {0, 0},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DeserializeEntriesRaw(data); err == nil {
				t.Fatal("accepted malformed directory")
			}
		})
	}
}
