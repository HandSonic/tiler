package pmtiles

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// HeaderV3LenBytes is the fixed PMTiles v3 header size.
const HeaderV3LenBytes = HEADERV3_LEN_BYTES

// SerializeHeader writes the official PMTiles v3 binary header layout.
func SerializeHeader(header HeaderV3) []byte { return serialize_header(header) }

// SerializeEntries encodes a gzip directory. Entries must have strictly
// increasing TileId values and nonzero lengths, as required by PMTiles v3.
func SerializeEntries(entries []EntryV3) []byte { return serialize_entries(entries) }

// DeserializeHeader rejects truncated headers and versions this package cannot
// interpret before calling the original fixed-offset decoder.
func DeserializeHeader(data []byte) (HeaderV3, error) {
	if len(data) < HeaderV3LenBytes {
		return HeaderV3{}, fmt.Errorf("PMTiles header is truncated: %d bytes", len(data))
	}
	if string(data[:7]) != "PMTiles" || data[7] != 3 {
		return HeaderV3{}, fmt.Errorf("expected PMTiles version 3")
	}
	h, err := deserialize_header(data[:HeaderV3LenBytes])
	if err != nil {
		return HeaderV3{}, err
	}
	for _, section := range []struct{ offset, length uint64 }{
		{h.RootOffset, h.RootLength}, {h.MetadataOffset, h.MetadataLength},
		{h.LeafDirectoryOffset, h.LeafDirectoryLength}, {h.TileDataOffset, h.TileDataLength},
	} {
		if section.length > math.MaxUint64-section.offset {
			return HeaderV3{}, fmt.Errorf("PMTiles section offset overflow")
		}
	}
	return h, nil
}

// DeserializeEntries reads one gzip directory with bounded allocation and
// validates varints, ascending IDs, field widths, offsets, gzip integrity and
// trailing data. It is suitable for inspecting the directories we generate.
func DeserializeEntries(data []byte) ([]EntryV3, error) {
	const maxDecodedBytes = 64 << 20
	const maxEntries = 2 << 20
	raw := bytes.NewReader(data)
	reader, err := gzip.NewReader(raw)
	if err != nil {
		return nil, fmt.Errorf("read PMTiles gzip directory: %w", err)
	}
	reader.Multistream(false)
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDecodedBytes+1))
	closeErr := reader.Close()
	if err != nil {
		return nil, fmt.Errorf("decode PMTiles directory: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close PMTiles directory: %w", closeErr)
	}
	if len(decoded) > maxDecodedBytes {
		return nil, fmt.Errorf("PMTiles directory exceeds %d decoded bytes", maxDecodedBytes)
	}
	if raw.Len() != 0 {
		return nil, fmt.Errorf("trailing gzip data in PMTiles directory")
	}
	input := bytes.NewReader(decoded)
	count, err := binary.ReadUvarint(input)
	if err != nil {
		return nil, fmt.Errorf("read PMTiles directory count: %w", err)
	}
	// Every entry needs at least four one-byte varints. Check this before
	// allocation so a forged count cannot force a large allocation.
	if count > maxEntries || count > uint64(input.Len()/4) {
		return nil, fmt.Errorf("invalid PMTiles directory entry count %d", count)
	}
	entries := make([]EntryV3, int(count))
	read := func() (uint64, error) {
		v, e := binary.ReadUvarint(input)
		if e != nil {
			return 0, fmt.Errorf("invalid PMTiles directory varint: %w", e)
		}
		return v, nil
	}
	var last uint64
	for i := range entries {
		delta, e := read()
		if e != nil {
			return nil, e
		}
		if delta > math.MaxUint64-last || (i > 0 && delta == 0) {
			return nil, fmt.Errorf("PMTiles directory tile IDs are not strictly increasing")
		}
		entries[i].TileId = last + delta
		last = entries[i].TileId
	}
	for i := range entries {
		run, e := read()
		if e != nil {
			return nil, e
		}
		if run > math.MaxUint32 {
			return nil, fmt.Errorf("PMTiles run length overflows uint32")
		}
		entries[i].RunLength = uint32(run)
	}
	for i := range entries {
		length, e := read()
		if e != nil {
			return nil, e
		}
		if length == 0 || length > math.MaxUint32 {
			return nil, fmt.Errorf("invalid PMTiles entry length %d", length)
		}
		entries[i].Length = uint32(length)
	}
	for i := range entries {
		encoded, e := read()
		if e != nil {
			return nil, e
		}
		if encoded == 0 {
			if i == 0 {
				return nil, fmt.Errorf("invalid first PMTiles entry offset")
			}
			previous := entries[i-1]
			if uint64(previous.Length) > math.MaxUint64-previous.Offset {
				return nil, fmt.Errorf("PMTiles entry offset overflow")
			}
			entries[i].Offset = previous.Offset + uint64(previous.Length)
		} else {
			entries[i].Offset = encoded - 1
		}
		if uint64(entries[i].Length) > math.MaxUint64-entries[i].Offset {
			return nil, fmt.Errorf("PMTiles entry range overflow")
		}
	}
	if input.Len() != 0 {
		return nil, fmt.Errorf("trailing bytes in PMTiles directory")
	}
	return entries, nil
}
