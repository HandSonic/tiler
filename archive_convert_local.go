package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulmach/orb/maptile"
)

type archiveVisit func(z int, x, y uint32, body []byte, format string) error

// convertLocalArchive uses one temporary SQLite database as the common,
// bounded-memory representation. Archive replacement happens only after all
// input tiles and metadata have been validated and exported successfully.
func convertLocalArchive(ctx context.Context, input, output string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var result ArchiveConvertResult
	if ctx == nil {
		return result, fmt.Errorf("conversion context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Append && opts.Overwrite {
		return result, fmt.Errorf("choose either append or overwrite")
	}
	targetFormat, err := archiveFormat(opts.Format)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(input)
	if err != nil {
		return result, fmt.Errorf("open archive input: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return result, fmt.Errorf("input must be a tile directory, MBTiles or PMTiles file")
	}
	kind, err := archiveOutputKind(output, opts.OutputKind)
	if err != nil {
		return result, err
	}
	destination, err := filepath.Abs(output)
	if err != nil {
		return result, err
	}
	inputAbs, err := filepath.Abs(input)
	if err != nil {
		return result, err
	}
	resolvedInput, err := archiveResolvedPath(inputAbs)
	if err != nil {
		return result, fmt.Errorf("resolve archive input: %w", err)
	}
	resolvedDestination, err := archiveResolvedPath(destination)
	if err != nil {
		return result, fmt.Errorf("resolve archive destination: %w", err)
	}
	if info.IsDir() {
		if archivePathContains(resolvedInput, resolvedDestination) {
			return result, fmt.Errorf("output cannot be inside the input tile directory")
		}
	}
	if kind == "file" {
		if archivePathContains(resolvedDestination, resolvedInput) {
			return result, fmt.Errorf("output tile directory cannot contain the input path")
		}
	}
	existing := false
	if dst, statErr := os.Lstat(destination); statErr == nil {
		existing = true
		if !opts.Overwrite && !opts.Append {
			return result, fmt.Errorf("output already exists: %s (use -overwrite or -append)", output)
		}
		if (kind == "file" && !dst.IsDir()) || (kind != "file" && !dst.Mode().IsRegular()) {
			return result, fmt.Errorf("output type does not match destination; symbolic links are not supported")
		}
		if os.SameFile(info, dst) {
			return result, fmt.Errorf("input and output refer to the same file or directory")
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	if existing && kind != "file" {
		if err := archiveCheckArchiveDestination(destination, kind); err != nil {
			return result, err
		}
	}
	outputPattern := opts.OutputTilePath
	var previousFileMetadata map[string]string
	if kind == "file" {
		if existing && opts.Append {
			previousFileMetadata, err = archiveReadDirectoryMetadata(destination)
			if err != nil {
				return result, err
			}
			originalFormat, err := archiveFormat(previousFileMetadata["format"])
			if err != nil {
				return result, err
			}
			if targetFormat == "" {
				targetFormat = originalFormat
			}
			if originalFormat != "" && targetFormat != originalFormat {
				return result, fmt.Errorf("directory append cannot change the existing tile format")
			}
			oldPattern, e := normalizeTilePath(previousFileMetadata["tile_path"])
			if e != nil {
				return result, e
			}
			if outputPattern == "" {
				outputPattern = oldPattern
			}
			pattern, e := normalizeTilePath(outputPattern)
			if e != nil {
				return result, e
			}
			if pattern != oldPattern {
				return result, fmt.Errorf("append tile path must match the existing directory metadata")
			}
			outputPattern = pattern
		} else {
			outputPattern, err = normalizeTilePath(outputPattern)
			if err != nil {
				return result, err
			}
		}
		if err := archiveCheckFilePattern(outputPattern); err != nil {
			return result, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return result, err
	}
	work, err := os.MkdirTemp(filepath.Dir(destination), ".tiler-convert-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(work)
	stage := filepath.Join(work, "combined.mbtiles")
	db, err := archiveOpenSQLite(stage, false)
	if err != nil {
		return result, err
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, query := range []string{
		"CREATE TABLE metadata (name TEXT PRIMARY KEY,value TEXT)",
		"CREATE TABLE tiles (zoom_level INTEGER,tile_column INTEGER,tile_row INTEGER,tile_data BLOB,PRIMARY KEY(zoom_level,tile_column,tile_row))",
		"CREATE TABLE input_seen (z INTEGER,x INTEGER,y INTEGER,PRIMARY KEY(z,x,y))",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return result, err
		}
	}
	extent := archiveExtent{}
	var metadata map[string]string
	importOne := func(path, pattern string, keepExisting bool) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM input_seen"); err != nil {
			return err
		}
		walker := archiveWalkLocal
		existingInput := existing && opts.Append && !keepExisting
		if existingInput {
			walker = func(ctx context.Context, path, pattern string, visit archiveVisit) (map[string]string, error) {
				switch kind {
				case "mbtiles":
					return archiveWalkMB(ctx, path, visit)
				case "pmtiles":
					return archiveWalkPM(ctx, path, visit)
				default:
					return archiveWalkDirectoryCanonical(ctx, path, pattern, true, visit)
				}
			}
		}
		incomingMetadata, err := walker(ctx, path, pattern, func(z int, x, y uint32, body []byte, sourceFormat string) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO input_seen(z,x,y) VALUES(?,?,?)", z, x, y); err != nil {
				return fmt.Errorf("duplicate input tile coordinate %d/%d/%d: %w", z, x, y, err)
			}
			if sourceFormat == "" {
				// An extensionless empty placeholder has no detectable encoding.
				if existingInput && len(body) == 0 {
					return nil
				}
				sourceFormat, err = archiveInferFormat(body)
				if err != nil && targetFormat == PBF {
					sourceFormat, err = PBF, nil
				}
				if err != nil {
					return err
				}
			}
			if targetFormat == "" {
				targetFormat = sourceFormat
			}
			if existing && opts.Append && kind == "file" && !keepExisting && targetFormat != sourceFormat {
				return fmt.Errorf("directory append cannot change the existing tile format")
			}
			if opts.Format == "" && targetFormat != sourceFormat {
				return fmt.Errorf("mixed tile formats; set -format to choose the target encoding")
			}
			// Keep path/format validation for old empty tiles: silently retaining
			// a differently named placeholder would create duplicate coordinates.
			if existingInput && len(body) == 0 {
				return nil
			}
			prepared, err := archivePrepareTileData(body, sourceFormat, targetFormat)
			if err != nil {
				return fmt.Errorf("tile %d/%d/%d: %w", z, x, y, err)
			}
			query := "INSERT INTO tiles(zoom_level,tile_column,tile_row,tile_data) VALUES(?,?,?,?)"
			if keepExisting {
				query = "INSERT OR IGNORE INTO tiles(zoom_level,tile_column,tile_row,tile_data) VALUES(?,?,?,?)"
			}
			saved, err := tx.ExecContext(ctx, query, z, x, (uint64(1)<<uint(z))-1-uint64(y), prepared)
			if err != nil {
				return fmt.Errorf("store tile %d/%d/%d: %w", z, x, y, err)
			}
			n, err := saved.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				extent.add(z, x, y)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if existingInput && targetFormat == "" {
			// Preserve the declared format even when all previous tiles were
			// empty and therefore could not establish it from their bytes.
			targetFormat, err = archiveFormat(incomingMetadata["format"])
			if err != nil {
				return err
			}
		}
		if metadata == nil {
			metadata = incomingMetadata
		} else {
			metadata, err = archiveMergeMetadata(metadata, incomingMetadata)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if existing && opts.Append {
		basePattern := ""
		if kind == "file" {
			basePattern = outputPattern
		}
		if err := importOne(destination, basePattern, false); err != nil {
			return result, fmt.Errorf("read existing output: %w", err)
		}
	}
	if err := importOne(input, opts.InputTilePath, opts.Append && existing); err != nil {
		return result, err
	}
	if extent.count == 0 {
		return result, fmt.Errorf("input contains no supported tiles")
	}
	if metadata == nil {
		metadata = make(map[string]string)
	}
	if err := archiveApplyMetadata(metadata, opts.Metadata); err != nil {
		return result, err
	}
	delete(metadata, "tile_path")
	if opts.Append && existing {
		archiveUnionMetadataBounds(metadata, extent)
		delete(metadata, "center")
	}
	if err := archiveCompleteMetadata(metadata, targetFormat, strings.TrimSuffix(filepath.Base(inputAbs), filepath.Ext(inputAbs)), extent); err != nil {
		return result, err
	}
	if kind == "mbtiles" && targetFormat == PBF && !opts.allowMissingVectorMetadata {
		if err := archiveNormalizeMBVectorMetadata(metadata, extent.minZ, extent.maxZ); err != nil {
			return result, err
		}
	}
	if err := archiveValidateMetadataSize(metadata); err != nil {
		return result, err
	}
	for key, value := range metadata {
		if _, err := tx.ExecContext(ctx, "INSERT INTO metadata(name,value) VALUES(?,?)", key, value); err != nil {
			return result, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE input_seen"); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	if err := db.Close(); err != nil {
		return result, err
	}
	closed = true
	result = ArchiveConvertResult{Tiles: extent.count, Format: targetFormat}
	if kind == "file" {
		if existing && opts.Append {
			return result, archiveExportFiles(ctx, stage, destination, outputPattern, true)
		}
		stagedDir := filepath.Join(work, "tiles")
		if err := archiveExportFiles(ctx, stage, stagedDir, outputPattern, false); err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		return result, archivePublishDirectory(stagedDir, destination, existing)
	}
	staged := stage
	if kind == "pmtiles" {
		staged = filepath.Join(work, "archive.pmtiles")
		result, err = archiveWritePMTiles(ctx, stage, staged, work)
		if err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Overwrite || (opts.Append && existing) {
		if err := archiveCheckArchiveDestination(destination, kind); err != nil {
			return result, err
		}
		if existing {
			mode, err := replacementOutputMode(destination)
			if err != nil {
				return result, err
			}
			if err := os.Chmod(staged, mode); err != nil {
				return result, fmt.Errorf("preserve archive permissions: %w", err)
			}
		}
		err = os.Rename(staged, destination)
	} else {
		err = archivePublishNoReplace(staged, destination)
	}
	if err != nil {
		return result, fmt.Errorf("publish archive: %w", err)
	}
	return result, nil
}

// Resolve existing ancestors as well as the final path: an output reached
// through a parent directory symlink must not evade input/output overlap checks.
func archiveResolvedPath(path string) (string, error) {
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}

func archivePathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func archiveCheckArchiveDestination(destination, kind string) error {
	if kind == "mbtiles" {
		return checkSQLiteSidecars(destination)
	}
	file, err := os.Open(destination)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var header [16]byte
	n, readErr := io.ReadFull(file, header[:])
	closeErr := file.Close()
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if bytes.HasPrefix(header[:n], []byte("SQLite format 3\x00")) {
		return checkSQLiteSidecars(destination)
	}
	return nil
}

// Replacing a database while its live WAL or rollback journal remains beside
// the destination can make the new archive read stale pages from that sidecar.
func checkSQLiteSidecars(destination string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Lstat(destination + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > 0 && suffix == "-journal" {
			file, err := os.Open(destination + suffix)
			if err != nil {
				return err
			}
			var header [8]byte
			_, readErr := io.ReadFull(file, header[:])
			closeErr := file.Close()
			// PERSIST mode retains the committed journal with a zeroed header.
			if readErr == nil && closeErr == nil && header == [8]byte{} {
				continue
			}
		}
		if !info.Mode().IsRegular() || info.Size() > 0 {
			return fmt.Errorf("MBTiles has an active %s sidecar; close SQLite writers and checkpoint the database before writing or uploading", suffix)
		}
	}
	return nil
}

func archiveOutputKind(output, requested string) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(requested))
	if kind != "" {
		if kind == "file" || kind == "mbtiles" || kind == "pmtiles" {
			extension := strings.ToLower(filepath.Ext(output))
			if kind != "file" && (extension == ".mbtiles" || extension == ".pmtiles") && extension != "."+kind {
				return "", fmt.Errorf("output type %s conflicts with the %s filename extension", kind, extension)
			}
			return kind, nil
		}
		return "", fmt.Errorf("output type must be file, mbtiles or pmtiles")
	}
	if info, err := os.Stat(output); err == nil && info.IsDir() {
		return "file", nil
	}
	switch strings.ToLower(filepath.Ext(output)) {
	case ".mbtiles":
		return "mbtiles", nil
	case ".pmtiles":
		return "pmtiles", nil
	case "":
		return "file", nil
	default:
		return "", fmt.Errorf("output must end in .mbtiles or .pmtiles, or use -output-type file")
	}
}

func archiveCheckFilePattern(pattern string) error {
	return validateTilePathFileConflicts(pattern, "", "metadata.json", "failures.json")
}

func archiveWalkLocal(ctx context.Context, input, pattern string, visit archiveVisit) (map[string]string, error) {
	info, err := os.Lstat(input)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symbolic archive inputs are not supported")
	}
	if info.IsDir() {
		return archiveWalkDirectory(ctx, input, pattern, visit)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive input is not a regular file")
	}
	switch strings.ToLower(filepath.Ext(input)) {
	case ".mbtiles":
		return archiveWalkMB(ctx, input, visit)
	case ".pmtiles":
		return archiveWalkPM(ctx, input, visit)
	default:
		// Explicit output types permit arbitrary filenames. Recognize those
		// archives by their signatures so the output can be converted again.
		file, err := os.Open(input)
		if err != nil {
			return nil, err
		}
		var header [16]byte
		n, readErr := io.ReadFull(file, header[:])
		closeErr := file.Close()
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		switch {
		case bytes.HasPrefix(header[:n], []byte("SQLite format 3\x00")):
			return archiveWalkMB(ctx, input, visit)
		case bytes.HasPrefix(header[:n], []byte("PMTiles")):
			return archiveWalkPM(ctx, input, visit)
		default:
			return nil, fmt.Errorf("input must be a tile directory, MBTiles or PMTiles archive")
		}
	}
}

func archiveWalkDirectory(ctx context.Context, input, pattern string, visit archiveVisit) (map[string]string, error) {
	return archiveWalkDirectoryCanonical(ctx, input, pattern, false, visit)
}

func archiveWalkDirectoryCanonical(ctx context.Context, input, pattern string, requireCanonical bool, visit archiveVisit) (map[string]string, error) {
	metadata, err := archiveReadDirectoryMetadata(input)
	if err != nil {
		return nil, err
	}
	if pattern == "" {
		pattern = metadata["tile_path"]
	}
	pattern, err = normalizeTilePath(pattern)
	if err != nil {
		return nil, err
	}
	declared, err := archiveFormat(metadata["format"])
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(input, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not supported in tile input: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(input, path)
		if err != nil {
			return err
		}
		tile, format, matched, err := parseTilePath(pattern, filepath.ToSlash(relative))
		if err != nil {
			return fmt.Errorf("tile path %s: %w", relative, err)
		}
		if !matched {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("tile input is not a regular file: %s", path)
		}
		if format == "" {
			format = declared
		} else {
			format, err = archiveFormat(format)
			if err != nil {
				return err
			}
		}
		if requireCanonical {
			canonicalFormat := format
			if canonicalFormat == "" {
				canonicalFormat = PNG
			} // no {format} in this pattern
			canonical, err := renderTilePath(pattern, tile, canonicalFormat)
			if err != nil {
				return err
			}
			if filepath.ToSlash(relative) != canonical {
				return fmt.Errorf("existing tile path %s is not canonical (%s); convert to a new directory before appending", relative, canonical)
			}
		}
		size, err := entry.Info()
		if err != nil {
			return err
		}
		if size.Size() > maxTileDataBytes {
			return fmt.Errorf("tile %s exceeds 64 MiB input limit", relative)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return visit(int(tile.Z), tile.X, tile.Y, body, format)
	})
	delete(metadata, "tile_path")
	return metadata, err
}

func archiveWalkMB(ctx context.Context, input string, visit archiveVisit) (map[string]string, error) {
	db, err := archiveOpenSQLite(input, true)
	if err != nil {
		return nil, fmt.Errorf("open MBTiles input: %w", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	metadata, err := archiveReadMBMetadata(ctx, tx)
	if err != nil {
		return nil, err
	}
	format, err := archiveFormat(metadata["format"])
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT zoom_level,tile_column,tile_row,CASE WHEN length(CAST(tile_data AS BLOB))<=? THEN tile_data ELSE NULL END,length(CAST(tile_data AS BLOB)) FROM tiles", maxTileDataBytes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var z, x, y int64
		var body []byte
		var size int64
		if err := rows.Scan(&z, &x, &y, &body, &size); err != nil {
			return nil, err
		}
		if size > maxTileDataBytes {
			return nil, fmt.Errorf("MBTiles tile exceeds 64 MiB input limit")
		}
		if err := archiveCoordinate(z, x, y); err != nil {
			return nil, err
		}
		if err := visit(int(z), uint32(x), uint32((int64(1)<<uint(z))-1-y), body, format); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	delete(metadata, "tile_path")
	return metadata, nil
}

func archiveExportFiles(ctx context.Context, input, output, pattern string, appendExisting bool) error {
	// Serialize and validate metadata before writing any append tiles. JSON
	// escaping can make otherwise small SQL metadata exceed the reader's limit.
	db, err := archiveOpenSQLite(input, true)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return err
	}
	metadata, err := archiveReadMBMetadata(ctx, tx)
	tx.Rollback()
	db.Close()
	if err != nil {
		return err
	}
	metadata["tile_path"] = pattern
	metadata["scheme"] = "xyz"
	if strings.Contains(pattern, "{-y}") {
		metadata["scheme"] = "tms"
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxMetadataBytes {
		return fmt.Errorf("output metadata.json exceeds 16 MiB")
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	_, err = archiveWalkMB(ctx, input, func(z int, x, y uint32, body []byte, format string) error {
		relative, err := renderTilePath(pattern, maptile.Tile{Z: maptile.Zoom(z), X: x, Y: y}, format)
		if err != nil {
			return err
		}
		destination := filepath.Join(output, filepath.FromSlash(relative))
		if appendExisting {
			info, err := os.Lstat(destination)
			if err == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("output tile is not a regular file: %s", relative)
				}
				if info.Size() > 0 {
					return nil
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if format == PBF {
			body, err = prepareTileData(body, format, false)
			if err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			return err
		}
		return archiveWriteFileAtomic(destination, body)
	})
	if err != nil {
		return err
	}
	return archiveWriteFileAtomic(filepath.Join(output, "metadata.json"), append(data, '\n'))
}

func archiveWriteFileAtomic(destination string, body []byte) error {
	mode, err := replacementOutputMode(destination)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".tiler-tile-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return fmt.Errorf("preserve output permissions: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

func archivePublishDirectory(staged, destination string, replace bool) error {
	return archivePublishDirectoryWithRename(staged, destination, replace, os.Rename)
}

func archivePublishDirectoryWithRename(staged, destination string, replace bool, rename func(string, string) error) error {
	if !replace {
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("output appeared during conversion")
		} else if !os.IsNotExist(err) {
			return err
		}
		return rename(staged, destination)
	}
	// Keep the previous directory outside the conversion work directory. If
	// both publication and rollback fail it must survive deferred work cleanup.
	backupRoot, err := os.MkdirTemp(filepath.Dir(destination), ".tiler-previous-*")
	if err != nil {
		return err
	}
	backup := filepath.Join(backupRoot, "previous-output")
	if err := rename(destination, backup); err != nil {
		os.RemoveAll(backupRoot)
		return fmt.Errorf("move previous output aside: %w", err)
	}
	if err := rename(staged, destination); err != nil {
		if restoreErr := rename(backup, destination); restoreErr != nil {
			return fmt.Errorf("publish directory: %v; restore previous output: %v; original data retained at %s", err, restoreErr, backup)
		}
		os.RemoveAll(backupRoot)
		return err
	}
	if err := os.RemoveAll(backupRoot); err != nil {
		return fmt.Errorf("output published, but previous directory cleanup failed at %s: %w", backup, err)
	}
	return nil
}

func archiveUnionMetadataBounds(metadata map[string]string, extent archiveExtent) {
	bounds := []float64{extent.west, extent.south, extent.east, extent.north}
	if old, err := archiveNumbers(metadata["bounds"], 4); err == nil {
		if old[0] < bounds[0] {
			bounds[0] = old[0]
		}
		if old[1] < bounds[1] {
			bounds[1] = old[1]
		}
		if old[2] > bounds[2] {
			bounds[2] = old[2]
		}
		if old[3] > bounds[3] {
			bounds[3] = old[3]
		}
	}
	metadata["bounds"] = archiveBoundsText(bounds)
}

func archiveMergeMetadata(existing, incoming map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(existing)+len(incoming))
	for key, value := range existing {
		result[key] = value
	}
	for key, value := range incoming {
		if result[key] == "" {
			result[key] = value
		}
	}
	if existing["json"] != "" || incoming["json"] != "" {
		merged, err := mergeArchiveJSON(existing["json"], incoming["json"])
		if err != nil {
			return nil, err
		}
		result["json"] = merged
	}
	if a, err := archiveNumbers(existing["bounds"], 4); err == nil {
		if b, e := archiveNumbers(incoming["bounds"], 4); e == nil {
			if b[0] < a[0] {
				a[0] = b[0]
			}
			if b[1] < a[1] {
				a[1] = b[1]
			}
			if b[2] > a[2] {
				a[2] = b[2]
			}
			if b[3] > a[3] {
				a[3] = b[3]
			}
			result["bounds"] = archiveBoundsText(a)
		}
	}
	// Merge vector layer descriptions from both regions instead of losing layers
	// that happen to be absent from the first region's metadata.
	layers := map[string]map[string]interface{}{}
	var order []string
	for index, metadata := range []map[string]string{existing, incoming} {
		object := map[string]interface{}{}
		if metadata["json"] != "" {
			if err := json.Unmarshal([]byte(metadata["json"]), &object); err != nil {
				return nil, fmt.Errorf("invalid JSON metadata: %w", err)
			}
			if object == nil {
				return nil, fmt.Errorf("metadata json must be an object")
			}
		}
		if index == 0 {
			// Keep old JSON extensions ahead of newly supplied flat values.
			for key := range object {
				if !archiveFlatMetadataKey(key) && existing[key] == "" {
					delete(result, key)
				}
			}
		}
		if text := metadata["vector_layers"]; text != "" {
			var value interface{}
			if err := json.Unmarshal([]byte(text), &value); err != nil {
				return nil, err
			}
			object["vector_layers"] = value
		}
		if value, ok := object["vector_layers"]; ok {
			if err := archiveValidateVectorLayers(value); err != nil {
				return nil, err
			}
			for _, raw := range value.([]interface{}) {
				layer := raw.(map[string]interface{})
				id := layer["id"].(string)
				if previous, ok := layers[id]; ok {
					if err := mergeVectorLayer(previous, layer); err != nil {
						return nil, err
					}
				} else {
					layers[id] = layer
					order = append(order, id)
				}
			}
		}
	}
	if len(order) > 0 {
		merged := make([]interface{}, 0, len(order))
		for _, id := range order {
			merged = append(merged, layers[id])
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return nil, err
		}
		result["vector_layers"] = string(data)
	}
	delete(result, "tile_path")
	return result, nil
}

func archiveApplyMetadata(metadata, overrides map[string]string) error {
	for key, value := range overrides {
		if key == "json" && strings.TrimSpace(value) != "" {
			merged, err := mergeArchiveJSON(value, metadata[key])
			if err != nil {
				return err
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(value), &fields); err != nil {
				return err
			}
			for name := range fields {
				if !archiveFlatMetadataKey(name) || name == "vector_layers" {
					if _, explicitFlat := overrides[name]; !explicitFlat {
						delete(metadata, name)
					}
				}
			}
			value = merged
		}
		metadata[key] = value
	}
	return nil
}

// The first object wins conflicts; the second only supplies missing keys.
func mergeArchiveJSON(preferred, fallback string) (string, error) {
	object := make(map[string]json.RawMessage)
	for _, text := range []string{fallback, preferred} {
		if strings.TrimSpace(text) == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &fields); err != nil || fields == nil {
			return "", fmt.Errorf("metadata json must be a JSON object")
		}
		for key, value := range fields {
			object[key] = value
		}
	}
	encoded, err := json.Marshal(object)
	return string(encoded), err
}

// Existing standard/project fields use string storage or dataset-derived values.
func archiveFlatMetadataKey(key string) bool {
	switch key {
	case "name", "description", "attribution", "version", "type", "format", "bounds", "center", "minzoom", "maxzoom", "scheme", "tile_path", "vector_layers", "json", "id", "basename", "pixel_scale":
		return true
	}
	return false
}
