package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulmach/orb/maptile"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

func NewRepairTask(layers []Layer, tm TileMap, manifest *FailureManifest, manifestPath string) (*Task, error) {
	return NewRepairTaskContext(context.Background(), layers, tm, manifest, manifestPath)
}

func NewRepairTaskContext(ctx context.Context, layers []Layer, tm TileMap, manifest *FailureManifest, manifestPath string) (*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if manifest == nil {
		return nil, fmt.Errorf("repair requires a failure manifest")
	}
	data := manifest.Snapshot()
	target, ok := manifest.Target()
	if !ok {
		return nil, fmt.Errorf("repair manifest does not identify the original output target")
	}
	sameFormat := data.Format == strings.ToLower(strings.TrimSpace(tm.Format))
	if target.Kind == "mbtiles" || target.Kind == "compactv2" {
		sameFormat = sameTileEncoding(data.Format, strings.TrimSpace(tm.Format))
	}
	if data.MapName != tm.Name || !sameFormat {
		return nil, fmt.Errorf("repair map name and tile format must match the manifest")
	}
	if target.Kind != strings.ToLower(viper.GetString("output.format")) {
		return nil, fmt.Errorf("repair output.format must match the manifest target %q", target.Kind)
	}
	if target.Kind == "file" || target.Kind == "s3" {
		current, err := normalizeTilePath(viper.GetString("output.tile_path"))
		if err != nil {
			return nil, err
		}
		original, err := normalizeTilePath(target.TilePath)
		if err != nil {
			return nil, err
		}
		if current != original {
			return nil, fmt.Errorf("repair output.tile_path must match the original manifest target")
		}
	}
	var s3Destination *s3Output
	switch target.Kind {
	case "file", "mbtiles", "compactv2":
		root, err := filepath.Abs(viper.GetString("output.directory"))
		if err != nil {
			return nil, err
		}
		destination, err := filepath.Abs(target.Path)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(root, destination)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("repair target must be inside the configured output.directory; keep the original destination configuration")
		}
		target.Path = destination
		if err := validateExistingRepairTarget(target.Kind, destination); err != nil {
			return nil, err
		}
	case "s3":
		if target.Bucket != strings.TrimSpace(viper.GetString("output.s3.bucket")) || target.Prefix != strings.Trim(viper.GetString("output.s3.prefix"), "/") {
			return nil, fmt.Errorf("repair S3 bucket and prefix must match the manifest target")
		}
		var err error
		s3Destination, err = newS3Output(ctx)
		if err != nil {
			return nil, err
		}
		if s3Destination.endpointFingerprint != target.EndpointFingerprint {
			return nil, fmt.Errorf("repair S3 endpoint and region must match the manifest target")
		}
	}
	task := newDownloadTaskContext(ctx, layers, tm, manifest)
	if task == nil {
		return nil, fmt.Errorf("repair requires source layers")
	}
	if task.initErr != nil {
		task.Cancel()
		return nil, task.initErr
	}
	task.resume = true
	task.File = target.Path
	task.s3 = s3Destination
	path, err := filepath.Abs(manifestPath)
	if err != nil {
		task.Cancel()
		return nil, err
	}
	task.failurePath = path
	if err := task.setupFailureManifest(); err != nil {
		task.Cancel()
		return nil, err
	}
	return task, nil
}

func validateExistingRepairTarget(kind, path string) error {
	checkedPath := path
	if kind == "file" || kind == "compactv2" {
		checkedPath = filepath.Join(path, "metadata.json")
	}
	if err := validateOutputFilePath(viper.GetString("output.directory"), checkedPath); err != nil {
		return fmt.Errorf("unsafe original repair target: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("repair target no longer exists; restore the original output before repairing")
		}
		return fmt.Errorf("inspect original repair target: %w", err)
	}
	if (kind == "file" || kind == "compactv2") && !info.IsDir() {
		return fmt.Errorf("repair %s target must be the original output directory", kind)
	}
	if kind == "mbtiles" && !info.Mode().IsRegular() {
		return fmt.Errorf("repair mbtiles target must be the original regular database file")
	}
	if kind == "compactv2" {
		ok, err := compactIsDirectory(path)
		if err != nil {
			return fmt.Errorf("inspect original Compact v2 target: %w", err)
		}
		if !ok {
			return fmt.Errorf("repair Compact v2 target must be the original cache directory")
		}
	}
	return nil
}

