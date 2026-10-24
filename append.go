package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readDownloadFileMetadata accepts string-valued metadata from older tiler
// versions as well as the numbers and objects used by other TileJSON tools.
func readDownloadFileMetadata(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMetadataBytes {
		return nil, fmt.Errorf("existing metadata.json exceeds 16 MiB")
	}
	metadata, err := archiveDecodeJSONMetadata(body)
	if err != nil {
		return nil, fmt.Errorf("read existing metadata.json: %w", err)
	}
	var coordinates map[string]json.RawMessage
	if err := json.Unmarshal(body, &coordinates); err != nil {
		return nil, err
	}
	for _, key := range []string{"bounds", "center"} {
		value := coordinates[key]
		if strings.TrimSpace(string(value)) == "null" {
			return nil, fmt.Errorf("existing metadata %s cannot be null", key)
		}
		var numbers []*float64
		if json.Unmarshal(value, &numbers) == nil && len(numbers) > 0 {
			parts := make([]string, len(numbers))
			for i, number := range numbers {
				if number == nil {
					return nil, fmt.Errorf("existing metadata %s coordinates cannot be null", key)
				}
				parts[i] = strconv.FormatFloat(*number, 'g', -1, 64)
			}
			metadata[key] = strings.Join(parts, ",")
		}
	}
	return metadata, nil
}

func readDownloadMBMetadata(db *sql.DB) (map[string]string, error) {
	var total int64
	if err := db.QueryRow("select coalesce(sum(coalesce(length(cast(name as blob)),0) + coalesce(length(cast(value as blob)),0)),0) from metadata").Scan(&total); err != nil {
		return nil, fmt.Errorf("read existing MBTiles metadata size: %w", err)
	}
	if total > maxMetadataBytes {
		return nil, fmt.Errorf("existing MBTiles metadata exceeds 16 MiB")
	}
	rows, err := db.Query("select name, value from metadata")
	if err != nil {
		return nil, fmt.Errorf("read existing MBTiles metadata: %w", err)
	}
	defer rows.Close()
	metadata := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		metadata[name] = value
	}
	return metadata, rows.Err()
}

// validateFileResumeMetadata runs before the output directory or its metadata
// is changed. The save operation repeats the check before publishing metadata.
func (task *Task) validateFileResumeMetadata() error {
	if task.outformat != "file" || !task.resume {
		return nil
	}
	_, err := task.fileMetadataForDownload()
	return err
}

func validateDownloadMBMetadata(metadata map[string]string) error {
	if sameTileEncoding(strings.TrimSpace(metadata["format"]), JPG) {
		metadata["format"] = JPG
	}
	if metadata["type"] != "overlay" && metadata["type"] != "baselayer" {
		metadata["type"] = "baselayer"
	}
	if strings.EqualFold(strings.TrimSpace(metadata["format"]), PBF) {
		// MBTiles requires the JSON metadata entry, rather than a separate
		// top-level vector_layers row, for describing vector tile layers.
		description, err := decodeTileJSONMetadata(metadata["json"])
		if err != nil {
			return fmt.Errorf("PBF MBTiles requires json metadata containing vector_layers")
		}
		if _, err := mergeDownloadVectorLayers(nil, description["vector_layers"]); err != nil {
			return fmt.Errorf("invalid PBF MBTiles vector_layers: %w", err)
		}
		if _, ok := description["vector_layers"].([]interface{}); !ok {
			return fmt.Errorf("PBF MBTiles requires vector_layers as a JSON array")
		}
		minZoom, maxZoom := 0, 30
		for key, destination := range map[string]*int{"minzoom": &minZoom, "maxzoom": &maxZoom} {
			if value := metadata[key]; value != "" {
				zoom, err := downloadMetadataZoom(value)
				if err != nil {
					return err
				}
				*destination = zoom
			}
		}
		if err := archiveValidateMBVectorMetadata(description, minZoom, maxZoom); err != nil {
			return fmt.Errorf("invalid PBF MBTiles vector metadata: %w", err)
		}
		encoded, err := json.Marshal(description)
		if err != nil {
			return err
		}
		metadata["json"] = string(encoded)
	}
	return archiveValidateMetadataSize(metadata)
}

