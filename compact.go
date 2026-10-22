package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// These dimensions and bit widths are defined by Esri's Compact Cache V2
// specification, not tunable batching policies.
const (
	compactPacketSize          = 128
	compactRecordCount         = compactPacketSize * compactPacketSize
	compactHeaderSize          = 64
	compactIndexSize           = compactRecordCount * 8
	compactDataStart           = compactHeaderSize + compactIndexSize
	compactOffsetMask   uint64 = (1 << 40) - 1
	compactMaxTileBytes        = (1 << 24) - 1
	compactMercatorEdge        = math.Pi * 6378137
)

const compactMercatorWKT = `PROJCS["WGS_1984_Web_Mercator_Auxiliary_Sphere",GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],UNIT["Degree",0.0174532925199433]],PROJECTION["Mercator_Auxiliary_Sphere"],PARAMETER["False_Easting",0.0],PARAMETER["False_Northing",0.0],PARAMETER["Central_Meridian",0.0],PARAMETER["Standard_Parallel_1",0.0],PARAMETER["Auxiliary_Sphere_Type",0.0],UNIT["Meter",1.0]]`

var compactBundleName = regexp.MustCompile(`^R([0-9a-fA-F]{4,})C([0-9a-fA-F]{4,})\.bundle$`)

type compactSpatialReference struct {
	WKT        string `xml:"WKT"`
	WKID       int    `xml:"WKID"`
	LatestWKID int    `xml:"LatestWKID"`
}
type compactLOD struct {
	Level      int     `xml:"LevelID"`
	Scale      float64 `xml:"Scale"`
	Resolution float64 `xml:"Resolution"`
}
type compactXML struct {
	XMLName xml.Name `xml:"CacheInfo"`
	Cache   struct {
		Spatial compactSpatialReference `xml:"SpatialReference"`
		Origin  struct {
			X float64 `xml:"X"`
			Y float64 `xml:"Y"`
		} `xml:"TileOrigin"`
		Width      int          `xml:"TileCols"`
		Height     int          `xml:"TileRows"`
		DPI        int          `xml:"DPI"`
		PreciseDPI int          `xml:"PreciseDPI"`
		LODs       []compactLOD `xml:"LODInfos>LODInfo"`
	} `xml:"TileCacheInfo"`
	Image struct {
		Format  string `xml:"CacheTileFormat"`
		Quality int    `xml:"CompressionQuality"`
	} `xml:"TileImageInfo"`
	Storage struct {
		Format     string `xml:"StorageFormat"`
		PacketSize int    `xml:"PacketSize"`
	} `xml:"CacheStorageInfo"`
}
type compactEnvelope struct {
	XMLName xml.Name                `xml:"EnvelopeN"`
	XMin    float64                 `xml:"XMin"`
	YMin    float64                 `xml:"YMin"`
	XMax    float64                 `xml:"XMax"`
	YMax    float64                 `xml:"YMax"`
	Spatial compactSpatialReference `xml:"SpatialReference"`
}

type compactCache struct {
	mu       sync.Mutex
	root     string
	metadata map[string]string
	format   string
	tileSize int
	resume   bool
	closed   bool
}