func (task *Task) recordTileFailure(tile maptile.Tile, source string, err error) {
	task.tileFailure(err)
	if task.manifest != nil {
		if recordErr := task.manifest.Record(tile, source, err); recordErr != nil {
			task.recordError(fmt.Errorf("record failed tile: %w", recordErr))
		}
	}
}

func (task *Task) resolveTileFailure(tile maptile.Tile, source string) {
	// Sources were validated during task initialization. Successful downloads
	// need no source fingerprint work when there are no failures to resolve.
	if task.manifest != nil && task.manifest.Len() != 0 {
		if err := task.manifest.Resolve(tile, source); err != nil {
			task.recordError(fmt.Errorf("resolve failed tile: %w", err))
		}
	}
}

func (task *Task) setupFailureManifest() error {
	target := FailureTarget{Kind: task.outformat}
	if task.outformat == "file" || task.outformat == "s3" {
		target.TilePath = task.tilePath
	}
	if task.outformat == "s3" {
		target.Bucket, target.Prefix = task.s3.bucket, task.s3.prefix
		target.EndpointFingerprint = task.s3.endpointFingerprint
	} else {
		path, err := filepath.Abs(task.File)
		if err != nil {
			return err
		}
		target.Path = path
	}
	if task.failurePath == "" {
		task.failurePath = viper.GetString("output.failure_file")
		if task.failurePath == "" {
			switch task.outformat {
			case "file", "compactv2":
				task.failurePath = filepath.Join(task.File, "failures.json")
			case "mbtiles":
				task.failurePath = task.File + ".failures.json"
			default:
				task.failurePath = filepath.Join(viper.GetString("output.directory"), fmt.Sprintf("%s-z%d-%d.%s.failures.json", safeOutputName(task.Name), task.Min, task.Max, task.ID))
			}
		}
	}
	path, err := filepath.Abs(task.failurePath)
	if err != nil {
		return err
	}
	if strings.ToLower(filepath.Ext(path)) != ".json" {
		return fmt.Errorf("output.failure_file must be a .json path")
	}
	if err := validateFailureManifestSafety(path); err != nil {
		return fmt.Errorf("unsafe failure manifest path: %w", err)
	}
	if task.outformat != "s3" {
		if archivePathContains(strings.ToLower(path), strings.ToLower(target.Path)) {
			return fmt.Errorf("failure manifest cannot be the output path or one of its parent directories")
		}
		files := []string{target.Path}
		if task.outformat == "mbtiles" {
			files = append(files, target.Path+"-wal", target.Path+"-journal")
		}
		if task.outformat == "file" || task.outformat == "compactv2" {
			files = []string{filepath.Join(target.Path, "metadata.json")}
			if task.outformat == "compactv2" {
				files = append(files, filepath.Join(target.Path, "conf.xml"), filepath.Join(target.Path, "conf.cdi"))
			}
		}
		// These files may not exist until output initialization. Check path
		// components now, including Windows casing, before creating anything.
		for _, file := range files {
			if archivePathContains(strings.ToLower(file), strings.ToLower(path)) {
				return fmt.Errorf("failure manifest cannot replace or be inside output file %s", filepath.Base(file))
			}
		}
	}
	if task.outformat == "file" {
		reserved := []string{"metadata.json"}
		root, err := filepath.Abs(task.File)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			reserved = append(reserved, filepath.ToSlash(relative))
		}
		if err := validateTilePathFileConflicts(task.tilePath, task.TileMap.Format, reserved...); err != nil {
			return err
		}
	} else if task.outformat == "compactv2" {
		relative, err := filepath.Rel(target.Path, path)
		if err == nil { // A report on another Windows volume is outside the cache.
			parts := strings.Split(relative, string(filepath.Separator))
			if len(parts) > 1 && strings.EqualFold(parts[0], "_alllayers") {
				for _, parent := range parts[1 : len(parts)-1] {
					if strings.EqualFold(filepath.Ext(parent), ".bundle") {
						return fmt.Errorf("failure manifest cannot be inside a Compact bundle path")
					}
				}
			}
		}
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("failure manifest path must be a regular .json file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect failure manifest path: %w", err)
	}
	// Check the nearest existing parent without creating anything. A bad report
	// configuration must fail before replacing metadata or recreating a database.
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("failure manifest parent must be a directory")
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect failure manifest parent: %w", err)
		}
		if filepath.Dir(parent) == parent {
			return fmt.Errorf("failure manifest has no existing parent directory")
		}
	}
	if err := task.manifest.SetTarget(target); err != nil {
		return err
	}
	if !task.repairMode {
		if _, err := os.Stat(path); err == nil {
			previous, err := LoadFailureManifest(path)
			if err != nil {
				return fmt.Errorf("read existing failure manifest before continuing: %w", err)
			}
			data := previous.Snapshot()
			sameFormat := data.Format == strings.ToLower(strings.TrimSpace(task.TileMap.Format))
			if task.outformat == "mbtiles" || task.outformat == "compactv2" {
				sameFormat = sameTileEncoding(data.Format, strings.TrimSpace(task.TileMap.Format))
			}
			if data.MapName != task.Name || !sameFormat {
				return fmt.Errorf("existing failure manifest belongs to a different map or format; choose a separate output.failure_file")
			}
			oldTarget, oldOK := previous.Target()
			currentTarget, _ := task.manifest.Target()
			if !oldOK || !sameFailureTarget(oldTarget, currentTarget) {
				return fmt.Errorf("existing failure manifest belongs to a different output target; keep the original destination or choose a separate output.failure_file")
			}
			if task.resume {
				// Retain failures from previous regions, even when the current job
				// visits only a newly appended region. Successful saves resolve them.
				task.manifest = previous
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	task.failurePath = path
	return nil
}

func validateFailureManifestSafety(path string) error {
	root, err := filepath.Abs(viper.GetString("output.directory"))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, path)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return validateOutputFilePath(root, path)
	}
	return validateOutputPathSafety(path)
}

