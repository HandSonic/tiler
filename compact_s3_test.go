package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestCompactS3RoundTripAppendAndUpload(t *testing.T) {
	ctx := context.Background()
	store, out := archiveS3Store(t) // Two entries per page, including metadata.
	root := t.TempDir()
	makePNG := func(c color.Color) []byte {
		picture := image.NewRGBA(image.Rect(0, 0, 256, 256))
		draw.Draw(picture, picture.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, picture); err != nil {
			t.Fatal(err)
		}
		return encoded.Bytes()
	}
	red, blue, green := makePNG(color.RGBA{R: 255, A: 255}), makePNG(color.RGBA{B: 255, A: 255}), makePNG(color.RGBA{G: 255, A: 255})
	store.put("source/tiles/8/127/129.png", red)
	store.put("source/tiles/8/128/130.png", blue)
	const destination = "s3://destination/cache/"
	if result, err := ConvertArchive(ctx, "s3://source/tiles/", destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2"}); err != nil || result.Tiles != 2 {
		t.Fatalf("S3 loose tiles to Compact: result=%+v err=%v", result, err)
	}
	if len(store.get("destination/cache/conf.xml")) == 0 || len(store.get("destination/cache/_alllayers/L08/R0080C0000.bundle")) == 0 || len(store.get("destination/cache/_alllayers/L08/R0080C0080.bundle")) == 0 {
		t.Fatal("S3 cache metadata or bundles are missing")
	}
	pmtiles := filepath.Join(root, "roundtrip.pmtiles")
	if _, err := ConvertArchive(ctx, destination, pmtiles, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	if got := archiveV2Snapshot(t, pmtiles, ""); len(got) != 2 || !bytes.Equal(got["8/127/129"], red) || !bytes.Equal(got["8/128/130"], blue) {
		t.Fatal("paginated Compact to PMTiles conversion changed tiles")
	}
	regionB := filepath.Join(root, "region-b")
	archiveTestWrite(t, filepath.Join(regionB, "8", "127", "129.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "8", "129", "131.png"), green)
	if result, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || result.Tiles != 3 {
		t.Fatalf("S3 Compact append: result=%+v err=%v", result, err)
	}
	// A sibling whose name starts with the source name is not nested in it.
	const sibling = "s3://destination/cached/"
	if _, err := ConvertArchive(ctx, destination, sibling, ArchiveConvertOptions{S3: out, OutputKind: "compactv2"}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "compact")
	if _, err := ConvertArchive(ctx, sibling, local, ArchiveConvertOptions{S3: out, OutputKind: "compactv2"}); err != nil {
		t.Fatal(err)
	}
	if got := archiveV2Snapshot(t, local, ""); len(got) != 3 || !bytes.Equal(got["8/127/129"], red) || !bytes.Equal(got["8/128/130"], blue) || !bytes.Equal(got["8/129/131"], green) {
		t.Fatal("S3 Compact append failed to preserve old tiles and add the new region")
	}

	// The upload command accepts an explicit cache root and ignores unrelated
	// JSON/XML and loose images; cache config is uploaded after every bundle.
	archiveTestWrite(t, filepath.Join(local, "private.json"), []byte(`{"private":true}`))
	archiveTestWrite(t, filepath.Join(local, "notes.xml"), []byte("<notes/>"))
	archiveTestWrite(t, filepath.Join(local, "8", "0", "0.png"), red)
	upload := *out
	upload.bucket, upload.prefix = "destination", "uploaded"
	result, err := UploadArchives(ctx, &upload, []string{local}, ArchiveUploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Uploaded != 5 || result.Objects[len(result.Objects)-1].Key != "uploaded/conf.xml" || len(store.get("destination/uploaded/private.json")) != 0 || len(store.get("destination/uploaded/notes.xml")) != 0 || len(store.get("destination/uploaded/8/0/0.png")) != 0 {
		t.Fatalf("Compact upload included unrelated files or wrong order: %+v", result)
	}

	puts := store.count("PUT")
	for _, overlapping := range []string{"s3://destination/cache/copy/", "s3://destination/"} {
		if _, err := ConvertArchive(ctx, destination, overlapping, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Fatalf("overlapping S3 cache prefixes must be rejected: %s: %v", overlapping, err)
		}
	}
	if _, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Overwrite: true}); err == nil || !strings.Contains(err.Error(), "new prefix") {
		t.Fatalf("existing Compact S3 overwrite must be rejected: %v", err)
	}
	if _, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "file", Append: true}); err == nil {
		t.Fatal("loose tile output accepted an existing Compact prefix")
	}
	if _, err := ConvertArchive(ctx, destination, strings.TrimSuffix(destination, "/"), ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err == nil {
		t.Fatal("conversion accepted the same S3 input and output prefix")
	}
	if store.count("PUT") != puts {
		t.Fatal("rejected conversion changed the remote cache")
	}
	store.put("destination/cache/_alllayers/../escape.bundle", []byte("invalid"))
	invalid := filepath.Join(root, "invalid.mbtiles")
	if _, err := ConvertArchive(ctx, destination, invalid, ArchiveConvertOptions{S3: out}); err == nil {
		t.Fatal("S3 Compact import accepted a traversal path")
	}
	if _, err := os.Stat(invalid); !os.IsNotExist(err) {
		t.Fatal("invalid cache published an output")
	}

	// A direct loose-tile download cannot resume a Compact dataset: those
	// new keys would be ignored when the prefix is read as Compact later.
	configureRegressionOutput(t)
	viper.Set("output.format", "s3")
	viper.Set("output.append", true)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(green)
	}))
	defer source.Close()
	target := *out
	target.bucket, target.prefix = "destination", "cache"
	task := newRegressionTask(t, source.URL)
	task.s3 = &target
	if err := task.Download(); err == nil || !strings.Contains(err.Error(), "Compact") {
		t.Fatalf("direct S3 append accepted a Compact prefix: %v", err)
	}
	if store.count("PUT") != puts {
		t.Fatal("rejected direct S3 append changed the cache prefix")
	}
}
