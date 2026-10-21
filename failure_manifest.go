package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/paulmach/orb/maptile"
)

const FailureManifestVersion = 1

// FailureTarget locates the original output without keeping credentials or a
// service endpoint in the manifest. Paths can be relative or absolute.
type FailureTarget struct {
	Kind                string `json:"kind"`
	Path                string `json:"path,omitempty"`
	Bucket              string `json:"bucket,omitempty"`
	Prefix              string `json:"prefix,omitempty"`
	TilePath            string `json:"tile_path,omitempty"`
	EndpointFingerprint string `json:"endpoint_fingerprint,omitempty"`
}

type FailureRecord struct {
	Z      int    `json:"z"`
	X      uint32 `json:"x"`
	Y      uint32 `json:"y"`
	Source string `json:"source"`
	Error  string `json:"error"`
}

type FailureManifestData struct {
	Version int             `json:"version"`
	MapName string          `json:"map_name"`
	Format  string          `json:"format"`
	Target  *FailureTarget  `json:"target,omitempty"`
	Tiles   []FailureRecord `json:"tiles"`
}

type failureKey struct {
	tile   maptile.Tile
	source string
}

// FailureManifest tracks unresolved failures. Resolve must be called after the
// destination write completes, including after an asynchronous SQLite write.
type FailureManifest struct {
	mu      sync.RWMutex
	mapName string
	format  string
	target  *FailureTarget
	tiles   map[failureKey]FailureRecord
}

func newFailureManifest(mapName, format string) *FailureManifest {
	return &FailureManifest{mapName: mapName, format: strings.ToLower(strings.TrimSpace(format)), tiles: make(map[failureKey]FailureRecord)}
}

func (manifest *FailureManifest) SetTarget(target FailureTarget) error {
	if err := validateFailureTarget(target); err != nil {
		return err
	}
	if target.Kind == "file" || target.Kind == "mbtiles" {
		absolute, err := filepath.Abs(target.Path)
		if err != nil {
			return fmt.Errorf("resolve failure target path: %w", err)
		}
		target.Path = absolute
	} else if target.Kind == "s3" {
		target.EndpointFingerprint = strings.ToLower(target.EndpointFingerprint)
	}
	manifest.mu.Lock()
	defer manifest.mu.Unlock()
	manifest.target = &target
	return nil
}

func (manifest *FailureManifest) Target() (FailureTarget, bool) {
	manifest.mu.RLock()
	defer manifest.mu.RUnlock()
	if manifest.target == nil {
		return FailureTarget{}, false
	}
	return *manifest.target, true
}

func (manifest *FailureManifest) Record(tile maptile.Tile, sourceTemplate string, err error) error {
	if err == nil {
		return fmt.Errorf("cannot record a tile failure without an error")
	}
	if coordinateErr := validateFailureCoordinates(int(tile.Z), tile.X, tile.Y); coordinateErr != nil {
		return coordinateErr
	}
	source, fingerprintErr := FailureSourceFingerprint(sourceTemplate)
	if fingerprintErr != nil {
		return fingerprintErr
	}
	manifest.mu.Lock()
	defer manifest.mu.Unlock()
	if manifest.tiles == nil {
		manifest.tiles = make(map[failureKey]FailureRecord)
	}
	manifest.tiles[failureKey{tile: tile, source: source}] = FailureRecord{Z: int(tile.Z), X: tile.X, Y: tile.Y, Source: source, Error: safeFailureError(err)}
	return nil
}

func (manifest *FailureManifest) Resolve(tile maptile.Tile, sourceTemplate string) error {
	source, err := FailureSourceFingerprint(sourceTemplate)
	if err != nil {
		return err
	}
	manifest.mu.Lock()
	defer manifest.mu.Unlock()
	delete(manifest.tiles, failureKey{tile: tile, source: source})
	return nil
}

func (manifest *FailureManifest) Len() int {
	manifest.mu.RLock()
	defer manifest.mu.RUnlock()
	return len(manifest.tiles)
}

func (manifest *FailureManifest) Snapshot() FailureManifestData {
	manifest.mu.RLock()
	defer manifest.mu.RUnlock()
	data := FailureManifestData{Version: FailureManifestVersion, MapName: manifest.mapName, Format: manifest.format, Tiles: make([]FailureRecord, 0, len(manifest.tiles))}
	if manifest.target != nil {
		target := *manifest.target
		data.Target = &target
	}
	for _, tile := range manifest.tiles {
		data.Tiles = append(data.Tiles, tile)
	}
	sort.Slice(data.Tiles, func(i, j int) bool {
		a, b := data.Tiles[i], data.Tiles[j]
		if a.Z != b.Z {
			return a.Z < b.Z
		}
		if a.X != b.X {
			return a.X < b.X
		}
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.Source < b.Source
	})
	return data
}