func (task *Task) fileMetadataForDownload() (map[string]string, error) {
	metadata := task.MetaItems()
	pattern, err := normalizeTilePath(task.tilePath)
	if err != nil {
		return nil, err
	}
	metadata["tile_path"] = pattern
	if !task.resume {
		return mergeDownloadMetadata(nil, metadata, true)
	}
	existing, err := readDownloadFileMetadata(filepath.Join(task.File, "metadata.json"))
	if err != nil {
		return nil, err
	}
	if existing == nil && pattern != defaultTilePath {
		entries, err := os.ReadDir(task.File)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(entries) != 0 {
			return nil, fmt.Errorf("cannot resume a custom tile_path in an existing directory without metadata.json; keep the original layout or use an empty output")
		}
	}
	return mergeDownloadMetadata(existing, metadata, true)
}

// mergeDownloadMetadata keeps the original dataset identity while extending
// its geographic and zoom coverage. It is also used for ordinary resume jobs:
// narrowing a retry region must never hide tiles already present in the output.
func mergeDownloadMetadata(existing, incoming map[string]string, fileOutput bool) (map[string]string, error) {
	result := make(map[string]string, len(existing)+len(incoming))
	for name, value := range incoming {
		result[name] = value
	}
	format := strings.ToLower(strings.TrimSpace(incoming["format"]))
	if err := validateFailureFormat(format); err != nil {
		return nil, err
	}
	if existing != nil {
		oldFormat := strings.ToLower(strings.TrimSpace(existing["format"]))
		if oldFormat != "" && oldFormat != format && (fileOutput || !sameTileEncoding(oldFormat, format)) {
			return nil, fmt.Errorf("existing output tile format %q differs from %q; use a different output", oldFormat, format)
		}
		if !fileOutput && oldFormat == "" {
			return nil, fmt.Errorf("existing MBTiles metadata is missing format; cannot safely resume or append")
		}
		if existing["minzoom"] != "" && existing["maxzoom"] != "" {
			low, lowErr := downloadMetadataZoom(existing["minzoom"])
			high, highErr := downloadMetadataZoom(existing["maxzoom"])
			if lowErr != nil || highErr != nil || low > high {
				return nil, fmt.Errorf("invalid existing output zoom interval")
			}
		}
		for name, value := range existing {
			if value != "" || !archiveFlatMetadataKey(name) {
				result[name] = value
			}
		}
	}
	result["format"] = format
	if fileOutput {
		pattern, err := normalizeTilePath(incoming["tile_path"])
		if err != nil {
			return nil, err
		}
		if existing != nil {
			oldPattern, err := tilePathFromMetadata(existing)
			if err != nil {
				return nil, fmt.Errorf("invalid existing tile_path metadata: %w", err)
			}
			if oldPattern != pattern {
				return nil, fmt.Errorf("existing output tile_path %q differs from %q; keep the original layout or use a different output", oldPattern, pattern)
			}
		}
		result["tile_path"] = pattern
		result["scheme"] = "xyz"
		if strings.Contains(pattern, "{-y}") {
			result["scheme"] = "tms"
		}
	} else {
		delete(result, "tile_path")
	}
	newBounds, err := downloadMetadataBounds(incoming["bounds"])
	if err != nil {
		return nil, fmt.Errorf("invalid download bounds: %w", err)
	}
	if existing["bounds"] != "" {
		oldBounds, err := downloadMetadataBounds(existing["bounds"])
		if err != nil {
			return nil, fmt.Errorf("invalid existing bounds: %w", err)
		}
		newBounds[0] = math.Min(newBounds[0], oldBounds[0])
		newBounds[1] = math.Min(newBounds[1], oldBounds[1])
		newBounds[2] = math.Max(newBounds[2], oldBounds[2])
		newBounds[3] = math.Max(newBounds[3], oldBounds[3])
	}
	minZoom, err := downloadMetadataZoom(incoming["minzoom"])
	if err != nil {
		return nil, err
	}
	maxZoom, err := downloadMetadataZoom(incoming["maxzoom"])
	if err != nil || minZoom > maxZoom {
		return nil, fmt.Errorf("invalid download zoom metadata")
	}
	if existing["minzoom"] != "" {
		old, err := downloadMetadataZoom(existing["minzoom"])
		if err != nil {
			return nil, fmt.Errorf("invalid existing minzoom: %w", err)
		}
		if old < minZoom {
			minZoom = old
		}
	}
	if existing["maxzoom"] != "" {
		old, err := downloadMetadataZoom(existing["maxzoom"])
		if err != nil {
			return nil, fmt.Errorf("invalid existing maxzoom: %w", err)
		}
		if old > maxZoom {
			maxZoom = old
		}
	}
	boundParts := make([]string, len(newBounds))
	for i, number := range newBounds {
		boundParts[i] = formatDownloadCoordinate(number)
	}
	result["bounds"] = strings.Join(boundParts, ",")
	result["minzoom"], result["maxzoom"] = strconv.Itoa(minZoom), strconv.Itoa(maxZoom)
	result["center"] = fmt.Sprintf("%s,%s,%d", formatDownloadCoordinate((newBounds[0]+newBounds[2])/2), formatDownloadCoordinate((newBounds[1]+newBounds[3])/2), (minZoom+maxZoom)/2)
	if err := mergeDownloadVectorMetadata(existing, incoming, result); err != nil {
		return nil, err
	}
	return result, nil
}