func compactIsDirectory(root string) (bool, error) {
	info, err := os.Lstat(filepath.Join(root, "conf.xml"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("compact conf.xml must be a regular file")
	}
	return true, nil
}

func compactReadXML(path string, target interface{}) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return fmt.Errorf("compact metadata must be a regular file of at most 16 MiB: %s", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxMetadataBytes {
		return fmt.Errorf("compact metadata exceeds 16 MiB")
	}
	if err := xml.Unmarshal(body, target); err != nil {
		return fmt.Errorf("read compact metadata: %w", err)
	}
	return nil
}

func compactReadConfig(root string) (compactXML, string, error) {
	var config compactXML
	if err := compactReadXML(filepath.Join(root, "conf.xml"), &config); err != nil {
		return config, "", err
	}
	if config.Storage.Format != "esriMapCacheStorageModeCompactV2" || config.Storage.PacketSize != compactPacketSize {
		return config, "", fmt.Errorf("cache must use Compact v2 with 128 by 128 tiles per bundle")
	}
	wkid := config.Cache.Spatial.WKID
	if wkid != 3857 && wkid != 102100 && wkid != 102113 && config.Cache.Spatial.LatestWKID != 3857 {
		return config, "", fmt.Errorf("Compact v2 supports only the Web Mercator grid (EPSG:3857)")
	}
	if config.Cache.Width != config.Cache.Height || (config.Cache.Width != 256 && config.Cache.Width != 512) {
		return config, "", fmt.Errorf("Compact v2 tile size must be 256 or 512 pixels square")
	}
	near := func(a, b float64) bool {
		return !math.IsNaN(a) && !math.IsInf(a, 0) && math.Abs(a-b) <= math.Max(1, math.Abs(b))*1e-9
	}
	if !near(config.Cache.Origin.X, -compactMercatorEdge) || !near(config.Cache.Origin.Y, compactMercatorEdge) {
		return config, "", fmt.Errorf("Compact v2 origin does not match the global Web Mercator grid")
	}
	if len(config.Cache.LODs) == 0 || len(config.Cache.LODs) > 31 {
		return config, "", fmt.Errorf("Compact v2 must define zoom levels from 0 through at most 30")
	}
	originError := math.Max(math.Abs(config.Cache.Origin.X+compactMercatorEdge), math.Abs(config.Cache.Origin.Y-compactMercatorEdge))
	for z, lod := range config.Cache.LODs {
		pixelsAcrossWorld := float64(config.Cache.Width) * math.Exp2(float64(z))
		expected := 2 * compactMercatorEdge / pixelsAcrossWorld
		resolutionError := math.Abs(lod.Resolution - expected)
		// Allow rounded Esri metadata, but require the combined origin and
		// accumulated resolution error to stay within half an output pixel.
		// Relative tolerance alone can admit whole-tile shifts at high zooms.
		gridError := originError + resolutionError*pixelsAcrossWorld
		matchesResolution := resolutionError <= expected*1e-9 && gridError <= expected/2
		if lod.Level != z || !matchesResolution {
			return config, "", fmt.Errorf("Compact v2 LOD resolutions must match global XYZ zoom levels starting at 0")
		}
	}
	format := strings.ToUpper(config.Image.Format)
	switch format {
	case "JPEG", "JPG":
		return config, JPG, nil
	case "PNG", "PNG8", "PNG24", "PNG32":
		return config, PNG, nil
	case "MIXED":
		return config, "", nil
	default:
		return config, "", fmt.Errorf("Compact v2 raster cache must contain PNG or JPEG tiles")
	}
}

func compactReadMetadata(root string) (map[string]string, error) {
	config, format, err := compactReadConfig(root)
	if err != nil {
		return nil, err
	}
	metadata, err := archiveReadDirectoryMetadata(root)
	if err != nil {
		return nil, err
	}
	if previous := metadata["format"]; previous != "" && format != "" && !sameTileEncoding(previous, format) {
		return nil, fmt.Errorf("compact metadata.json format does not match conf.xml")
	}
	metadata["format"], metadata["scheme"], metadata["pixel_scale"] = format, "xyz", strconv.Itoa(config.Cache.Width)
	delete(metadata, "tile_path")
	if metadata["minzoom"] == "" {
		metadata["minzoom"] = "0"
	}
	metadata["maxzoom"] = strconv.Itoa(len(config.Cache.LODs) - 1)
	if min, err := downloadMetadataZoom(metadata["minzoom"]); err != nil || min >= len(config.Cache.LODs) {
		return nil, fmt.Errorf("invalid compact minimum zoom")
	}
	if metadata["bounds"] == "" {
		var envelope compactEnvelope
		err := compactReadXML(filepath.Join(root, "conf.cdi"), &envelope)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			values := []float64{envelope.XMin, envelope.YMin, envelope.XMax, envelope.YMax}
			for _, v := range values {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, fmt.Errorf("compact extent must be finite")
				}
			}
			if envelope.XMin > envelope.XMax || envelope.YMin > envelope.YMax {
				return nil, fmt.Errorf("invalid compact extent")
			}
			lon := func(x float64) float64 { return math.Max(-180, math.Min(180, x/compactMercatorEdge*180)) }
			lat := func(y float64) float64 { return math.Atan(math.Sinh(y/6378137)) * 180 / math.Pi }
			metadata["bounds"] = archiveBoundsText([]float64{lon(envelope.XMin), lat(envelope.YMin), lon(envelope.XMax), lat(envelope.YMax)})
		} else {
			metadata["bounds"] = "-180,-85.0511287798066,180,85.0511287798066"
		}
	}
	if _, err := downloadMetadataBounds(metadata["bounds"]); err != nil {
		return nil, err
	}
	return metadata, nil
}

