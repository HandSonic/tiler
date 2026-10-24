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
	store.put("source/tiles/7/64/65.png", blue) // Unrelated old bundle.
	const destination = "s3://destination/cache/"
	// The first transfer must also be retryable after some bundles arrive.
	// Fail the second bundle, leaving the completed L07 bundle in place.
	firstFailure := "PUT destination/cache/_alllayers/L08/R0080C0000.bundle"
	store.fail(firstFailure, http.StatusForbidden)
	if _, err := ConvertArchive(ctx, "s3://source/tiles/", destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2"}); err == nil {
		t.Fatal("initial conversion ignored a failed bundle upload")
	}
	for _, name := range []string{"metadata.json", "conf.cdi", "conf.xml", "_alllayers/L07/R0000C0000.bundle"} {
		if len(store.get("destination/cache/"+name)) == 0 {
			t.Fatalf("initial transfer cannot be resumed: missing %s", name)
		}
	}
	store.fail(firstFailure, 0)
	beforeFirstRetry := store.totalRequests()
	if result, err := ConvertArchive(ctx, "s3://source/tiles/", destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || !result.Incremental || result.Tiles != 2 {
		t.Fatalf("resume first S3 Compact transfer: result=%+v err=%v", result, err)
	}
	store.mu.Lock()
	firstRetryRequests := append([]string(nil), store.requests[beforeFirstRetry:]...)
	store.mu.Unlock()
	for _, request := range firstRetryRequests {
		if request == "PUT destination/cache/_alllayers/L07/R0000C0000.bundle" {
			t.Fatal("first transfer retry reuploaded a completed bundle")
		}
	}
	if len(store.get("destination/cache/conf.xml")) == 0 || len(store.get("destination/cache/_alllayers/L08/R0080C0000.bundle")) == 0 || len(store.get("destination/cache/_alllayers/L08/R0080C0080.bundle")) == 0 {
		t.Fatal("S3 cache metadata or bundles are missing")
	}
	pmtiles := filepath.Join(root, "roundtrip.pmtiles")
	if _, err := ConvertArchive(ctx, destination, pmtiles, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatal(err)
	}
	if got := archiveV2Snapshot(t, pmtiles, ""); len(got) != 3 || !bytes.Equal(got["8/127/129"], red) || !bytes.Equal(got["8/128/130"], blue) || !bytes.Equal(got["7/64/65"], blue) {
		t.Fatal("paginated Compact to PMTiles conversion changed tiles")
	}
	regionB := filepath.Join(root, "region-b")
	archiveTestWrite(t, filepath.Join(regionB, "8", "127", "129.png"), green)
	archiveTestWrite(t, filepath.Join(regionB, "8", "129", "131.png"), green)
	// A late listing page can contain a noncanonical alias even after valid
	// bundles were found. Reject it before changing any remote object.
	for _, aliasKey := range []string{"destination/cache/_alllayers/L8/R0080C0080.bundle", "destination/cache/other.bundle", "destination/cache/_alllayers/L08/R0080C0080.BUNDLE"} {
		store.put(aliasKey, store.get("destination/cache/_alllayers/L08/R0080C0080.bundle"))
		putsBeforeAlias := store.count("PUT")
		if _, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("S3 append accepted an external bundle path alias %s: %v", aliasKey, err)
		}
		if _, err := ConvertArchive(ctx, destination, filepath.Join(root, "alias-input.mbtiles"), ArchiveConvertOptions{S3: out}); err == nil {
			t.Fatalf("S3 conversion silently omitted an invalid bundle path %s", aliasKey)
		}
		if store.count("PUT") != putsBeforeAlias {
			t.Fatal("rejected S3 append modified remote objects")
		}
		store.mu.Lock()
		delete(store.objects, aliasKey)
		store.mu.Unlock()
	}
	beforeAppend := store.totalRequests()
	if result, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || !result.Incremental || result.Tiles != 1 {
		t.Fatalf("S3 Compact append: result=%+v err=%v", result, err)
	}
	store.mu.Lock()
	requests := append([]string(nil), store.requests[beforeAppend:]...)
	store.mu.Unlock()
	var writtenBundles []string
	for _, request := range requests {
		if strings.HasSuffix(request, "cache/_alllayers/L07/R0000C0000.bundle") {
			t.Fatalf("incremental append accessed an unrelated old bundle: %s", request)
		}
		if strings.HasPrefix(request, "PUT ") && strings.HasSuffix(request, ".bundle") {
			writtenBundles = append(writtenBundles, request)
		}
	}
	if len(writtenBundles) != 1 || writtenBundles[0] != "PUT destination/cache/_alllayers/L08/R0080C0080.bundle" {
		t.Fatalf("append must upload only the changed bundle: %v", writtenBundles)
	}
	beforeRetry := store.totalRequests()
	if result, err := ConvertArchive(ctx, regionB, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || !result.Incremental || result.Tiles != 0 {
		t.Fatalf("S3 Compact repeated append: result=%+v err=%v", result, err)
	}
	store.mu.Lock()
	requests = append([]string(nil), store.requests[beforeRetry:]...)
	store.mu.Unlock()
	for _, request := range requests {
		if strings.HasPrefix(request, "PUT ") && strings.HasSuffix(request, ".bundle") {
			t.Fatalf("repeated append uploaded an unchanged bundle: %s", request)
		}
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
	if got := archiveV2Snapshot(t, local, ""); len(got) != 4 || !bytes.Equal(got["8/127/129"], red) || !bytes.Equal(got["8/128/130"], blue) || !bytes.Equal(got["8/129/131"], green) || !bytes.Equal(got["7/64/65"], blue) {
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
	if result.Uploaded != 6 || result.Objects[len(result.Objects)-1].Key != "uploaded/conf.xml" || len(store.get("destination/uploaded/private.json")) != 0 || len(store.get("destination/uploaded/notes.xml")) != 0 || len(store.get("destination/uploaded/8/0/0.png")) != 0 {
		t.Fatalf("Compact upload included unrelated files or wrong order: %+v", result)
	}

	// Fail the second bundle at a new zoom. Metadata must already include
	// that zoom, so the partially appended cache remains readable and retryable.
	regionC := filepath.Join(root, "region-c")
	archiveTestWrite(t, filepath.Join(regionC, "9", "127", "129.png"), green)
	archiveTestWrite(t, filepath.Join(regionC, "9", "128", "130.png"), green)
	failedUpload := "PUT destination/cache/_alllayers/L09/R0080C0080.bundle"
	store.fail(failedUpload, http.StatusForbidden)
	if _, err := ConvertArchive(ctx, regionC, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err == nil {
		t.Fatal("append ignored a failed bundle upload")
	}
	partial := filepath.Join(root, "partial.mbtiles")
	if _, err := ConvertArchive(ctx, destination, partial, ArchiveConvertOptions{S3: out}); err != nil {
		t.Fatalf("interrupted append left the cache unreadable: %v", err)
	}
	if got := archiveV2Snapshot(t, partial, ""); len(got) != 5 || !bytes.Equal(got["8/127/129"], red) || !bytes.Equal(got["9/127/129"], green) {
		t.Fatal("interrupted append lost old tiles or the completed bundle")
	}
	store.fail(failedUpload, 0)
	if result, err := ConvertArchive(ctx, regionC, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || result.Tiles != 1 {
		t.Fatalf("retry must add only the missing bundle: result=%+v err=%v", result, err)
	}

	// S3 clients can create directory markers before the first actual tile.
	// More markers than one listing page must not lock an empty cache to 256px.
	for _, name := range []string{"metadata.json", "conf.cdi", "conf.xml"} {
		store.put("destination/initialized/"+name, store.get("destination/cache/"+name))
	}
	for _, name := range []string{"_alllayers/", "_alllayers/L00/", "_alllayers/L08/"} {
		store.put("destination/initialized/"+name, nil)
		store.put("destination/cache/"+name, nil)
	}
	store.put("destination/initialized/_alllayers/L00/errors.json", []byte(`{"failures":[]}`))
	var large bytes.Buffer
	if err := png.Encode(&large, image.NewRGBA(image.Rect(0, 0, 512, 512))); err != nil {
		t.Fatal(err)
	}
	largeRegion := filepath.Join(root, "large-region")
	archiveTestWrite(t, filepath.Join(largeRegion, "8", "130", "132.png"), large.Bytes())
	if _, err := ConvertArchive(ctx, largeRegion, destination, ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("directory markers hid established tile dimensions: %v", err)
	}
	if result, err := ConvertArchive(ctx, largeRegion, "s3://destination/initialized/", ArchiveConvertOptions{S3: out, OutputKind: "compactv2", Append: true}); err != nil || result.Tiles != 1 {
		t.Fatalf("first tile could not establish dimensions through directory markers: result=%+v err=%v", result, err)
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
