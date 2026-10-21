package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	pm "tiler/internal/pmtiles"
)

// PMTiles v3 readers accept both compressed and uncompressed directories and
// tiles. Unsupported compression/types are rejected before creating output.
// Payloads are read per tile; no archive-wide tile byte slice is retained.
func archiveWalkPM(ctx context.Context, input string, visit archiveVisit) (map[string]string, error) {
	file, err := os.Open(input)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < pm.HeaderV3LenBytes {
		return nil, fmt.Errorf("PMTiles header is truncated")
	}
	headerBytes := make([]byte, pm.HeaderV3LenBytes)
	if _, err := file.ReadAt(headerBytes, 0); err != nil {
		return nil, err
	}
	header, err := pm.DeserializeHeader(headerBytes)
	if err != nil {
		return nil, err
	}
	size := uint64(info.Size())
	for _, section := range []struct{ offset, length uint64 }{{header.RootOffset, header.RootLength}, {header.MetadataOffset, header.MetadataLength}, {header.LeafDirectoryOffset, header.LeafDirectoryLength}, {header.TileDataOffset, header.TileDataLength}} {
		if section.offset > size || section.length > size-section.offset {
			return nil, fmt.Errorf("PMTiles section is outside the archive")
		}
	}
	if header.RootOffset < pm.HeaderV3LenBytes || header.RootLength == 0 || header.RootOffset+header.RootLength > 16384 {
		return nil, fmt.Errorf("PMTiles root directory must fit the first 16 KiB")
	}
	if header.MinZoom > header.MaxZoom || header.MaxZoom > 30 {
		return nil, fmt.Errorf("PMTiles input zoom must be within 0..30")
	}
	if header.InternalCompression != pm.NoCompression && header.InternalCompression != pm.Gzip {
		return nil, fmt.Errorf("unsupported PMTiles internal compression %d; supported: none and gzip", header.InternalCompression)
	}
	if header.TileCompression != pm.NoCompression && header.TileCompression != pm.Gzip {
		return nil, fmt.Errorf("unsupported PMTiles tile compression %d; supported: none and gzip", header.TileCompression)
	}
	format := ""
	switch header.TileType {
	case pm.Png:
		format = PNG
	case pm.Jpeg:
		format = JPG
	case pm.Webp:
		format = WEBP
	case pm.Mvt:
		format = PBF
	default:
		return nil, fmt.Errorf("unsupported PMTiles tile type %d", header.TileType)
	}
	readSection := func(offset, length uint64, limit uint64) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if length > limit || length > uint64(int(^uint(0)>>1)) || offset > math.MaxInt64 {
			return nil, fmt.Errorf("PMTiles section or tile exceeds safe read limits")
		}
		body := make([]byte, int(length))
		if _, err := file.ReadAt(body, int64(offset)); err != nil {
			return nil, err
		}
		return body, nil
	}
	metadataBytes, err := readSection(header.MetadataOffset, header.MetadataLength, 16<<20)
	if err != nil {
		return nil, err
	}
	metadataBytes, err = archiveDecodePMBytes(metadataBytes, header.InternalCompression, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("PMTiles metadata: %w", err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadataBytes, &values); err != nil || values == nil {
		if err == nil {
			err = fmt.Errorf("metadata must be an object")
		}
		return nil, fmt.Errorf("PMTiles metadata: %w", err)
	}
	metadata := make(map[string]string, len(values)+7)
	for key, value := range values {
		var text string
		if json.Unmarshal(value, &text) == nil {
			metadata[key] = text
		} else {
			metadata[key] = string(value)
		}
	}
	metadata["format"] = format
	metadata["bounds"] = fmt.Sprintf("%.7f,%.7f,%.7f,%.7f", float64(header.MinLonE7)/1e7, float64(header.MinLatE7)/1e7, float64(header.MaxLonE7)/1e7, float64(header.MaxLatE7)/1e7)
	metadata["center"] = fmt.Sprintf("%.7f,%.7f,%d", float64(header.CenterLonE7)/1e7, float64(header.CenterLatE7)/1e7, header.CenterZoom)
	metadata["minzoom"] = strconv.Itoa(int(header.MinZoom))
	metadata["maxzoom"] = strconv.Itoa(int(header.MaxZoom))
	delete(metadata, "tile_path")
	maxID := pm.ZxyToId(31, 0, 0)
	var previousEnd uint64
	var hasPrevious bool
	var addressed uint64
	const directoryMemoryLimit uint64 = 64 << 20
	const directoryEntryLimit uint64 = 1 << 20
	var liveDirectoryBytes, liveDirectoryEntries uint64
	active := map[uint64]bool{}
	var walkDirectory func(offset, length uint64, depth int) error
	walkDirectory = func(offset, length uint64, depth int) error {
		if depth > 8 {
			return fmt.Errorf("PMTiles directory nesting is too deep")
		}
		if active[offset] {
			return fmt.Errorf("PMTiles directory cycle")
		}
		active[offset] = true
		defer delete(active, offset)
		data, err := readSection(offset, length, directoryMemoryLimit-liveDirectoryBytes)
		if err != nil {
			return err
		}
		entries, memory, err := archiveReadPMDirectory(data, header.InternalCompression, directoryMemoryLimit-liveDirectoryBytes, directoryEntryLimit-liveDirectoryEntries)
		if err != nil {
			return err
		}
		liveDirectoryBytes += memory
		liveDirectoryEntries += uint64(len(entries))
		defer func() { liveDirectoryBytes -= memory; liveDirectoryEntries -= uint64(len(entries)) }()

		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.RunLength == 0 {
				if entry.Offset > header.LeafDirectoryLength || uint64(entry.Length) > header.LeafDirectoryLength-entry.Offset {
					return fmt.Errorf("PMTiles leaf directory is outside its section")
				}
				if err := walkDirectory(header.LeafDirectoryOffset+entry.Offset, uint64(entry.Length), depth+1); err != nil {
					return err
				}
				continue
			}
			end := entry.TileId + uint64(entry.RunLength)
			if entry.TileId >= maxID || end < entry.TileId || end > maxID {
				return fmt.Errorf("PMTiles tile ID is outside zoom 0..30")
			}
			if hasPrevious && entry.TileId < previousEnd {
				return fmt.Errorf("PMTiles tile runs overlap or are not ordered")
			}
			if entry.Offset > header.TileDataLength || uint64(entry.Length) > header.TileDataLength-entry.Offset {
				return fmt.Errorf("PMTiles tile is outside its data section")
			}
			body, err := readSection(header.TileDataOffset+entry.Offset, uint64(entry.Length), maxTileDataBytes)
			if err != nil {
				return err
			}
			body, err = archiveDecodePMBytes(body, header.TileCompression, maxTileDataBytes)
			if err != nil {
				return fmt.Errorf("PMTiles tile: %w", err)
			}
			for id := entry.TileId; id < end; id++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				z, x, y := pm.IdToZxy(id)
				if z < header.MinZoom || z > header.MaxZoom {
					return fmt.Errorf("PMTiles tile zoom lies outside its header range")
				}
				if err := visit(int(z), x, y, body, format); err != nil {
					return err
				}
				addressed++
			}
			previousEnd = end
			hasPrevious = true
		}
		return nil
	}
	if err := walkDirectory(header.RootOffset, header.RootLength, 0); err != nil {
		return nil, err
	}
	if addressed == 0 {
		return nil, fmt.Errorf("PMTiles contains no tiles")
	}
	if addressed != header.AddressedTilesCount {
		return nil, fmt.Errorf("PMTiles addressed tile count does not match its directory")
	}
	return metadata, nil
}

