package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"

	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
	"golang.org/x/image/webp"
)

// Check both the container canvas and the actual frame header before any
// decoder can allocate pixels. webp.DecodeConfig returns immediately at VP8X,
// so checking that alone would let a small canvas conceal a huge frame.
func webPTileFrame(body []byte) (image.Config, []byte, bool, error) {
	if len(body) < 20 || string(body[:4]) != "RIFF" || string(body[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(body[4:8]))+8 != uint64(len(body)) {
		return image.Config{}, nil, false, fmt.Errorf("invalid WebP RIFF container")
	}
	var canvas, frame image.Config
	var payload []byte
	var lossless, hasCanvas, hasFrame, hasAlpha bool
	for offset := 12; offset < len(body); {
		if len(body)-offset < 8 {
			return image.Config{}, nil, false, fmt.Errorf("truncated WebP chunk header")
		}
		kind := string(body[offset : offset+4])
		length := binary.LittleEndian.Uint32(body[offset+4 : offset+8])
		end := uint64(offset) + 8 + uint64(length)
		if end > uint64(len(body)) || end+uint64(length&1) > uint64(len(body)) {
			return image.Config{}, nil, false, fmt.Errorf("truncated WebP chunk payload")
		}
		chunk := body[offset+8 : int(end)]
		switch kind {
		case "VP8X":
			if offset != 12 || hasCanvas || hasFrame || len(chunk) != 10 || chunk[0]&0xc1 != 0 || chunk[1] != 0 || chunk[2] != 0 || chunk[3] != 0 {
				return image.Config{}, nil, false, fmt.Errorf("invalid WebP extended header")
			}
			if chunk[0]&2 != 0 {
				return image.Config{}, nil, false, fmt.Errorf("animated WebP tiles are not supported")
			}
			canvas.Width = 1 + int(chunk[4]) + int(chunk[5])<<8 + int(chunk[6])<<16
			canvas.Height = 1 + int(chunk[7]) + int(chunk[8])<<8 + int(chunk[9])<<16
			hasCanvas = true
			if uint64(canvas.Width)*uint64(canvas.Height) > maxTilePixels {
				return image.Config{}, nil, false, fmt.Errorf("image tile exceeds the 16 megapixel limit")
			}
		case "ALPH":
			if !hasCanvas || hasFrame || hasAlpha || len(chunk) == 0 {
				return image.Config{}, nil, false, fmt.Errorf("invalid WebP alpha chunk")
			}
			hasAlpha = true
		case "VP8 ", "VP8L":
			if hasFrame {
				return image.Config{}, nil, false, fmt.Errorf("multiple WebP image frames")
			}
			var err error
			if kind == "VP8L" {
				if hasAlpha {
					return image.Config{}, nil, false, fmt.Errorf("invalid separate alpha for lossless WebP")
				}
				frame, err = vp8l.DecodeConfig(bytes.NewReader(chunk))
				lossless = true
			} else {
				decoder := vp8.NewDecoder()
				decoder.Init(bytes.NewReader(chunk), len(chunk))
				header, headerErr := decoder.DecodeFrameHeader()
				err = headerErr
				if !header.KeyFrame && err == nil {
					err = fmt.Errorf("WebP frame is not a keyframe")
				}
				frame = image.Config{ColorModel: color.YCbCrModel, Width: header.Width, Height: header.Height}
			}
			if err != nil {
				return image.Config{}, nil, false, fmt.Errorf("decode WebP frame header: %w", err)
			}
			if frame.Width <= 0 || frame.Height <= 0 || uint64(frame.Width)*uint64(frame.Height) > maxTilePixels {
				return image.Config{}, nil, false, fmt.Errorf("image tile exceeds the 16 megapixel limit")
			}
			if hasCanvas && (canvas.Width != frame.Width || canvas.Height != frame.Height) {
				return image.Config{}, nil, false, fmt.Errorf("WebP frame dimensions do not match its canvas")
			}
			hasFrame, payload = true, chunk
		}
		offset = int(end + uint64(length&1))
	}
	if !hasFrame {
		return image.Config{}, nil, false, fmt.Errorf("WebP container has no image frame")
	}
	return frame, payload, lossless, nil
}

func decodeWebPTile(body []byte) (image.Image, error) {
	_, payload, lossless, err := webPTileFrame(body)
	if err != nil {
		return nil, err
	}
	// VP8L carries its own alpha. Decoding its frame also handles extended
	// lossless containers whose alpha hint older webp.Decode versions reject.
	if lossless {
		return vp8l.Decode(bytes.NewReader(payload))
	}
	return webp.Decode(bytes.NewReader(body))
}
