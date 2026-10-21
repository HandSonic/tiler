package pmtiles

import (
	"reflect"
	"testing"
)

func FuzzDirectoryAuditRoundTrip(f *testing.F) {
	f.Add(SerializeEntries([]EntryV3{{TileId: 0, Offset: 0, Length: 1, RunLength: 1}}))
	f.Add(SerializeEntries([]EntryV3{{TileId: 5, Offset: 42, Length: 3, RunLength: 2}, {TileId: 9, Offset: 0, Length: 20, RunLength: 0}}))
	f.Add([]byte{0x1f, 0x8b})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		entries, err := DeserializeEntries(data)
		if err != nil {
			return
		}
		again, err := DeserializeEntries(SerializeEntries(entries))
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
		if z > 0 && ParentId(id) != ZxyToId(z-1, x/2, y/2) {
			t.Fatalf("invalid Hilbert parent for %d/%d/%d", z, x, y)
		}
	})
}