func formatDownloadCoordinate(number float64) string {
	text := strconv.FormatFloat(number, 'f', -1, 64)
	point := strings.IndexByte(text, '.')
	if point < 0 {
		return text + ".000000"
	}
	if digits := len(text) - point - 1; digits < 6 {
		return text + strings.Repeat("0", 6-digits)
	}
	return text
}

func downloadMetadataBounds(text string) ([4]float64, error) {
	var result [4]float64
	parts := strings.Split(text, ",")
	if len(parts) != 4 {
		return result, fmt.Errorf("expected west,south,east,north")
	}
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return result, fmt.Errorf("bounds must contain finite numbers")
		}
		result[i] = value
	}
	if result[0] < -180 || result[2] > 180 || result[1] < -90 || result[3] > 90 || result[0] > result[2] || result[1] > result[3] {
		return result, fmt.Errorf("bounds are outside the geographic range")
	}
	return result, nil
}

func downloadMetadataZoom(text string) (int, error) {
	z, err := strconv.Atoi(text)
	if err != nil || z < 0 || z > 30 {
		return 0, fmt.Errorf("zoom metadata must be an integer from 0 to 30")
	}
	return z, nil
}

func decodeDownloadVectorMetadata(metadata map[string]string) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	if text := strings.TrimSpace(metadata["json"]); text != "" {
		parsed, err := decodeTileJSONMetadata(text)
		if err != nil {
			return nil, fmt.Errorf("vector json metadata must be a JSON object")
		}
		result = parsed
		if layers, exists := result["vector_layers"]; exists && layers == nil {
			return nil, fmt.Errorf("vector_layers must be a JSON array")
		}
	}
	if text := strings.TrimSpace(metadata["vector_layers"]); text != "" {
		direct, err := decodeVectorLayers([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("vector_layers must be a JSON array")
		}
		layers, err := mergeDownloadVectorLayers(result["vector_layers"], direct)
		if err != nil {
			return nil, err
		}
		result["vector_layers"] = layers
	}
	return result, nil
}