// Save writes a complete replacement beside the destination before renaming
// it. A successful repair retains an empty tiles array as an audit record.
func (manifest *FailureManifest) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("failure manifest path is required")
	}
	data := manifest.Snapshot()
	if err := validateFailureFormat(data.Format); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode failure manifest: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create failure manifest directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".failures-*")
	if err != nil {
		return fmt.Errorf("create failure manifest temporary file: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write failure manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync failure manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close failure manifest: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace failure manifest: %w", err)
	}
	return nil
}

func LoadFailureManifest(path string) (*FailureManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open failure manifest: %w", err)
	}
	defer file.Close()
	var document struct {
		Version *int              `json:"version"`
		MapName string            `json:"map_name"`
		Format  string            `json:"format"`
		Target  *FailureTarget    `json:"target,omitempty"`
		Tiles   []json.RawMessage `json:"tiles"`
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse failure manifest: %w", err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return nil, err
	}
	if document.Version == nil || *document.Version != FailureManifestVersion {
		return nil, fmt.Errorf("unsupported failure manifest version (expected %d)", FailureManifestVersion)
	}
	if document.Tiles == nil {
		return nil, fmt.Errorf("failure manifest tiles must be an array")
	}
	document.Format = strings.ToLower(strings.TrimSpace(document.Format))
	if err := validateFailureFormat(document.Format); err != nil {
		return nil, err
	}
	manifest := newFailureManifest(document.MapName, document.Format)
	if document.Target != nil {
		if err := manifest.SetTarget(*document.Target); err != nil {
			return nil, err
		}
	}
	for i, raw := range document.Tiles {
		var tile struct {
			Z      *int    `json:"z"`
			X      *uint32 `json:"x"`
			Y      *uint32 `json:"y"`
			Source string  `json:"source"`
			Error  string  `json:"error"`
		}
		entry := json.NewDecoder(strings.NewReader(string(raw)))
		entry.DisallowUnknownFields()
		if err := entry.Decode(&tile); err != nil {
			return nil, fmt.Errorf("parse failed tile %d: %w", i+1, err)
		}
		if tile.Z == nil || tile.X == nil || tile.Y == nil {
			return nil, fmt.Errorf("failed tile %d requires z, x and y", i+1)
		}
		if err := validateFailureCoordinates(*tile.Z, *tile.X, *tile.Y); err != nil {
			return nil, fmt.Errorf("failed tile %d: %w", i+1, err)
		}
		source, err := hex.DecodeString(tile.Source)
		if err != nil || len(source) != sha256.Size {
			return nil, fmt.Errorf("failed tile %d has an invalid source fingerprint", i+1)
		}
		fingerprint := strings.ToLower(tile.Source)
		coordinate := maptile.Tile{Z: maptile.Zoom(*tile.Z), X: *tile.X, Y: *tile.Y}
		manifest.tiles[failureKey{tile: coordinate, source: fingerprint}] = FailureRecord{Z: *tile.Z, X: *tile.X, Y: *tile.Y, Source: fingerprint, Error: safeFailureError(errors.New(tile.Error))}
	}
	return manifest, nil
}

type RepairTile struct {
	Tile maptile.Tile
	URL  string
}

// RecordsForLayers resolves the recorded source hashes to current templates.
// It does not re-enumerate a polygon: only listed coordinates are returned.
func (manifest *FailureManifest) RecordsForLayers(layers []Layer) ([]RepairTile, error) {
	sources := make(map[int]map[string]string)
	for _, layer := range layers {
		if layer.Zoom < 0 || layer.Zoom > 30 {
			return nil, fmt.Errorf("repair layer zoom must be between 0 and 30")
		}
		fingerprint, err := FailureSourceFingerprint(layer.URL)
		if err != nil {
			return nil, fmt.Errorf("repair layer %d: %w", layer.Zoom, err)
		}
		if sources[layer.Zoom] == nil {
			sources[layer.Zoom] = make(map[string]string)
		}
		if _, exists := sources[layer.Zoom][fingerprint]; !exists {
			sources[layer.Zoom][fingerprint] = layer.URL
		}
	}
	data := manifest.Snapshot()
	tiles := make([]RepairTile, 0, len(data.Tiles))
	for _, record := range data.Tiles {
		template, exists := sources[record.Z][record.Source]
		if !exists {
			return nil, fmt.Errorf("failed tile %d/%d/%d does not match a current source layer; keep its URL and zoom configuration", record.Z, record.X, record.Y)
		}
		tiles = append(tiles, RepairTile{Tile: maptile.Tile{Z: maptile.Zoom(record.Z), X: record.X, Y: record.Y}, URL: template})
	}
	return tiles, nil
}

