package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pm "tiler/internal/pmtiles"
)

// ArchiveConvertOptions controls the target archive. Format, when set, is the
// target tile encoding; PNG/JPEG tiles are actually transcoded when necessary.
type ArchiveConvertOptions struct {
	Format    string
	Overwrite bool
	Metadata  map[string]string
}

type ArchiveConvertResult struct {
	Tiles  int64
	Format string
}

type archiveExtent struct {
	count                    int64
	minZ, maxZ               int
	west, south, east, north float64
}

func (extent *archiveExtent) add(z int, x, y uint32) {
	n := math.Exp2(float64(z))
	west, east := float64(x)/n*360-180, (float64(x)+1)/n*360-180
	north := math.Atan(math.Sinh(math.Pi*(1-2*float64(y)/n))) * 180 / math.Pi
	south := math.Atan(math.Sinh(math.Pi*(1-2*(float64(y)+1)/n))) * 180 / math.Pi
	if extent.count == 0 {
		extent.minZ, extent.maxZ = z, z
		extent.west, extent.south, extent.east, extent.north = west, south, east, north
	} else {
		if z < extent.minZ {
			extent.minZ = z
		}
		if z > extent.maxZ {
			extent.maxZ = z
		}
		extent.west = math.Min(extent.west, west)
		extent.east = math.Max(extent.east, east)
		extent.south = math.Min(extent.south, south)
		extent.north = math.Max(extent.north, north)
	}
	extent.count++
}

func archiveFormat(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "":
		return "", nil
	case "png":
		return PNG, nil
	case "jpg", "jpeg":
		return JPG, nil
	case "pbf", "mvt":
		return PBF, nil
	case "webp":
		return WEBP, nil
	default:
		return "", fmt.Errorf("unsupported archive tile format %q (use png, jpg, pbf or webp)", format)
	}
}

func archiveCoordinate(z int64, x, y int64) error {
	if z < 0 || z > 30 || x < 0 || y < 0 || x >= int64(1)<<uint(z) || y >= int64(1)<<uint(z) {
		return fmt.Errorf("invalid tile coordinate z=%d x=%d y=%d (expected z 0..30 and x/y within the zoom grid)", z, x, y)
	}
	return nil
}