func sameFailureTarget(a, b FailureTarget) bool {
	if a.Kind != b.Kind || a.Bucket != b.Bucket || a.Prefix != b.Prefix || a.EndpointFingerprint != b.EndpointFingerprint {
		return false
	}
	if a.Path != b.Path {
		if a.Kind != "file" && a.Kind != "mbtiles" && a.Kind != "compactv2" {
			return false
		}
		// Existing local targets can have multiple names, including different
		// casing on Windows. Confirm identity rather than guessing from text.
		original, originalErr := os.Stat(a.Path)
		current, currentErr := os.Stat(b.Path)
		if originalErr != nil || currentErr != nil || !os.SameFile(original, current) {
			return false
		}
		// SQLite sidecars use the filename, not the database's file ID. An
		// alias could otherwise open the main file without its committed WAL.
		if a.Kind == "mbtiles" && (checkSQLiteSidecars(a.Path) != nil || checkSQLiteSidecars(b.Path) != nil) {
			return false
		}
	}
	if a.Kind == "file" || a.Kind == "s3" {
		ap, aerr := normalizeTilePath(a.TilePath)
		bp, berr := normalizeTilePath(b.TilePath)
		return aerr == nil && berr == nil && ap == bp
	}
	return true
}

func (task *Task) persistFailureManifest() error {
	if task.manifest == nil || task.failurePath == "" {
		return nil
	}
	if task.manifest.Len() == 0 && !task.repairMode {
		if _, err := os.Stat(task.failurePath); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
	}
	if err := task.manifest.Save(task.failurePath); err != nil {
		return err
	}
	log.Infof("Failure manifest: %s (%d unresolved tiles)", task.failurePath, task.manifest.Len())
	return nil
}