// Account for compressed input, the bounded preflight decoding, the codec's
// decoding and its entry allocation before entering another directory level.
// Returning releases the budget so an archive may contain arbitrarily many
// leaves while its live directory working set stays bounded on 32-bit builds.
func archiveReadPMDirectory(data []byte, compression pm.Compression, budget, entryBudget uint64) ([]pm.EntryV3, uint64, error) {
	if budget < uint64(len(data)) {
		return nil, 0, fmt.Errorf("PMTiles live directory memory limit exceeded")
	}
	decodeLimit := int64((budget - uint64(len(data))) / 2)
	decoded, err := archiveDecodePMBytes(data, compression, decodeLimit)
	if err != nil {
		return nil, 0, err
	}
	if uint64(len(decoded)) > uint64(decodeLimit) {
		return nil, 0, fmt.Errorf("PMTiles live directory memory limit exceeded")
	}
	count, err := binary.ReadUvarint(bytes.NewReader(decoded))
	if err != nil || count == 0 || count > entryBudget || count > uint64(len(decoded)/4) {
		return nil, 0, fmt.Errorf("PMTiles live directory entry limit exceeded or invalid count")
	}
	encoded := data
	if compression == pm.NoCompression {
		// Gzip can be slightly larger than its input; leave fixed headroom
		// before constructing the codec's canonical gzip representation.
		if uint64(len(decoded))*3+count*24+65536 > budget {
			return nil, 0, fmt.Errorf("PMTiles live directory memory limit exceeded")
		}
		encoded, err = archiveGzip(decoded)
		if err != nil {
			return nil, 0, err
		}
	}
	memory := uint64(len(encoded)) + uint64(len(decoded))*2 + count*24
	if memory > budget {
		return nil, 0, fmt.Errorf("PMTiles live directory memory limit exceeded")
	}
	entries, err := pm.DeserializeEntries(encoded)
	if err != nil {
		return nil, 0, err
	}
	return entries, memory, nil
}

func archiveDecodePMBytes(body []byte, compression pm.Compression, limit int64) ([]byte, error) {
	if compression == pm.NoCompression {
		return body, nil
	}
	if compression != pm.Gzip {
		return nil, fmt.Errorf("unsupported PMTiles compression")
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	decoded, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(decoded)) > limit {
		return nil, fmt.Errorf("PMTiles decompressed data exceeds safe read limit")
	}
	return decoded, nil
}