func compactOpen(root string, metadata map[string]string, resume bool) (*compactCache, error) {
	format, err := archiveFormat(metadata["format"])
	if err != nil {
		return nil, err
	}
	if format != PNG && format != JPG {
		return nil, fmt.Errorf("Compact v2 output supports PNG and JPEG raster tiles only")
	}
	if err := validateOutputFilePath(root, root); err != nil {
		return nil, err
	}
	present, err := compactIsDirectory(root)
	if err != nil {
		return nil, err
	}
	var previous map[string]string
	tileSize := 0
	if present {
		previous, err = compactReadMetadata(root)
		if err != nil {
			return nil, err
		}
		if previous["format"] == "" {
			return nil, fmt.Errorf("cannot append to a mixed-format compact cache; convert to a new target")
		}
		tileSize, _ = strconv.Atoi(previous["pixel_scale"])
		// Initialization writes usable metadata before any network request. If
		// that request was cancelled, no bundle exists to fix the image size yet.
		if _, statErr := os.Lstat(filepath.Join(root, "_alllayers")); os.IsNotExist(statErr) {
			tileSize = 0
		} else if statErr != nil {
			return nil, statErr
		}
	} else if entries, readErr := os.ReadDir(root); readErr == nil && len(entries) > 0 {
		return nil, fmt.Errorf("existing Compact v2 output has no conf.xml; use a new empty directory")
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	incoming := make(map[string]string, len(metadata))
	for k, v := range metadata {
		incoming[k] = v
	}
	incoming["format"] = format
	merged, err := mergeDownloadMetadata(previous, incoming, false)
	if err != nil {
		return nil, err
	}
	merged["scheme"] = "xyz"
	cache := &compactCache{root: root, metadata: merged, format: format, tileSize: tileSize, resume: resume}
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	if err := cache.writeMetadata(); err != nil {
		return nil, err
	}
	return cache, nil
}

func compactCoordinate(z int, x, y uint32) error {
	if z < 0 || z > 30 || uint64(x) >= uint64(1)<<uint(z) || uint64(y) >= uint64(1)<<uint(z) {
		return fmt.Errorf("invalid Compact v2 XYZ coordinate %d/%d/%d", z, x, y)
	}
	return nil
}
func compactPath(root string, z int, x, y uint32) string {
	return filepath.Join(root, "_alllayers", fmt.Sprintf("L%02d", z), fmt.Sprintf("R%04xC%04x.bundle", y/compactPacketSize*compactPacketSize, x/compactPacketSize*compactPacketSize))
}
func compactIndexOffset(x, y uint32) int64 {
	return compactHeaderSize + 8*int64((y%compactPacketSize)*compactPacketSize+x%compactPacketSize)
}

func compactHeader() []byte {
	h := make([]byte, compactDataStart)
	for off, value := range map[int]uint32{0: 3, 4: compactRecordCount, 12: 5, 40: 20 + compactIndexSize, 44: 3, 48: 16, 52: compactRecordCount, 56: 5, 60: compactIndexSize} {
		binary.LittleEndian.PutUint32(h[off:], value)
	}
	binary.LittleEndian.PutUint64(h[24:], compactDataStart)
	binary.LittleEndian.PutUint64(h[32:], 40)
	return h
}
func compactCheckBundle(file *os.File) (int64, uint32, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < compactDataStart {
		return 0, 0, fmt.Errorf("compact bundle is not a complete regular file")
	}
	var h [compactHeaderSize]byte
	if _, err := file.ReadAt(h[:], 0); err != nil {
		return 0, 0, err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 3 || binary.LittleEndian.Uint32(h[4:8]) != compactRecordCount || binary.LittleEndian.Uint32(h[12:16]) != 5 || binary.LittleEndian.Uint32(h[60:64]) != compactIndexSize {
		return 0, 0, fmt.Errorf("invalid Compact v2 bundle header")
	}
	return info.Size(), binary.LittleEndian.Uint32(h[8:12]), nil
}
func compactReadRecord(file *os.File, indexOffset, size int64) (uint64, uint64, error) {
	var record [8]byte
	if _, err := file.ReadAt(record[:], indexOffset); err != nil {
		return 0, 0, err
	}
	return compactDecodeRecord(file, binary.LittleEndian.Uint64(record[:]), size)
}

func compactDecodeRecord(file *os.File, entry uint64, size int64) (uint64, uint64, error) {
	offset, length := entry&compactOffsetMask, entry>>40
	if length == 0 {
		return 0, 0, nil
	}
	if offset < compactDataStart+4 || offset > uint64(size) || length > uint64(size)-offset {
		return 0, 0, fmt.Errorf("compact tile index points outside the bundle")
	}
	var prefix [4]byte
	if _, err := file.ReadAt(prefix[:], int64(offset)-4); err != nil {
		return 0, 0, err
	}
	if uint64(binary.LittleEndian.Uint32(prefix[:])) != length {
		return 0, 0, fmt.Errorf("compact tile length prefix differs from its index")
	}
	return offset, length, nil
}

func (cache *compactCache) Exists(ctx context.Context, z int, x, y uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := compactCoordinate(z, x, y); err != nil {
		return false, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return false, fmt.Errorf("compact cache is closed")
	}
	path := compactPath(cache.root, z, x, y)
	if err := validateOutputFilePath(cache.root, path); err != nil {
		return false, err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	size, _, err := compactCheckBundle(file)
	if err != nil {
		return false, err
	}
	_, length, err := compactReadRecord(file, compactIndexOffset(x, y), size)
	return length > 0, err
}

func (cache *compactCache) Save(ctx context.Context, z int, x, y uint32, body []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := compactCoordinate(z, x, y); err != nil {
		return err
	}
	if len(body) == 0 || len(body) > compactMaxTileBytes {
		return fmt.Errorf("Compact v2 tile size must be between 1 and 16777215 bytes")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("compact tile image header: %w", err)
	}
	if !sameTileEncoding(format, cache.format) {
		return fmt.Errorf("compact tile encoding differs from the cache format")
	}
	if config.Width != config.Height || (config.Width != 256 && config.Width != 512) {
		return fmt.Errorf("Compact v2 tiles must be 256 or 512 pixels square")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return fmt.Errorf("compact cache is closed")
	}
	if cache.tileSize != 0 && cache.tileSize != config.Width {
		return fmt.Errorf("compact tile dimensions differ from the existing cache")
	}
	if cache.tileSize == 0 {
		cache.tileSize = config.Width
		if err := cache.writeMetadata(); err != nil {
			return err
		}
	}
	path := compactPath(cache.root, z, x, y)
	if err := validateOutputFilePath(cache.root, path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
	if err == nil {
		if _, err = file.Write(compactHeader()); err != nil {
			file.Close()
			os.Remove(path)
			return err
		}
	} else if os.IsExist(err) {
		file, err = os.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	size, maximum, err := compactCheckBundle(file)
	if err != nil {
		return err
	}
	indexOffset := compactIndexOffset(x, y)
	_, oldSize, err := compactReadRecord(file, indexOffset, size)
	if err != nil {
		return err
	}
	if cache.resume && oldSize > 0 {
		return nil
	}
	if uint64(size)+4+uint64(len(body)) > compactOffsetMask {
		return fmt.Errorf("compact bundle exceeds its 40-bit offset limit")
	}
	// Do not check cancellation between data and index publication. A normally
	// cancelled task leaves this tile either absent or fully indexed. This is
	// not a cross-process lock or a guarantee against torn writes on power loss.
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, err := file.WriteAt(prefix[:], size); err != nil {
		return err
	}
	if _, err := file.WriteAt(body, size+4); err != nil {
		return err
	}
	if uint32(len(body)) > maximum {
		maximum = uint32(len(body))
	}
	binary.LittleEndian.PutUint32(prefix[:], maximum)
	if _, err := file.WriteAt(prefix[:], 8); err != nil {
		return err
	}
	var record [8]byte
	binary.LittleEndian.PutUint64(record[:], uint64(size+4+int64(len(body))))
	if _, err := file.WriteAt(record[:], 24); err != nil {
		return err
	}
	binary.LittleEndian.PutUint64(record[:], uint64(size+4)|(uint64(len(body))<<40))
	if _, err := file.WriteAt(record[:], indexOffset); err != nil {
		return err
	}
	return nil
}

func (cache *compactCache) writeMetadata() error {
	size := cache.tileSize
	if size == 0 {
		size = 256
	}
	low, err := downloadMetadataZoom(cache.metadata["minzoom"])
	if err != nil {
		return err
	}
	high, err := downloadMetadataZoom(cache.metadata["maxzoom"])
	if err != nil || low > high {
		return fmt.Errorf("invalid compact zoom interval")
	}
	bounds, err := downloadMetadataBounds(cache.metadata["bounds"])
	if err != nil {
		return err
	}
	var config compactXML
	config.Cache.Spatial = compactSpatialReference{WKT: compactMercatorWKT, WKID: 102100, LatestWKID: 3857}
	config.Cache.Origin.X, config.Cache.Origin.Y = -compactMercatorEdge, compactMercatorEdge
	config.Cache.Width, config.Cache.Height, config.Cache.DPI, config.Cache.PreciseDPI = size, size, 96, 96
	for z := 0; z <= high; z++ {
		res := 2 * compactMercatorEdge / (float64(size) * math.Exp2(float64(z)))
		config.Cache.LODs = append(config.Cache.LODs, compactLOD{Level: z, Scale: res * 96 / 0.0254, Resolution: res})
	}
	config.Image.Format = "PNG32"
	if cache.format == JPG {
		config.Image.Format = "JPEG"
	}
	config.Image.Quality = 90
	config.Storage.Format, config.Storage.PacketSize = "esriMapCacheStorageModeCompactV2", compactPacketSize
	lat := func(v float64) float64 {
		v = math.Max(-85.0511287798066, math.Min(85.0511287798066, v))
		return math.Log(math.Tan(math.Pi/4+v*math.Pi/360)) * 6378137
	}
	envelope := compactEnvelope{XMin: bounds[0] / 180 * compactMercatorEdge, YMin: lat(bounds[1]), XMax: bounds[2] / 180 * compactMercatorEdge, YMax: lat(bounds[3]), Spatial: config.Cache.Spatial}
	cache.metadata["format"], cache.metadata["scheme"], cache.metadata["pixel_scale"] = cache.format, "xyz", strconv.Itoa(size)
	delete(cache.metadata, "tile_path")
	values := make(map[string]interface{})
	if value := strings.TrimSpace(cache.metadata["json"]); value != "" {
		values, err = decodeTileJSONMetadata(value)
		if err != nil {
			return err
		}
	}
	// The directory reader reserves json for the extension container. Keep
	// an extension with that name inside it instead of flattening it away.
	if value, exists := values["json"]; exists {
		values["json"] = map[string]interface{}{"json": value}
	}
	for key, value := range cache.metadata {
		if key != "json" {
			values[key] = value
		}
	}
	metadata, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	if len(metadata)+1 > maxMetadataBytes {
		return fmt.Errorf("compact metadata exceeds 16 MiB")
	}
	cdi, err := xml.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	conf, err := xml.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	for _, item := range []struct {
		name string
		body []byte
	}{{"metadata.json", append(metadata, '\n')}, {"conf.cdi", append([]byte(xml.Header), cdi...)}, {"conf.xml", append([]byte(xml.Header), conf...)}} {
		path := filepath.Join(cache.root, item.name)
		if err := validateOutputFilePath(cache.root, path); err != nil {
			return err
		}
		if err := archiveWriteFileAtomic(path, item.body); err != nil {
			return err
		}
	}
	return nil
}
func (cache *compactCache) Close() error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.closed {
		return nil
	}
	cache.closed = true
	return cache.writeMetadata()
}

func archiveWalkCompact(ctx context.Context, root string, visit archiveVisit) (map[string]string, error) {
	metadata, err := compactReadMetadata(root)
	if err != nil {
		return nil, err
	}
	config, _, err := compactReadConfig(root)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, "_alllayers")
	if err := validateOutputFilePath(root, root); err != nil {
		return nil, err
	}
	if err := validateOutputFilePath(base, base); err != nil {
		return nil, err
	}
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == base && os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("compact cache must not contain symbolic links")
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".bundle") {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("compact bundle must be a regular file")
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || len(parts[0]) < 2 || parts[0][0] != 'L' {
			return fmt.Errorf("invalid compact bundle path %s", rel)
		}
		z, err := strconv.Atoi(parts[0][1:])
		if err != nil || z < 0 || z >= len(config.Cache.LODs) {
			return fmt.Errorf("compact bundle level is outside conf.xml")
		}
		matches := compactBundleName.FindStringSubmatch(parts[1])
		if matches == nil {
			return fmt.Errorf("invalid compact bundle name %s", parts[1])
		}
		row, err := strconv.ParseUint(matches[1], 16, 32)
		if err != nil {
			return err
		}
		col, err := strconv.ParseUint(matches[2], 16, 32)
		if err != nil {
			return err
		}
		if row%compactPacketSize != 0 || col%compactPacketSize != 0 {
			return fmt.Errorf("compact bundle origin must align to 128 tiles")
		}
		if err := compactCoordinate(z, uint32(col), uint32(row)); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		size, _, err := compactCheckBundle(file)
		if err != nil {
			return err
		}
		index := make([]byte, compactIndexSize)
		if _, err := file.ReadAt(index, compactHeaderSize); err != nil {
			return err
		}
		for i := 0; i < compactRecordCount; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			offset, length, err := compactDecodeRecord(file, binary.LittleEndian.Uint64(index[i*8:]), size)
			if err != nil {
				return err
			}
			if length == 0 {
				continue
			}
			x, y := uint32(col)+uint32(i%compactPacketSize), uint32(row)+uint32(i/compactPacketSize)
			if err := compactCoordinate(z, x, y); err != nil {
				return err
			}
			body := make([]byte, int(length))
			if _, err := file.ReadAt(body, int64(offset)); err != nil {
				return err
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("compact image header: %w", err)
			}
			if format == "jpeg" {
				format = JPG
			}
			if format != PNG && format != JPG {
				return fmt.Errorf("compact raster cache contains unsupported image encoding")
			}
			if metadata["format"] != "" && !sameTileEncoding(metadata["format"], format) {
				return fmt.Errorf("compact tile encoding differs from conf.xml")
			}
			if cfg.Width != config.Cache.Width || cfg.Height != config.Cache.Height {
				return fmt.Errorf("compact tile dimensions differ from conf.xml")
			}
			if err := visit(z, x, y, body, format); err != nil {
				return err
			}
		}
		return nil
	})
	return metadata, err
}
