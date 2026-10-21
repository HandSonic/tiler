package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
)

// prepareTileData makes the stored bytes agree with the configured extension.
// PBF sources may already be gzipped; always decode that layer before deciding
// which representation the destination should receive.
func prepareTileData(body []byte, format string, gzipPBF bool) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty tile data")
	}
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "pbf":
		if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("decode gzipped PBF tile: %w", err)
			}
			data, err := io.ReadAll(zr)
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
		return buf.Bytes(), nil
	case "webp":
		// The standard library has no WebP encoder. Preserve WebP data rather
		// than writing a different image format behind a .webp extension.
		if len(body) < 20 || !bytes.Equal(body[:4], []byte("RIFF")) || !bytes.Equal(body[8:12], []byte("WEBP")) {
			return nil, fmt.Errorf("source is not a WebP tile; WebP conversion is not supported")
		}
		if uint64(binary.LittleEndian.Uint32(body[4:8]))+8 != uint64(len(body)) {
			return nil, fmt.Errorf("invalid WebP tile length")
		}
		switch string(body[12:16]) {
		case "VP8 ", "VP8L", "VP8X":
		default:
			return nil, fmt.Errorf("invalid WebP tile chunk")
		}
		return body, nil
	case "png", "jpg", "jpeg":
		img, sourceFormat, err := image.Decode(bytes.NewReader(body))
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
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported tile format %q", format)
	}
}
