package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

// saveFileMetadata replaces the file output's metadata only after the entire
// JSON document has been written and closed successfully.
func (task *Task) saveFileMetadata() error {
	if task.outformat != "file" {
		return nil
	}
	if task.File == "" {
		return fmt.Errorf("file output directory is required for metadata")
	}
	if err := task.validateFileOutputSafety(); err != nil {
		return err
	}
	metadata, err := task.fileMetadataForDownload()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode file metadata: %w", err)
	}
	if len(data)+1 > maxMetadataBytes {
		return fmt.Errorf("output metadata.json exceeds 16 MiB")
	}
	mode, err := replacementOutputMode(filepath.Join(task.File, "metadata.json"))
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(task.File, ".metadata-*")
	if err != nil {
		return fmt.Errorf("create file metadata temporary file: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write file metadata: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return fmt.Errorf("preserve metadata permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close file metadata: %w", err)
	}
	if err := task.validateFileOutputSafety(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(task.File, "metadata.json")); err != nil {
		return fmt.Errorf("replace file metadata: %w", err)
	}
	return nil
}

func (task *Task) outputPath() (string, error) {
	name := viper.GetString("output.name")
	if name != "" {
		if name != safeOutputName(name) || name == "." || name == ".." {
			return "", fmt.Errorf("output.name must be a valid Windows filename without directories")
		}
	} else {
		name = fmt.Sprintf("%s-z%d-%d", safeOutputName(task.Name), task.Min, task.Max)
		if !task.resume {
			name += "." + task.ID
		}
	}
	if task.outformat == "mbtiles" {
		name += ".mbtiles"
	}
	if err := validateRelativeTilePath(name); err != nil {
		return "", fmt.Errorf("invalid output filename: %w", err)
	}
	return filepath.Join(viper.GetString("output.directory"), name), nil
}

func safeOutputName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, " .")
	if name == "" {
		return "tiles"
	}
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		isWindowsNumberedDevice(base) {
		return "_" + name
	}
	return name
}

func isWindowsNumberedDevice(base string) bool {
	for _, prefix := range []string{"COM", "LPT"} {
		if strings.HasPrefix(base, prefix) {
			suffix := strings.TrimPrefix(base, prefix)
			if (len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9') || suffix == "¹" || suffix == "²" || suffix == "³" {
				return true
			}
		}
	}
	return false
}

func (task *Task) tileExists(ctx context.Context, tile maptile.Tile) (bool, error) {
	switch task.outformat {
	case "s3":
		return task.s3.exists(ctx, tile, task.TileMap.Format)
	case "mbtiles":
		var found int
		err := task.db.QueryRowContext(ctx, "select 1 from tiles where zoom_level = ? and tile_column = ? and tile_row = ? and length(tile_data) > 0 limit 1", tile.Z, tile.X, (Tile{T: tile}).flipY()).Scan(&found)
		if err == sql.ErrNoRows {
			return false, nil
		}
		return err == nil, err
	default:
		filename, err := task.fileTilePath(tile)
		if err != nil {
			return false, err
		}
		info, err := os.Stat(filename)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return info.Mode().IsRegular() && info.Size() > 0, nil
	}
}

func (task *Task) fileTilePath(tile maptile.Tile) (string, error) {
	relative, err := renderTilePath(task.tilePath, tile, task.TileMap.Format)
	if err != nil {
		return "", err
	}
	filename := filepath.Join(task.File, filepath.FromSlash(relative))
	if err := validateOutputFilePath(task.File, filename); err != nil {
		return "", err
	}
	return filename, nil
}

// Ancestors above root may refer to mounted or explicitly linked volumes, but
// root and its descendants must not redirect writes or resume checks through
// links. Lstat also detects Windows junctions in Go.
func validateOutputFilePath(root, filename string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	filename, err = filepath.Abs(filename)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output file must remain inside its dataset directory")
	}
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	current := root
	for index := 0; index <= len(parts); index++ {
		if index > 0 {
			current = filepath.Join(current, parts[index-1])
		}
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect output path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output dataset paths cannot contain symbolic links or junctions: %s", current)
		}
		if index == 0 || index < len(parts) {
			if !info.IsDir() {
				return fmt.Errorf("output parent is not a directory: %s", current)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("output tile or metadata must be a regular file: %s", current)
		}
	}
	return nil
}

func (task *Task) validateFileOutputSafety() error {
	root := viper.GetString("output.directory")
	if root == "" {
		root = task.File
	}
	return validateOutputFilePath(root, filepath.Join(task.File, "metadata.json"))
}

func replacementOutputMode(filename string) (os.FileMode, error) {
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return 0600, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("output replacement requires a regular file: %s", filename)
	}
	return info.Mode().Perm(), nil
}

// validateOutputPathSafety checks a complete path when no narrower configured
// dataset boundary is available. Missing suffix components are allowed, but an
// existing ancestor must be a real directory, never a link or special file.
func validateOutputPathSafety(filename string) error {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	for current := absolute; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect output path: %w", err)
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("output paths cannot contain symbolic links or junctions: %s", current)
			}
			if current != absolute && !info.IsDir() {
				return fmt.Errorf("output parent is not a directory: %s", current)
			}
			if current == absolute && !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("output must be a regular file or directory: %s", current)
			}
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}
