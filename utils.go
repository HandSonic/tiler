package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	log "github.com/sirupsen/logrus"
	"tiler/internal/cover"
)

func saveToMBTile(tile Tile, db *sql.DB) error {
	_, err := db.Exec("insert or replace into tiles (zoom_level, tile_column, tile_row, tile_data) values (?, ?, ?, ?);", tile.T.Z, tile.T.X, tile.flipY(), tile.C)
	// _, err := db.Exec("insert or ignore into tiles (zoom_level, tile_column, tile_row, tile_data) values (?, ?, ?, ?);", tile.T.Z, tile.T.X, tile.flipY(), tile.C)
	if err != nil {
		return err
	}
	return nil
}

func saveToFiles(tile Tile, task *Task) error {
	fileName, err := task.fileTilePath(tile.T)
	if err != nil {
		return err
	}
	dir := filepath.Dir(fileName)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return err
	}
	if err := validateOutputFilePath(task.File, fileName); err != nil {
		return err
	}
	mode, err := replacementOutputMode(fileName)
	if err != nil {
		return err
	}
	// A canceled or interrupted write must not become a resumable partial tile.
	file, err := os.CreateTemp(dir, ".tile-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(tile.C); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := validateOutputFilePath(task.File, fileName); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), fileName); err != nil {
		return err
	}
	log.Println(fileName)
	return nil
}

func optimizeConnection(db *sql.DB) error {
	// _, err := db.Exec("PRAGMA synchronous=0")
	// if err != nil {
	// 	return err
	// }
	_, err := db.Exec("PRAGMA locking_mode=EXCLUSIVE")
	if err != nil {
		return err
	}
	_, err = db.Exec("PRAGMA journal_mode=DELETE")
	if err != nil {
		return err
	}
	return nil
}

func optimizeDatabase(db *sql.DB) error {
	_, err := db.Exec("ANALYZE;")
	if err != nil {
		return err
	}

	_, err = db.Exec("VACUUM;")
	if err != nil {
		return err
	}

	return nil
}

func loadFeature(path string) *geojson.Feature {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("unable to read file: %v", err)
	}

	f, err := geojson.UnmarshalFeature(data)
	if err == nil {
		return f
	}

	fc, err := geojson.UnmarshalFeatureCollection(data)
	if err == nil {
		if len(fc.Features) != 1 {
			log.Fatalf("must have 1 feature: %v", len(fc.Features))
		}
		return fc.Features[0]
	}

	g, err := geojson.UnmarshalGeometry(data)
	if err != nil {
		log.Fatalf("unable to unmarshal feature: %v", err)
	}

	return geojson.NewFeature(g.Geometry())
}

func loadFeatureCollection(path string) *geojson.FeatureCollection {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("unable to read file: %v", err)
	}

	fc, err := geojson.UnmarshalFeatureCollection(data)
	if err != nil {
		log.Fatalf("unable to unmarshal feature: %v", err)
	}

	count := 0
	for i := range fc.Features {
		if fc.Features[i].Properties["name"] != "original" {
			fc.Features[count] = fc.Features[i]
			count++
		}
	}
	fc.Features = fc.Features[:count]

	return fc
}

func loadCollection(path string) orb.Collection {
	collection, err := readCollection(path)
	if err != nil {
		log.Fatal(err)
	}
	return collection
}

