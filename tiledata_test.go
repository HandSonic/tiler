package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"strings"
	"testing"
)

type tileZeroReader struct{}

func (tileZeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestPrepareTileDataBoundsDecompressionAndImageAllocation(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.CopyN(writer, tileZeroReader{}, maxTileDataBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareTileData(compressed.Bytes(), PBF, true); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized decompressed PBF accepted: %v", err)
	}
	body := tileDataPNG(t)
	binary.BigEndian.PutUint32(body[16:20], 8192)
	binary.BigEndian.PutUint32(body[20:24], 8192)
	binary.BigEndian.PutUint32(body[29:33], crc32.ChecksumIEEE(body[12:29]))
	if _, err := prepareTileData(body, PNG, false); err == nil || !strings.Contains(err.Error(), "16 megapixel") {
		t.Fatalf("oversized image was not rejected from its header: %v", err)
	}
}

func TestPrepareTileDataNestedPBFAndDisguisedRaster(t *testing.T) {
	raw := []byte("vector tile")
	nested := tileDataGzip(t, tileDataGzip(t, raw))
	got, err := prepareTileData(nested, PBF, false)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("nested gzip was not normalized: %v", err)
	}
	for _, body := range [][]byte{tileDataPNG(t), tileDataGzip(t, tileDataGzip(t, tileDataPNG(t)))} {
		if _, err := prepareTileData(body, PBF, true); err == nil {
			t.Fatal("accepted a raster image disguised as PBF")
		}
	}
	for i := 0; i < 5; i++ {
		raw = tileDataGzip(t, raw)
	}
	if _, err := prepareTileData(raw, PBF, true); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("unbounded gzip nesting accepted: %v", err)
	}
}

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
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Name = "upstream.pbf"
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		body     []byte
		gzip     bool
		preserve bool
	}{
		{"raw to raw", raw, false, false},
		{"raw to gzip", raw, true, false},
		{"gzip to raw", compressed.Bytes(), false, false},
		{"gzip stays single layer", compressed.Bytes(), true, true},
		{"nested gzip becomes single layer", tileDataGzip(t, compressed.Bytes()), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prepareTileData(tt.body, "pbf", tt.gzip)
			if err != nil {
				t.Fatal(err)
			}
			if tt.preserve && !bytes.Equal(got, tt.body) {
				t.Fatal("already-gzipped tile was recompressed")
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
	corrupt := append([]byte(nil), compressed.Bytes()...)
	// The eight-byte gzip trailer contains CRC32 followed by the original size.
	corrupt[len(corrupt)-8] ^= 0xff
	if _, err := prepareTileData(corrupt, PBF, true); err == nil {
		t.Fatal("accepted an already-gzipped tile with a corrupt checksum")
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
	body := tileDataWebP(t)
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

func TestWebPAuditTranscodesAndChecksActualFrameDimensions(t *testing.T) {
	body := tileDataWebP(t)
	for _, format := range []string{PNG, JPG} {
		converted, err := prepareTileData(body, format, false)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(bytes.NewReader(converted))
		if err != nil || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
			t.Fatalf("WebP transcode failed: %v", err)
		}
	}
	// VP8X advertises 1x1 while the inner frame advertises 8192x8192. A
	// decoder's canvas-only DecodeConfig must not authorize that allocation.
	payload := append([]byte(nil), body[20:35]...)
	bits := uint32(8191) | uint32(8191)<<14
	binary.LittleEndian.PutUint32(payload[1:5], bits)
	var fake bytes.Buffer
	fake.WriteString("RIFF")
	fake.Write(make([]byte, 4))
	fake.WriteString("WEBPVP8X")
	binary.Write(&fake, binary.LittleEndian, uint32(10))
	fake.Write(make([]byte, 10))
	fake.WriteString("VP8L")
	binary.Write(&fake, binary.LittleEndian, uint32(len(payload)))
	fake.Write(payload)
	fake.WriteByte(0)
	for _, format := range []string{WEBP, PNG, JPG} {
		oversized := append([]byte(nil), fake.Bytes()...)
		binary.LittleEndian.PutUint32(oversized[4:8], uint32(len(oversized)-8))
		if _, err := prepareTileData(oversized, format, false); err == nil || !strings.Contains(err.Error(), "16 megapixel") {
			t.Fatalf("actual frame size bypassed limit (%s): %v", format, err)
		}
	}
}

func TestWebPAuditAcceptsLossyAlphaAndExtendedLossless(t *testing.T) {
	// Exercise both actual frame codecs, including the extended container's
	// separate alpha plane for VP8 and the alpha carried inside VP8L itself.
	lossless := tileDataWebP(t)
	extended := make([]byte, 0, len(lossless)+18)
	extended = append(extended, lossless[:12]...)
	extended = append(extended, 'V', 'P', '8', 'X', 10, 0, 0, 0, 0x10, 0, 0, 0, 1, 0, 0, 1, 0, 0)
	extended = append(extended, lossless[12:]...)
	binary.LittleEndian.PutUint32(extended[4:8], uint32(len(extended)-8))
	for _, encoded := range []string{
		"UklGRjwAAABXRUJQVlA4IDAAAADwAQCdASoCAAIAAUAmJaACdLoB+AAETAAA/upl//3Bx7Bx7Bx+yb/8/M7txfzmAAA=",
		"UklGRlwAAABXRUJQVlA4WAoAAAAQAAAAAQAAAQAAQUxQSAUAAAAAgICAAABWUDggMAAAAPABAJ0BKgIAAgABQCYloAJ0ugH4AARMAAD+6mX//cHHsHHsHH7Jv/z8zu3F/OYAAA==",
		base64.StdEncoding.EncodeToString(extended),
	} {
		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range []string{WEBP, PNG, JPG} {
			got, err := prepareTileData(body, format, false)
			if err != nil {
				t.Fatalf("valid WebP variant rejected (%s): %v", format, err)
			}
			if format == WEBP && !bytes.Equal(got, body) {
				t.Fatal("WebP preservation changed its source bytes")
			}
			if format != WEBP {
				img, _, err := image.Decode(bytes.NewReader(got))
				if err != nil || img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
					t.Fatalf("WebP variant transcode failed (%s): %v", format, err)
				}
			}
		}
	}
}

func tileDataWebP(t *testing.T) []byte {
	t.Helper()
	// A real 2x2 lossless WebP image with alpha, produced by libwebp.
	body, err := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvAUAAEAcQ/Y8CBiKi/wEA")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestWebPAuditRejectsUndecodablePayloads(t *testing.T) {
	for _, length := range []int{20, 24} {
		body := make([]byte, length)
		copy(body, "RIFF")
		binary.LittleEndian.PutUint32(body[4:8], uint32(len(body)-8))
		copy(body[8:], "WEBPVP8 ")
		binary.LittleEndian.PutUint32(body[16:20], uint32(len(body)-20))
		if _, err := prepareTileData(body, WEBP, false); err == nil {
			t.Fatalf("accepted fake %d-byte WebP tile", length)
		}
	}
	truncated := tileDataWebP(t)[:25]
	binary.LittleEndian.PutUint32(truncated[4:8], uint32(len(truncated)-8))
	if _, err := prepareTileData(truncated, WEBP, false); err == nil {
		t.Fatal("accepted truncated VP8L tile")
	}
}

func TestPBFAuditRejectsTextErrorDocuments(t *testing.T) {
	for _, body := range [][]byte{[]byte("<html><body>Access Denied</body></html>"), []byte(`{"error":"rate limited"}`), []byte("<?xml version=\"1.0\"?><Error>Forbidden</Error>")} {
		for _, packed := range [][]byte{body, tileDataGzip(t, body)} {
			if _, err := prepareTileData(packed, PBF, true); err == nil {
				t.Fatalf("accepted error document as PBF: %q", body)
			}
		}
	}
}

func TestPBFAuditEncodingCannotExceedReadLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("64 MiB encoding boundary")
	}
	body := make([]byte, maxTileDataBytes)
	rand.New(rand.NewSource(1)).Read(body)
	if _, err := prepareTileData(body, PBF, true); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("encoded gzip larger than the supported read limit was accepted: %v", err)
	}
}
