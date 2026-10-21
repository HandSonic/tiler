package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"
)

func tileDataPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tileDataJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tileDataGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareTileDataImageFormat(t *testing.T) {
	pngBody := tileDataPNG(t)
	jpegBody := tileDataJPEG(t)
	tests := []struct {
		name, format, want string
		body               []byte
		unchanged          bool
	}{
		{"PNG preserved", "png", "png", pngBody, true},
		{"JPEG preserved", "jpg", "jpeg", jpegBody, true},
		{"JPEG alias preserved", "jpeg", "jpeg", jpegBody, true},
		{"PNG converted to JPEG", "jpg", "jpeg", pngBody, false},
		{"JPEG converted to PNG", "png", "png", jpegBody, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prepareTileData(tt.body, tt.format, false)
			if err != nil {
				t.Fatal(err)
			}
			img, gotFormat, err := image.Decode(bytes.NewReader(got))
			if err != nil {
				t.Fatalf("decode saved tile: %v", err)
			}
			if gotFormat != tt.want {
				t.Errorf("saved format = %q; want %q", gotFormat, tt.want)
			}
			if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
				t.Errorf("saved dimensions = %v; want 2x2", img.Bounds())
			}
			if tt.unchanged && !bytes.Equal(got, tt.body) {
				t.Error("matching source format should retain its original bytes")
			}
		})
	}
}

func TestPrepareTileDataJPEGUsesWhiteBackground(t *testing.T) {
	transparent := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, transparent); err != nil {
		t.Fatal(err)
	}
	body, err := prepareTileData(buf.Bytes(), "jpg", false)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(4, 4).RGBA()
	if r < 65000 || g < 65000 || b < 65000 {
		t.Errorf("transparent pixels converted to RGB(%d,%d,%d); want white", r, g, b)
	}
}

func TestPrepareTileDataPBFCompression(t *testing.T) {
	raw := []byte{0x1a, 0x05, 0x0a, 0x03, 'm', 'a', 'p'}
	tests := []struct {
		name string
		body []byte
		gzip bool
	}{
		{"raw to raw", raw, false},
		{"raw to gzip", raw, true},
		{"gzip to raw", tileDataGzip(t, raw), false},
		{"gzip stays single layer", tileDataGzip(t, raw), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prepareTileData(tt.body, "pbf", tt.gzip)
			if err != nil {
				t.Fatal(err)
			}
			if tt.gzip {
				zr, err := gzip.NewReader(bytes.NewReader(got))
				if err != nil {
					t.Fatalf("saved tile is not gzip: %v", err)
				}
				got, err = io.ReadAll(zr)
				zr.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, raw) {
				t.Errorf("saved PBF = %v; want original protobuf %v", got, raw)
			}
		})
	}
}

func TestPrepareTileDataRejectsInvalidData(t *testing.T) {
	corruptGzip := tileDataGzip(t, []byte("pbf"))
	corruptGzip[len(corruptGzip)-8] ^= 0xff
	for _, tt := range []struct {
		name, format string
		body         []byte
	}{
		{"empty", "png", nil},
		{"HTML error page", "jpg", []byte("<html>Access Denied</html>")},
		{"truncated PNG", "png", tileDataPNG(t)[:20]},
		{"truncated JPEG", "jpg", tileDataJPEG(t)[:20]},
		{"invalid gzip header", "pbf", []byte{0x1f, 0x8b}},
		{"corrupt gzip checksum", "pbf", corruptGzip},
		{"empty compressed PBF", "pbf", tileDataGzip(t, nil)},
		{"PNG cannot be saved as WebP", "webp", tileDataPNG(t)},
		{"unsupported format", "unknown", []byte("tile")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := prepareTileData(tt.body, tt.format, false); err == nil {
				t.Error("invalid tile should return an error")
			}
		})
	}
}

func TestPrepareTileDataWebPPreservation(t *testing.T) {
	// Container checks deliberately do not claim to decode WebP pixel data.
	body := make([]byte, 24)
	copy(body, "RIFF")
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(body)-8))
	copy(body[8:], "WEBPVP8 ")
	binary.LittleEndian.PutUint32(body[16:20], 4)
	got, err := prepareTileData(body, "webp", false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("WebP container bytes were changed")
	}
	body[4]++
	if _, err := prepareTileData(body, "webp", false); err == nil {
		t.Error("inconsistent WebP length should return an error")
	}
}