// readCollection accepts common GeoJSON exports without logging or exiting, so
// callers can report input errors and the parsing behavior can be tested.
func readCollection(path string) (orb.Collection, error) {
	const maxGeoJSONBytes = 64 << 20
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read GeoJSON %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("GeoJSON %s must be a regular file", path)
	}
	if info.Size() > maxGeoJSONBytes {
		return nil, fmt.Errorf("GeoJSON %s exceeds the 64 MiB input limit", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read GeoJSON %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGeoJSONBytes+1))
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("read GeoJSON %s: %w", path, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close GeoJSON %s: %w", path, closeErr)
	}
	if len(data) > maxGeoJSONBytes {
		return nil, fmt.Errorf("GeoJSON %s exceeds the 64 MiB input limit", path)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("GeoJSON %s is empty", path)
	}
	// Validate coordinate arrays before using orb's decoder. This version of
	// orb can otherwise turn some malformed coordinates into zero values.
	var checkCoordinates func(json.RawMessage, int) error
	checkCoordinates = func(raw json.RawMessage, depth int) error {
		if depth == 0 {
			var point []float64
			if err := json.Unmarshal(raw, &point); err != nil {
				return err
			}
			if len(point) < 2 {
				return fmt.Errorf("a position must contain longitude and latitude")
			}
			var values []json.RawMessage
			if err := json.Unmarshal(raw, &values); err != nil {
				return err
			}
			for _, value := range values {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return fmt.Errorf("a position must contain numbers, not null")
				}
			}
			if point[0] < -180 || point[0] > 180 || point[1] < -90 || point[1] > 90 {
				return fmt.Errorf("coordinates must use longitude [-180,180] and latitude [-90,90], got [%g,%g]", point[0], point[1])
			}
			return nil
		}
		var children []json.RawMessage
		if err := json.Unmarshal(raw, &children); err != nil {
			return err
		}
		if len(children) == 0 {
			return fmt.Errorf("coordinates must not be empty")
		}
		for _, child := range children {
			if err := checkCoordinates(child, depth-1); err != nil {
				return err
			}
		}
		return nil
	}
	var geometryBytes uint64
	var decodeGeometry func(json.RawMessage, int) (orb.Geometry, error)
	decodeGeometry = func(raw json.RawMessage, nesting int) (orb.Geometry, error) {
		if nesting > 32 {
			return nil, fmt.Errorf("GeoJSON GeometryCollection nesting exceeds 32 levels")
		}
		geometryBytes += uint64(len(raw))
		if geometryBytes > 128<<20 {
			return nil, fmt.Errorf("GeoJSON geometry decoding exceeds its 128 MiB work budget")
		}
		var geometry struct {
			Type        string            `json:"type"`
			Coordinates json.RawMessage   `json:"coordinates"`
			Geometries  []json.RawMessage `json:"geometries"`
		}
		if err := json.Unmarshal(raw, &geometry); err != nil {
			return nil, err
		}
		depth := 0
		switch geometry.Type {
		case "Point":
		case "MultiPoint", "LineString":
			depth = 1
		case "MultiLineString", "Polygon":
			depth = 2
		case "MultiPolygon":
			depth = 3
		case "GeometryCollection":
			if len(geometry.Geometries) == 0 {
				return nil, fmt.Errorf("GeometryCollection must not be empty")
			}
			collection := make(orb.Collection, 0, len(geometry.Geometries))
			for i, raw := range geometry.Geometries {
				g, err := decodeGeometry(raw, nesting+1)
				if err != nil {
					return nil, fmt.Errorf("geometry %d: %w", i+1, err)
				}
				collection = append(collection, g)
			}
			return collection, nil
		default:
			return nil, fmt.Errorf("unsupported or missing GeoJSON geometry type %q", geometry.Type)
		}
		if err := checkCoordinates(geometry.Coordinates, depth); err != nil {
			return nil, fmt.Errorf("invalid %s coordinates: %w", geometry.Type, err)
		}
		g, err := geojson.UnmarshalGeometry(raw)
		if err != nil {
			return nil, err
		}
		decoded := g.Geometry()
		if err := validateGeoJSONLinearGeometry(decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}
	decodeFeature := func(raw json.RawMessage) (orb.Geometry, error) {
		var feature struct {
			Type     string          `json:"type"`
			Geometry json.RawMessage `json:"geometry"`
		}
		if err := json.Unmarshal(raw, &feature); err != nil {
			return nil, err
		}
		if feature.Type != "Feature" {
			return nil, fmt.Errorf("expected Feature, got %q", feature.Type)
		}
		return decodeGeometry(feature.Geometry, 0)
	}

	if data[0] == '[' {
		var results []struct {
			GeoJSON json.RawMessage `json:"geojson"`
		}
		if err := json.Unmarshal(data, &results); err != nil {
			return nil, fmt.Errorf("parse Nominatim results in %s: %w", path, err)
		}
		if len(results) == 0 {
			return nil, fmt.Errorf("Nominatim results in %s are empty", path)
		}
		collection := make(orb.Collection, 0, len(results))
		for i, result := range results {
			if len(result.GeoJSON) == 0 || bytes.Equal(bytes.TrimSpace(result.GeoJSON), []byte("null")) {
				return nil, fmt.Errorf("Nominatim result %d in %s has no geojson; request polygon_geojson=1 when exporting", i+1, path)
			}
			g, err := decodeGeometry(result.GeoJSON, 0)
			if err != nil {
				return nil, fmt.Errorf("Nominatim result %d in %s: %w", i+1, path, err)
			}
			collection = append(collection, g)
		}
		return collection, nil
	}

	var document struct {
		Type     string            `json:"type"`
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse GeoJSON %s: %w", path, err)
	}
	switch document.Type {
	case "FeatureCollection":
		if len(document.Features) == 0 {
			return nil, fmt.Errorf("FeatureCollection in %s is empty", path)
		}
		collection := make(orb.Collection, 0, len(document.Features))
		for i, feature := range document.Features {
			g, err := decodeFeature(feature)
			if err != nil {
				return nil, fmt.Errorf("feature %d in %s: %w", i+1, path, err)
			}
			collection = append(collection, g)
		}
		return collection, nil
	case "Feature":
		g, err := decodeFeature(data)
		if err != nil {
			return nil, fmt.Errorf("feature in %s: %w", path, err)
		}
		return orb.Collection{g}, nil
	default:
		g, err := decodeGeometry(data, 0)
		if err != nil {
			return nil, fmt.Errorf("geometry in %s: %w", path, err)
		}
		return orb.Collection{g}, nil
	}
}