func mergeDownloadVectorMetadata(existing, incoming, result map[string]string) error {
	oldJSON, err := decodeDownloadVectorMetadata(existing)
	if err != nil {
		return fmt.Errorf("invalid existing vector metadata: %w", err)
	}
	newJSON, err := decodeDownloadVectorMetadata(incoming)
	if err != nil {
		return fmt.Errorf("invalid download vector metadata: %w", err)
	}
	// An old typed extension also takes precedence over an incoming string
	// stored outside JSON. Preserve an explicitly supplied old flat value.
	for key := range oldJSON {
		if _, hasFlat := existing[key]; !archiveFlatMetadataKey(key) && !hasFlat {
			delete(result, key)
		}
	}
	for key, value := range newJSON {
		if key == "vector_layers" {
			layers, err := mergeDownloadVectorLayers(oldJSON[key], value)
			if err != nil {
				return err
			}
			oldJSON[key] = layers
		} else if _, exists := oldJSON[key]; !exists {
			oldJSON[key] = value
		}
	}
	if layers, exists := oldJSON["vector_layers"]; exists {
		if _, err := mergeDownloadVectorLayers(nil, layers); err != nil {
			return err
		}
		if existing["vector_layers"] != "" || incoming["vector_layers"] != "" {
			encoded, err := json.Marshal(layers)
			if err != nil {
				return err
			}
			result["vector_layers"] = string(encoded)
		}
	}
	if len(oldJSON) == 0 {
		return nil
	}
	if err := syncTileJSONCoverage(oldJSON, result); err != nil {
		return err
	}
	encoded, err := json.Marshal(oldJSON)
	if err != nil {
		return err
	}
	result["json"] = string(encoded)
	return nil
}

// Keep existing full TileJSON coverage fields consistent with the normalized
// dataset metadata, without adding optional fields or changing custom keys.
func syncTileJSONCoverage(object map[string]interface{}, metadata map[string]string) error {
	for _, field := range []struct {
		name  string
		count int
	}{{"bounds", 4}, {"center", 3}} {
		if _, exists := object[field.name]; exists {
			numbers, err := archiveNumbers(metadata[field.name], field.count)
			if err != nil {
				return err
			}
			object[field.name] = numbers
		}
	}
	for _, key := range []string{"minzoom", "maxzoom"} {
		if _, exists := object[key]; exists {
			zoom, err := strconv.Atoi(metadata[key])
			if err != nil {
				return err
			}
			object[key] = zoom
		}
	}
	return nil
}

func mergeDownloadVectorLayers(previous, current interface{}) ([]interface{}, error) {
	result := []interface{}{}
	index := make(map[string]map[string]interface{})
	for _, value := range []interface{}{previous, current} {
		if value == nil {
			continue
		}
		layers, ok := value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("vector_layers must be a JSON array")
		}
		if err := archiveValidateVectorLayers(layers); err != nil {
			return nil, err
		}
		for _, value := range layers {
			layer := value.(map[string]interface{})
			id := layer["id"].(string)
			if strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("vector_layers entries require a nonempty string id")
			}
			old, exists := index[id]
			if !exists {
				index[id] = layer
				result = append(result, layer)
				continue
			}
			if err := mergeVectorLayer(old, layer); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// Both download append and archive conversion use the same merge rules after
// validating their input schema. Keep omitted zoom limits unbounded.
func mergeVectorLayer(previous, current map[string]interface{}) error {
	fields := previous["fields"].(map[string]interface{})
	for key, value := range current["fields"].(map[string]interface{}) {
		if old, exists := fields[key]; exists && old != value {
			return fmt.Errorf("vector layer %q field %q has conflicting types", previous["id"], key)
		}
		fields[key] = value
	}
	for _, key := range []string{"minzoom", "maxzoom"} {
		a, hadPrevious := previous[key].(float64)
		b, hasCurrent := current[key].(float64)
		if !hadPrevious || !hasCurrent {
			delete(previous, key)
		} else if key == "minzoom" {
			previous[key] = math.Min(a, b)
		} else {
			previous[key] = math.Max(a, b)
		}
	}
	for key, value := range current {
		if key == "fields" || key == "minzoom" || key == "maxzoom" {
			continue
		}
		if _, exists := previous[key]; !exists {
			previous[key] = value
		}
	}
	return nil
}
