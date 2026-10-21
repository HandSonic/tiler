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
	metadata, err := task.fileMetadataForDownload()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode file metadata: %w", err)
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
	if err := file.Close(); err != nil {
		return fmt.Errorf("close file metadata: %w", err)
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
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return "_" + name
	}
	return name
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
	return filepath.Join(task.File, filepath.FromSlash(relative)), nil
}
