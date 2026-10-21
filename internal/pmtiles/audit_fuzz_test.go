package pmtiles

import (
	"bytes"
	"compress/gzip"
	"io"
	"reflect"
	"testing"
)

func FuzzDirectoryAuditRoundTrip(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1, 1})
	f.Add([]byte{2, 5, 4, 2, 0, 3, 20, 43, 1})
	f.Add([]byte{1, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		entries, err := DeserializeEntriesRaw(data)
		if err != nil {
			return
		}
		reader, err := gzip.NewReader(bytes.NewReader(SerializeEntries(entries)))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		again, err := DeserializeEntriesRaw(raw)
		if err != nil || !reflect.DeepEqual(entries, again) {
			t.Fatalf("accepted directory failed canonical round trip: %v", err)
		}
	})
}

func FuzzHilbertAuditRoundTrip(f *testing.F) {
	f.Add(uint8(0), uint32(0), uint32(0))
	f.Add(uint8(30), uint32(1073741823), uint32(1073741823))
	f.Fuzz(func(t *testing.T, z uint8, x, y uint32) {
		z %= 31
		n := uint32(1) << z
		x &= n - 1
		y &= n - 1
		id := ZxyToId(z, x, y)
		zz, xx, yy := IdToZxy(id)
		if zz != z || xx != x || yy != y {
			t.Fatalf("Hilbert mapping changed %d/%d/%d to %d/%d/%d", z, x, y, zz, xx, yy)
		}
	})
}