// ConvertArchive converts an XYZ directory or an MBTiles file into MBTiles or
// PMTiles v3. Large tile payloads are processed one at a time; PMTiles ordering
// uses a temporary SQLite index, rather than an in-memory set of all tiles.
func ConvertArchive(ctx context.Context, input, output string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var result ArchiveConvertResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	target := strings.ToLower(filepath.Ext(output))
	if target != ".mbtiles" && target != ".pmtiles" {
		return result, fmt.Errorf("output must end in .mbtiles or .pmtiles")
	}
	format, err := archiveFormat(opts.Format)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(input)
	if err != nil {
		return result, fmt.Errorf("open archive input: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return result, fmt.Errorf("archive input must be an XYZ directory or MBTiles file")
	}
	if dst, err := os.Lstat(output); err == nil {
		if !opts.Overwrite {
			return result, fmt.Errorf("output already exists: %s (use -overwrite to replace it)", output)
		}
		if !dst.Mode().IsRegular() {
			return result, fmt.Errorf("output must be a regular file, not a directory or symlink")
		}
		if os.SameFile(info, dst) {
			return result, fmt.Errorf("input and output refer to the same file")
		}
	} else if !os.IsNotExist(err) {
		return result, err
	}
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0755); err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(filepath.Dir(outputAbs), ".tiler-convert-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	staged := filepath.Join(work, "archive"+target)
	if info.IsDir() {
		mbtiles := staged
		if target == ".pmtiles" {
			mbtiles = filepath.Join(work, "input.mbtiles")
		}
		result, err = archiveDirectoryToMBTiles(ctx, input, mbtiles, format, opts.Metadata)
		if err != nil {
			return result, err
		}
		if target == ".pmtiles" {
			result, err = archiveMBTilesToPMTiles(ctx, mbtiles, staged, work, "", nil)
		}
	} else {
		if target != ".pmtiles" {
			return result, fmt.Errorf("MBTiles input requires a .pmtiles output; XYZ directories can produce either archive type")
		}
		result, err = archiveMBTilesToPMTiles(ctx, input, staged, work, format, opts.Metadata)
	}
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Overwrite {
		if err := os.Rename(staged, outputAbs); err != nil {
			return result, fmt.Errorf("publish archive: %w", err)
		}
	} else {
		if err := archivePublishNoReplace(staged, outputAbs); err != nil {
			return result, fmt.Errorf("publish archive without overwriting: %w", err)
		}
	}
	return result, nil
}

func archiveOpenSQLite(path string, readOnly bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", archiveSQLiteURI(absolute, readOnly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// archiveSQLiteURI keeps the URI authority empty, including UNC paths. SQLite
// expects file:///C:/... for drives and file:////server/share/... for UNC paths.
// Keeping this independent of filepath allows Windows paths to be tested on Unix.
func archiveSQLiteURI(absolute string, readOnly bool) string {
	path := absolute
	if len(path) >= 2 && path[1] == ':' && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) {
		path = "/" + strings.ReplaceAll(path, `\`, "/")
	} else if strings.HasPrefix(path, `\\`) {
		path = strings.ReplaceAll(path, `\`, "/")
	}
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	if readOnly {
		query.Set("mode", "ro")
	}
	query.Set("_busy_timeout", "5000")
	u.RawQuery = query.Encode()
	return u.String()
}

func archiveReadDirectoryMetadata(dir string) (map[string]string, error) {
	metadata := make(map[string]string)
	file, err := os.Open(filepath.Join(dir, "metadata.json"))
	if os.IsNotExist(err) {
		return metadata, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 16*1024*1024 {
		return nil, fmt.Errorf("metadata.json exceeds 16 MiB")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("read metadata.json: %w", err)
	}
	for key, value := range values {
		var text string
		if json.Unmarshal(value, &text) == nil {
			metadata[key] = text
		} else {
			metadata[key] = string(value)
		}
	}
	return metadata, nil
}

func archiveDirectoryToMBTiles(ctx context.Context, input, output, targetFormat string, overrides map[string]string) (result ArchiveConvertResult, err error) {
	metadata, err := archiveReadDirectoryMetadata(input)
	if err != nil {
		return result, err
	}
	for k, v := range overrides {
		metadata[k] = v
	}
	formatChosen := targetFormat != ""
	if targetFormat == "" {
		targetFormat, err = archiveFormat(metadata["format"])
		if err != nil {
			return result, err
		}
	}
	db, err := archiveOpenSQLite(output, false)
	if err != nil {
		return result, err
	}
	defer func() {
		closeErr := db.Close()
		if err == nil {
			err = closeErr
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, query := range []string{
		"CREATE TABLE metadata (name TEXT PRIMARY KEY, value TEXT)",
		"CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB, PRIMARY KEY(zoom_level,tile_column,tile_row))",
	} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return result, err
		}
	}
	insert, err := tx.PrepareContext(ctx, "INSERT INTO tiles (zoom_level,tile_column,tile_row,tile_data) VALUES (?,?,?,?)")
	if err != nil {
		return result, err
	}
	defer insert.Close()
	extent := archiveExtent{}
	err = filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not supported in XYZ input: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(entry.Name())), ".")
		sourceFormat, parseErr := archiveFormat(extension)
		if parseErr != nil || sourceFormat == "" {
			return nil
		} // metadata, failure manifests and unrelated files are not tiles.
		relative, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 3 {
			return fmt.Errorf("invalid XYZ tile path %s: expected z/x/y.ext", relative)
		}
		z, ze := strconv.ParseInt(parts[0], 10, 64)
		x, xe := strconv.ParseInt(parts[1], 10, 64)
		y, ye := strconv.ParseInt(strings.TrimSuffix(parts[2], filepath.Ext(parts[2])), 10, 64)
		if ze != nil || xe != nil || ye != nil {
			return fmt.Errorf("invalid XYZ tile path %s: coordinates must be integers", relative)
		}
		if err := archiveCoordinate(z, x, y); err != nil {
			return fmt.Errorf("%s: %w", relative, err)
		}
		if targetFormat == "" {
			targetFormat = sourceFormat
		} else if !formatChosen && sourceFormat != targetFormat {
			return fmt.Errorf("mixed tile formats in XYZ input; set -format to choose the target encoding")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body, err = archivePrepareTileData(body, sourceFormat, targetFormat)
		if err != nil {
			return fmt.Errorf("tile %s: %w", relative, err)
		}
		tmsY := (int64(1) << uint(z)) - 1 - y
		if _, err := insert.ExecContext(ctx, z, x, tmsY, body); err != nil {
			return fmt.Errorf("insert XYZ tile %s (check for duplicate coordinates): %w", relative, err)
		}
		extent.add(int(z), uint32(x), uint32(y))
		return nil
	})
	if err != nil {
		return result, err
	}
	if extent.count == 0 {
		return result, fmt.Errorf("XYZ input contains no supported tiles")
	}
	if err := archiveCompleteMetadata(metadata, targetFormat, filepath.Base(filepath.Clean(input)), extent); err != nil {
		return result, err
	}
	for key, value := range metadata {
		if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(name,value) VALUES (?,?)", key, value); err != nil {
			return result, err
		}
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return ArchiveConvertResult{Tiles: extent.count, Format: targetFormat}, nil
}

func archiveReadMBMetadata(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name,value FROM metadata")
	if err != nil {
		return nil, fmt.Errorf("read MBTiles metadata: %w", err)
	}
	defer rows.Close()
	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		metadata[key] = value
	}
	return metadata, rows.Err()
}

func archiveCompleteMetadata(metadata map[string]string, format, name string, extent archiveExtent) error {
	metadata["format"] = format
	metadata["minzoom"] = strconv.Itoa(extent.minZ)
	metadata["maxzoom"] = strconv.Itoa(extent.maxZ)
	metadata["scheme"] = "tms"
	if metadata["name"] == "" {
		metadata["name"] = name
	}
	if metadata["version"] == "" {
		metadata["version"] = "1.3"
	}
	if metadata["type"] != "overlay" && metadata["type"] != "baselayer" {
		metadata["type"] = "baselayer"
	}
	bounds := []float64{extent.west, extent.south, extent.east, extent.north}
	if text := metadata["bounds"]; text != "" {
		parsed, err := archiveNumbers(text, 4)
		if err != nil {
			return fmt.Errorf("invalid bounds metadata: %w", err)
		}
		if parsed[0] < -180 || parsed[2] > 180 || parsed[1] < -90 || parsed[3] > 90 || parsed[0] > parsed[2] || parsed[1] > parsed[3] {
			return fmt.Errorf("bounds metadata is outside the geographic range")
		}
		if parsed[0] != parsed[2] && parsed[1] != parsed[3] {
			bounds = parsed
		}
	}
	metadata["bounds"] = fmt.Sprintf("%.7f,%.7f,%.7f,%.7f", bounds[0], bounds[1], bounds[2], bounds[3])
	if metadata["center"] == "" {
		metadata["center"] = fmt.Sprintf("%.7f,%.7f,%d", (bounds[0]+bounds[2])/2, (bounds[1]+bounds[3])/2, extent.minZ)
	} else {
		center, err := archiveNumbers(metadata["center"], 3)
		if err != nil {
			return fmt.Errorf("invalid center metadata: %w", err)
		}
		if center[0] < -180 || center[0] > 180 || center[1] < -90 || center[1] > 90 || center[2] < 0 || center[2] > 30 || center[2] != math.Trunc(center[2]) {
			return fmt.Errorf("center metadata is outside the geographic/zoom range")
		}
		if center[2] < float64(extent.minZ) || center[2] > float64(extent.maxZ) {
			center[2] = float64(extent.minZ)
		}
		metadata["center"] = fmt.Sprintf("%.7f,%.7f,%.0f", center[0], center[1], center[2])
	}
	return nil
}

func archiveNumbers(text string, count int) ([]float64, error) {
	parts := strings.Split(text, ",")
	if len(parts) != count {
		return nil, fmt.Errorf("expected %d comma-separated numbers", count)
	}
	result := make([]float64, count)
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("invalid number %q", part)
		}
		result[i] = value
	}
	return result, nil
}

func archiveMBTilesToPMTiles(ctx context.Context, input, output, work, targetFormat string, overrides map[string]string) (result ArchiveConvertResult, err error) {
	source, err := archiveOpenSQLite(input, true)
	if err != nil {
		return result, fmt.Errorf("open MBTiles input: %w", err)
	}
	defer source.Close()
	sourceTx, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer sourceTx.Rollback()
	metadata, err := archiveReadMBMetadata(ctx, sourceTx)
	if err != nil {
		return result, err
	}
	for k, v := range overrides {
		metadata[k] = v
	}
	sourceFormat, sourceFormatErr := archiveFormat(metadata["format"])
	if targetFormat == "" {
		targetFormat, err = sourceFormat, sourceFormatErr
		if err != nil {
			return result, err
		}
	}
	index, err := archiveOpenSQLite(filepath.Join(work, "tile-index.sqlite"), false)
	if err != nil {
		return result, err
	}
	defer index.Close()
	if _, err = index.ExecContext(ctx, "CREATE TABLE tile_index (id INTEGER PRIMARY KEY,z INTEGER,x INTEGER,y INTEGER)"); err != nil {
		return result, err
	}
	tx, err := index.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, "INSERT INTO tile_index(id,z,x,y) VALUES (?,?,?,?)")
	if err != nil {
		return result, err
	}
	rows, err := sourceTx.QueryContext(ctx, "SELECT zoom_level,tile_column,tile_row FROM tiles")
	if err != nil {
		insert.Close()
		return result, fmt.Errorf("read MBTiles tiles: %w", err)
	}
	extent := archiveExtent{}
	for rows.Next() {
		var z, x, y int64
		if err = rows.Scan(&z, &x, &y); err != nil {
			break
		}
		if err = archiveCoordinate(z, x, y); err != nil {
			break
		}
		if z > 26 {
			err = fmt.Errorf("PMTiles output supports zoom 0..26 for compatibility with the official JavaScript reader; found zoom %d (MBTiles output supports 0..30)", z)
			break
		}
		xyzY := (int64(1) << uint(z)) - 1 - y
		if _, err = insert.ExecContext(ctx, int64(pm.ZxyToId(uint8(z), uint32(x), uint32(xyzY))), z, x, y); err != nil {
			err = fmt.Errorf("duplicate MBTiles coordinate or invalid tile index: %w", err)
			break
		}
		extent.add(int(z), uint32(x), uint32(xyzY))
	}
	scanErr := rows.Err()
	rows.Close()
	insert.Close()
	if err != nil {
		return result, err
	}
	if scanErr != nil {
		return result, scanErr
	}
	if extent.count == 0 {
		return result, fmt.Errorf("MBTiles archive contains no tiles")
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	outputFile, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer func() {
		closeErr := outputFile.Close()
		if err == nil {
			err = closeErr
		}
	}()
	const rootArea = 16384
	if _, err = outputFile.Seek(rootArea, io.SeekStart); err != nil {
		return result, err
	}
	leafFile, err := os.CreateTemp(work, "leaf-directory-*")
	if err != nil {
		return result, err
	}
	defer leafFile.Close()
	// At most 512 leaf pointers make the compressed root fit into the first
	// 16 KiB even when offsets/TileIDs need their maximum varint widths.
	leafSize64 := (extent.count + 511) / 512
	if leafSize64 < 4096 {
		leafSize64 = 4096
	}
	if leafSize64 > int64(int(^uint(0)>>1))/32 {
		return result, fmt.Errorf("archive directory is too large for this build architecture")
	}
	leafSize := int(leafSize64)
	useLeaves := extent.count > 512
	entries := make([]pm.EntryV3, 0, leafSize)
	rootEntries := make([]pm.EntryV3, 0, 512)
	var tileOffset, leafOffset uint64
	flushLeaf := func() error {
		if len(entries) == 0 {
			return nil
		}
		data := pm.SerializeEntries(entries)
		if uint64(len(data)) > uint64(^uint32(0)) {
			return fmt.Errorf("PMTiles leaf directory exceeds format limits")
		}
		if _, err := leafFile.Write(data); err != nil {
			return err
		}
		rootEntries = append(rootEntries, pm.EntryV3{TileId: entries[0].TileId, Offset: leafOffset, Length: uint32(len(data)), RunLength: 0})
		leafOffset += uint64(len(data))
		entries = entries[:0]
		return nil
	}
	ids, err := index.QueryContext(ctx, "SELECT id,z,x,y FROM tile_index ORDER BY id")
	if err != nil {
		return result, err
	}
	defer ids.Close()
	for ids.Next() {
		var id, z, x, tmsY int64
		if err = ids.Scan(&id, &z, &x, &tmsY); err != nil {
			return result, err
		}
		var body []byte
		if err = sourceTx.QueryRowContext(ctx, "SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?", z, x, tmsY).Scan(&body); err != nil {
			return result, err
		}
		if targetFormat == "" {
			targetFormat, err = archiveInferFormat(body)
			if err != nil {
				return result, err
			}
		}
		body, err = archivePrepareTileData(body, sourceFormat, targetFormat)
		if err != nil {
			return result, fmt.Errorf("convert MBTiles tile %d/%d/%d: %w", z, x, tmsY, err)
		}
		if uint64(len(body)) > uint64(^uint32(0)) {
			return result, fmt.Errorf("tile exceeds PMTiles 4 GiB content limit")
		}
		if _, err = outputFile.Write(body); err != nil {
			return result, err
		}
		entries = append(entries, pm.EntryV3{TileId: uint64(id), Offset: tileOffset, Length: uint32(len(body)), RunLength: 1})
		tileOffset += uint64(len(body))
		if useLeaves && len(entries) == leafSize {
			if err = flushLeaf(); err != nil {
				return result, err
			}
		}
	}
	if err = ids.Err(); err != nil {
		return result, err
	}
	ids.Close()
	if useLeaves {
		if err = flushLeaf(); err != nil {
			return result, err
		}
	} else {
		rootEntries = entries
	}
	if err = archiveCompleteMetadata(metadata, targetFormat, strings.TrimSuffix(filepath.Base(input), filepath.Ext(input)), extent); err != nil {
		return result, err
	}
	jsonMetadata := make(map[string]interface{})
	if value := strings.TrimSpace(metadata["json"]); value != "" {
		if err = json.Unmarshal([]byte(value), &jsonMetadata); err != nil {
			return result, fmt.Errorf("invalid vector JSON metadata: %w", err)
		}
		if jsonMetadata == nil {
			return result, fmt.Errorf("MBTiles metadata json must be an object")
		}
	}
	// Explicit metadata entries override values from the optional JSON object.
	// Merge in this order so output does not depend on Go's map iteration order.
	for key, value := range metadata {
		switch key {
		case "bounds", "center", "minzoom", "maxzoom", "scheme", "json":
			continue
		default:
			jsonMetadata[key] = value
		}
	}
	jsonMetadata["scheme"] = "xyz"
	jsonMetadata["format"] = targetFormat
	// Some MBTiles producers store vector_layers directly as serialized JSON.
	if value, ok := jsonMetadata["vector_layers"].(string); ok {
		var parsed interface{}
		if err = json.Unmarshal([]byte(value), &parsed); err != nil {
			return result, fmt.Errorf("invalid vector_layers metadata: %w", err)
		}
		jsonMetadata["vector_layers"] = parsed
	}
	if targetFormat == PBF {
		if err = archiveValidateVectorLayers(jsonMetadata["vector_layers"]); err != nil {
			return result, err
		}
	}
	metadataBytes, err := json.Marshal(jsonMetadata)
	if err != nil {
		return result, err
	}
	metadataBytes, err = archiveGzip(metadataBytes)
	if err != nil {
		return result, err
	}
	rootBytes := pm.SerializeEntries(rootEntries)
	if len(rootBytes)+pm.HeaderV3LenBytes > rootArea {
		return result, fmt.Errorf("PMTiles root directory exceeds first 16 KiB")
	}
	metadataOffset := uint64(rootArea) + tileOffset
	if _, err = outputFile.Write(metadataBytes); err != nil {
		return result, err
	}
	if _, err = leafFile.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	if err = archiveCopy(ctx, outputFile, leafFile); err != nil {
		return result, err
	}
	header := pm.HeaderV3{SpecVersion: 3, RootOffset: pm.HeaderV3LenBytes, RootLength: uint64(len(rootBytes)), MetadataOffset: metadataOffset, MetadataLength: uint64(len(metadataBytes)), LeafDirectoryOffset: metadataOffset + uint64(len(metadataBytes)), LeafDirectoryLength: leafOffset, TileDataOffset: rootArea, TileDataLength: tileOffset, AddressedTilesCount: uint64(extent.count), TileEntriesCount: uint64(extent.count), TileContentsCount: uint64(extent.count), Clustered: true, InternalCompression: pm.Gzip, TileCompression: pm.NoCompression, MinZoom: uint8(extent.minZ), MaxZoom: uint8(extent.maxZ)}
	switch targetFormat {
	case PNG:
		header.TileType = pm.Png
	case JPG:
		header.TileType = pm.Jpeg
	case WEBP:
		header.TileType = pm.Webp
	case PBF:
		header.TileType = pm.Mvt
		header.TileCompression = pm.Gzip
	}
	bounds, _ := archiveNumbers(metadata["bounds"], 4)
	center, _ := archiveNumbers(metadata["center"], 3)
	header.MinLonE7 = int32(math.Round(bounds[0] * 1e7))
	header.MinLatE7 = int32(math.Round(bounds[1] * 1e7))
	header.MaxLonE7 = int32(math.Round(bounds[2] * 1e7))
	header.MaxLatE7 = int32(math.Round(bounds[3] * 1e7))
	header.CenterLonE7 = int32(math.Round(center[0] * 1e7))
	header.CenterLatE7 = int32(math.Round(center[1] * 1e7))
	header.CenterZoom = uint8(center[2])
	if _, err = outputFile.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	if _, err = outputFile.Write(pm.SerializeHeader(header)); err != nil {
		return result, err
	}
	if _, err = outputFile.Write(rootBytes); err != nil {
		return result, err
	}
	if err = outputFile.Sync(); err != nil {
		return result, err
	}
	if err = sourceTx.Commit(); err != nil {
		return result, err
	}
	return ArchiveConvertResult{Tiles: extent.count, Format: targetFormat}, nil
}

func archiveValidateVectorLayers(value interface{}) error {
	layers, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("PBF PMTiles requires vector_layers metadata as a JSON array (provide it in metadata.json or the MBTiles metadata json entry)")
	}
	for i, value := range layers {
		layer, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("vector_layers[%d] must be an object with id and fields", i)
		}
		id, ok := layer["id"].(string)
		if !ok {
			return fmt.Errorf("vector_layers[%d] must have a string id", i)
		}
		fields, ok := layer["fields"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("vector_layers[%d] (%s) must have a fields object", i, id)
		}
		for name, field := range fields {
			if _, ok := field.(string); !ok {
				return fmt.Errorf("vector_layers[%d] field %q must describe its type with a string", i, name)
			}
		}
	}
	return nil
}

func archivePrepareTileData(body []byte, sourceFormat, targetFormat string) ([]byte, error) {
	if sourceFormat != "" && (sourceFormat == PBF) != (targetFormat == PBF) {
		return nil, fmt.Errorf("cannot convert between raster images and PBF vector tiles (%s to %s); only PNG/JPEG image transcoding is supported", sourceFormat, targetFormat)
	}
	if targetFormat == PBF {
		probe := body
		if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
			if reader, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				probe, _ = io.ReadAll(io.LimitReader(reader, 512))
				reader.Close()
			}
		}
		if strings.HasPrefix(http.DetectContentType(probe), "image/") {
			return nil, fmt.Errorf("cannot store raster image data as PBF vector tiles")
		}
	}
	return prepareTileData(body, targetFormat, true)
}

func archiveInferFormat(body []byte) (string, error) {
	switch http.DetectContentType(body) {
	case "image/png":
		return PNG, nil
	case "image/jpeg":
		return JPG, nil
	case "image/webp":
		return WEBP, nil
	}
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		return PBF, nil
	}
	return "", fmt.Errorf("MBTiles metadata has no format and tile format cannot be identified; set -format")
}

func archiveGzip(data []byte) ([]byte, error) {
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func archiveCopy(ctx context.Context, output io.Writer, input io.Reader) error {
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := input.Read(buffer)
		if n > 0 {
			if _, writeErr := output.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
