package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// archivePrepareCompactAppend validates the incoming dataset independently,
// then builds replacements for just its affected bundles. The loader supplies
// an old bundle, if present; unrelated bundles are never read or enumerated.
// Only the current bundle is tracked in memory. Tiles counts new coordinates.
func archivePrepareCompactAppend(ctx context.Context, input string, previous map[string]string, work string, opts ArchiveConvertOptions, loadBundle func(context.Context, string, string) error) (string, ArchiveConvertResult, error) {
	result := ArchiveConvertResult{Incremental: true}
	format, err := archiveFormat(previous["format"])
	if err != nil || (format != PNG && format != JPG) {
		return "", result, fmt.Errorf("cannot append to a mixed-format or unsupported Compact cache; convert to a new target")
	}
	requested, err := archiveFormat(opts.Format)
	if err != nil {
		return "", result, err
	}
	if requested != "" && requested != format {
		return "", result, fmt.Errorf("Compact v2 append cannot change the existing tile format")
	}
	local := opts
	local.Append, local.Overwrite = false, false
	local.OutputKind, local.OutputTilePath = "compactv2", ""
	incomingRoot := filepath.Join(work, "incoming")
	if _, err := convertLocalArchive(ctx, input, incomingRoot, local); err != nil {
		return "", result, err
	}
	incoming, err := compactReadMetadata(incomingRoot)
	if err != nil {
		return "", result, err
	}
	if !sameTileEncoding(format, incoming["format"]) {
		return "", result, fmt.Errorf("Compact v2 append cannot change the existing tile format; set -format to convert incoming tiles")
	}
	if previous["pixel_scale"] != "" && previous["pixel_scale"] != incoming["pixel_scale"] {
		return "", result, fmt.Errorf("compact tile dimensions differ from the existing cache")
	}

	// Explicit descriptions still override old values, but cannot shrink the
	// existing coverage or change its grid. The merge also updates TileJSON.
	old := make(map[string]string, len(previous))
	for key, value := range previous {
		old[key] = value
	}
	if err := archiveApplyMetadata(old, opts.Metadata); err != nil {
		return "", result, err
	}
	for _, key := range []string{"format", "pixel_scale", "bounds", "minzoom", "maxzoom"} {
		old[key] = previous[key]
	}
	directory := filepath.Join(work, "delta")
	if err := os.Mkdir(directory, 0755); err != nil {
		return "", result, err
	}
	tileSize, _ := strconv.Atoi(incoming["pixel_scale"]) // Validated by compactReadMetadata.
	cache := &compactCache{root: directory, format: format, tileSize: tileSize, resume: true}
	extent := archiveExtent{}
	var current string
	changed := false
	finishBundle := func() error {
		if current != "" && !changed {
			// A bundle containing only overlaps does not need publication.
			if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	_, err = archiveWalkCompact(ctx, incomingRoot, func(z int, x, y uint32, body []byte, _ string) error {
		extent.add(z, x, y)
		path := compactPath(directory, z, x, y)
		if path != current {
			if err := finishBundle(); err != nil {
				return err
			}
			current, changed = path, false
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			relative, err := filepath.Rel(directory, path)
			if err != nil {
				return err
			}
			if err := loadBundle(ctx, filepath.ToSlash(relative), path); err != nil {
				return err
			}
		}
		present, err := cache.Exists(ctx, z, x, y)
		if err != nil || present {
			return err
		}
		if err := cache.Save(ctx, z, x, y, body); err != nil {
			return err
		}
		changed = true
		result.Tiles++
		return nil
	})
	if err != nil {
		return "", result, err
	}
	if err := finishBundle(); err != nil {
		return "", result, err
	}
	// Input declarations may understate their tiles. Append coverage must
	// contain the actual incoming coordinates as well as the previous bounds.
	archiveUnionMetadataBounds(incoming, extent)
	cache.metadata, err = mergeDownloadMetadata(old, incoming, false)
	if err != nil {
		return "", result, err
	}
	if err := cache.Close(); err != nil {
		return "", result, err
	}
	// Validate retained old entries in the bundles about to be published.
	// Unrelated bundles and overlap-only copies remain outside this walk.
	if _, err := archiveWalkCompact(ctx, directory, func(int, uint32, uint32, []byte, string) error { return nil }); err != nil {
		return "", result, err
	}
	result.Format = format
	return directory, result, nil
}

func archiveAppendCompactLocal(ctx context.Context, input, destination, work string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var result ArchiveConvertResult
	previous, err := compactReadMetadata(destination)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(filepath.Join(destination, "_alllayers")); os.IsNotExist(err) {
		// Initialization writes a default config before the first download.
		// With no bundles, the first incoming tile can still establish its size.
		previous["pixel_scale"] = ""
	} else if err != nil {
		return result, err
	}
	if err := archiveValidateCompactFailures(destination); err != nil {
		return result, err
	}
	directory, result, err := archivePrepareCompactAppend(ctx, input, previous, work, opts, func(ctx context.Context, relative, staged string) error {
		path := filepath.Join(destination, filepath.FromSlash(relative))
		if err := validateOutputFilePath(destination, path); err != nil {
			return err
		}
		source, err := os.Open(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil {
			return err
		}
		copy, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		err = archiveCopy(ctx, copy, source)
		if err == nil {
			err = copy.Chmod(info.Mode().Perm())
		}
		if closeErr := copy.Close(); err == nil {
			err = closeErr
		}
		return err
	})
	if err != nil {
		return result, err
	}
	return result, archivePublishCompactAppend(ctx, directory, destination)
}

// Metadata expands first so even an interrupted publication can read bundles
// at new zooms. Each bundle replaces one file; the whole append is not atomic.
func archivePublishCompactAppend(ctx context.Context, staged, destination string) error {
	metadataNames := []string{"metadata.json", "conf.cdi", "conf.xml"}
	for _, name := range metadataNames {
		if err := validateOutputFilePath(destination, filepath.Join(destination, name)); err != nil {
			return err
		}
	}
	for _, name := range metadataNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(destination, name)
		if err := validateOutputFilePath(destination, path); err != nil {
			return err
		}
		body, err := os.ReadFile(filepath.Join(staged, name))
		if err != nil {
			return err
		}
		if err := archiveWriteFileAtomic(path, body); err != nil {
			return err
		}
	}
	return filepath.WalkDir(filepath.Join(staged, "_alllayers"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(staged, path)
		if err != nil {
			return err
		}
		output := filepath.Join(destination, relative)
		if err := validateOutputFilePath(destination, output); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		return os.Rename(path, output)
	})
}
