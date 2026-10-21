package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
)

const maxTileDataBytes = 64 << 20
const maxTilePixels = 16 << 20

func readTileData(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxTileDataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxTileDataBytes {
		return nil, fmt.Errorf("tile data exceeds the 64 MiB limit")
	}
	return body, nil
}

// prepareTileData makes the stored bytes agree with the configured extension.
// PBF sources may already be gzipped; always decode that layer before deciding
// which representation the destination should receive.
func prepareTileData(body []byte, format string, gzipPBF bool) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty tile data")
	}
	if len(body) > maxTileDataBytes {
		return nil, fmt.Errorf("tile data exceeds the 64 MiB limit")
	}
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "pbf":
		for depth := 0; len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b; depth++ {
			if depth >= 4 {
				return nil, fmt.Errorf("PBF gzip nesting exceeds four layers")
			}
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("decode gzipped PBF tile: %w", err)
			}
			data, err := readTileData(zr)
			closeErr := zr.Close()
			if err != nil {
				return nil, fmt.Errorf("read gzipped PBF tile: %w", err)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close gzipped PBF tile: %w", closeErr)
			}
			body = data
			if len(body) == 0 {
				return nil, fmt.Errorf("empty PBF tile after decompression")
			}
		}
		// Check once after all gzip layers, for downloads and conversions alike.
		// Retain the short WebP signature check for truncated RIFF wrappers.
		if strings.HasPrefix(http.DetectContentType(body), "image/") || (len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP") {
			return nil, fmt.Errorf("raster image data cannot be stored as PBF vector tiles")
		}
		text := bytes.TrimSpace(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}))
		probe := text
		if len(probe) > 512 {
			probe = probe[:512]
		}
		lower := strings.ToLower(string(probe))
		if json.Valid(text) || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<?xml") || strings.HasPrefix(lower, "<error") {
			return nil, fmt.Errorf("HTML, XML or JSON response cannot be stored as a PBF vector tile")
		}
		if !gzipPBF {
			return body, nil
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			return nil, fmt.Errorf("compress PBF tile: %w", err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("finish compressing PBF tile: %w", err)
		}
		if buf.Len() > maxTileDataBytes {
			return nil, fmt.Errorf("encoded tile data exceeds the 64 MiB limit")
		}
		return buf.Bytes(), nil
	case "webp":
		if _, err := decodeWebPTile(body); err != nil {
			return nil, fmt.Errorf("decode WebP tile (encoding other images as WebP is not supported): %w", err)
		}
		return body, nil
	case "png", "jpg", "jpeg":
		config, configFormat, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode image tile header: %w", err)
		}
		if config.Width <= 0 || config.Height <= 0 || uint64(config.Width)*uint64(config.Height) > maxTilePixels {
			return nil, fmt.Errorf("image tile exceeds the 16 megapixel limit")
		}
		var img image.Image
		sourceFormat := configFormat
		if configFormat == "webp" {
			img, err = decodeWebPTile(body)
		} else {
			img, sourceFormat, err = image.Decode(bytes.NewReader(body))
		}
		if err != nil {
			return nil, fmt.Errorf("decode image tile: %w", err)
		}
		if format == "jpg" {
			format = "jpeg"
		}
		if sourceFormat == format {
			return body, nil
		}
		var buf bytes.Buffer
		if format == "png" {
			err = png.Encode(&buf, img)
		} else {
			// JPEG has no alpha channel. Composite transparent map labels on
			// white so conversion does not turn their background black.
			opaque := image.NewRGBA(img.Bounds())
			draw.Draw(opaque, opaque.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
			draw.Draw(opaque, opaque.Bounds(), img, img.Bounds().Min, draw.Over)
			err = jpeg.Encode(&buf, opaque, &jpeg.Options{Quality: 90})
		}
		if err != nil {
			return nil, fmt.Errorf("encode %s tile: %w", format, err)
		}
		if buf.Len() > maxTileDataBytes {
			return nil, fmt.Errorf("encoded tile data exceeds the 64 MiB limit")
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported tile format %q", format)
	}
}