func validateFailureCoordinates(z int, x, y uint32) error {
	if z < 0 || z > 30 {
		return fmt.Errorf("tile zoom must be between 0 and 30")
	}
	limit := uint64(1) << uint(z)
	if uint64(x) >= limit || uint64(y) >= limit {
		return fmt.Errorf("tile %d/%d/%d is outside the XYZ matrix", z, x, y)
	}
	return nil
}

func validateFailureFormat(format string) error {
	switch format {
	case "png", "jpg", "jpeg", "pbf", "webp":
		return nil
	default:
		return fmt.Errorf("unsupported failure manifest tile format %q", format)
	}
}

func validateFailureTarget(target FailureTarget) error {
	if target.Kind == "file" || target.Kind == "s3" {
		if _, err := normalizeTilePath(target.TilePath); err != nil {
			return fmt.Errorf("invalid failure target tile_path: %w", err)
		}
	} else if target.TilePath != "" {
		return fmt.Errorf("tile_path applies only to file or S3 failure targets")
	}
	switch target.Kind {
	case "file", "mbtiles":
		if strings.TrimSpace(target.Path) == "" || target.Bucket != "" || target.Prefix != "" || target.EndpointFingerprint != "" {
			return fmt.Errorf("local failure target requires only a path")
		}
	case "s3":
		if strings.TrimSpace(target.Bucket) == "" || target.Path != "" {
			return fmt.Errorf("S3 failure target requires a bucket and no local path")
		}
		fingerprint, err := hex.DecodeString(target.EndpointFingerprint)
		if err != nil || len(fingerprint) != sha256.Size {
			return fmt.Errorf("S3 failure target requires a valid endpoint fingerprint")
		}
	default:
		return fmt.Errorf("unsupported failure target kind %q", target.Kind)
	}
	return nil
}

// FailureS3EndpointFingerprint binds a repair to the service endpoint and the
// effective signing region without writing either service URL or credentials.
func FailureS3EndpointFingerprint(endpoint, region string) (string, error) {
	endpoint, region = strings.TrimSpace(endpoint), strings.TrimSpace(region)
	if region == "" {
		return "", fmt.Errorf("S3 endpoint fingerprint requires a region")
	}
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("S3 endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
		u.Scheme = strings.ToLower(u.Scheme)
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
			port = ""
		}
		if port != "" {
			u.Host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		} else {
			u.Host = host
		}
		endpoint = u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/")
	}
	hash := sha256.Sum256([]byte(endpoint + "\n" + region))
	return hex.EncodeToString(hash[:]), nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("failure manifest contains more than one JSON value")
		}
		return fmt.Errorf("invalid trailing failure manifest data: %w", err)
	}
	return nil
}

// FailureSourceFingerprint preserves the layer identity but excludes common
// authentication fields so rotating a token does not invalidate a repair.
func FailureSourceFingerprint(sourceTemplate string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(sourceTemplate))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("failure source must be a valid HTTP(S) tile template")
	}
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	u.Fragment = ""
	query := u.Query()
	for key := range query {
		if sensitiveFailureQueryKey(key) {
			query.Del(key)
		}
	}
	u.RawQuery = query.Encode()
	digest := sha256.Sum256([]byte(u.String()))
	return hex.EncodeToString(digest[:]), nil
}

func sensitiveFailureQueryKey(key string) bool {
	key = strings.ToLower(key)
	if strings.HasPrefix(key, "x-amz-") || strings.HasPrefix(key, "x-goog-") {
		return true
	}
	switch key {
	case "token", "access_token", "tk", "key", "api_key", "apikey", "api-key", "authorization", "signature", "sig", "credential", "credentials", "secret", "client_secret", "password", "auth", "authkey", "expires":
		return true
	}
	return false
}

var failureURLPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var failureSecretPattern = regexp.MustCompile(`(?i)\b(token|access_token|tk|key|api[_-]?key|authorization|signature|secret|secret_key|client_secret|aws_secret_access_key|password|auth|authkey|credentials?|x-amz-[a-z0-9-]+|x-goog-[a-z0-9-]+)\s*[:=]\s*[^\s,;]+`)

func safeFailureError(err error) string {
	message := err.Error()
	var requestError *url.Error
	if errors.As(err, &requestError) {
		message = requestError.Op + ": " + requestError.Err.Error()
	}
	message = failureURLPattern.ReplaceAllStringFunc(message, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "[redacted URL]"
		}
		return u.Scheme + "://" + u.Host + "/[redacted]"
	})
	return failureSecretPattern.ReplaceAllString(message, "$1=[redacted]")
}