func validateGeoJSONLinearGeometry(geometry orb.Geometry) error {
	checkLine := func(line orb.LineString) error {
		if len(line) < 2 {
			return fmt.Errorf("a GeoJSON LineString must have at least two positions")
		}
		return nil
	}
	checkPolygon := func(polygon orb.Polygon) error {
		for _, ring := range polygon {
			if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
				return fmt.Errorf("a GeoJSON polygon ring must have at least four positions and be closed")
			}
		}
		return nil
	}
	switch geometry := geometry.(type) {
	case orb.LineString:
		return checkLine(geometry)
	case orb.MultiLineString:
		for _, line := range geometry {
			if err := checkLine(line); err != nil {
				return err
			}
		}
	case orb.Polygon:
		return checkPolygon(geometry)
	case orb.MultiPolygon:
		for _, polygon := range geometry {
			if err := checkPolygon(polygon); err != nil {
				return err
			}
		}
	}
	return nil
}

// output gets called if there is a test failure for debugging.
func output(name string, r *geojson.FeatureCollection) {
	f := loadFeature("./data/" + name + ".geojson")
	if f.Properties == nil {
		f.Properties = make(geojson.Properties)
	}

	f.Properties["fill"] = "#FF0000"
	f.Properties["fill-opacity"] = "0.5"
	f.Properties["stroke"] = "#FF0000"
	f.Properties["name"] = "original"
	r.Append(f)

	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		log.Fatalf("error marshalling json: %v", err)
	}

	err = os.WriteFile("failure_"+name+".geojson", data, 0644)
	if err != nil {
		log.Fatalf("write file failure: %v", err)
	}
}

// output gets called if there is a test failure for debugging.
func output2(name string, r *geojson.FeatureCollection, wg *sync.WaitGroup) {
	defer wg.Done()
	data, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		log.Fatalf("error marshalling json: %v", err)
	}

	err = os.WriteFile(name+".geojson", data, 0644)
	if err != nil {
		log.Fatalf("write file failure: %v", err)
	}
}

func getZoomCount(g orb.Geometry, minz int, maxz int) map[int]int64 {

	info := make(map[int]int64)
	for z := minz; z <= maxz; z++ {
		count, err := cover.Count(orb.Collection{g}, maptile.Zoom(z))
		if err != nil {
			log.Error(err)
			continue
		}
		info[z] = count
	}
	return info
}
