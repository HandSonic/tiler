package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmach/orb/maptile"
)

const manifestTestURL = "https://tiles.example.test/map/{z}/{x}/{y}.png?style=blue&token=old-secret"

func TestFailureSourceFingerprintKeepsLayerButAllowsCredentialRotation(t *testing.T) {
	first, err := FailureSourceFingerprint(manifestTestURL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FailureSourceFingerprint("https://tiles.example.test/map/{z}/{x}/{y}.png?access_token=new-secret&style=blue")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("rotating authentication changed the source fingerprint")
	}
	changed, err := FailureSourceFingerprint("https://tiles.example.test/map/{z}/{x}/{y}.png?style=red&token=old-secret")
	if err != nil {
		t.Fatal(err)
	}
	if first == changed {
		t.Fatal("different layers have the same source fingerprint")
	}
	for _, source := range []string{"", "not-a-url", "ftp://example.test/map", "https:///map", "https://tiles.example.test/{z}?style=road;lang=en", "https://tiles.example.test/{z}?style=%xx"} {
		if _, err := FailureSourceFingerprint(source); err == nil {
			t.Errorf("accepted invalid source %q", source)
		}
	}
	a, _ := FailureSourceFingerprint("https://t.example/{z}?T=img_w&lyrs=s&key=secret")
	b, _ := FailureSourceFingerprint("https://t.example/{z}?lyrs=s&T=img_w&key=replacement")
	c, _ := FailureSourceFingerprint("https://t.example/{z}?T=vec_w&lyrs=s&key=secret")
	if a != b || a == c {
		t.Fatal("layer query values or query ordering were handled incorrectly")
	}
}

func TestFailureManifestRejectsOversizedFiles(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "large.json")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(64<<20 + 1)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFailureManifest(filename); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized failure report was not rejected before parsing: %v", err)
	}
}

func TestFailureManifestRejectsFIFOWithoutBlocking(t *testing.T) {
	command, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	filename := filepath.Join(t.TempDir(), "failures.json")
	if err := exec.Command(command, filename).Run(); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	result := make(chan error, 1)
	go func() { _, err := LoadFailureManifest(filename); result <- err }()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Errorf("FIFO error=%v", err)
		}
	case <-time.After(200 * time.Millisecond):
		// Release the old blocking open so this repro cannot leak a goroutine.
		writer, err := os.OpenFile(filename, os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writer.Close()
		<-result
		t.Error("opening a FIFO failure report blocked waiting for a writer")
	}
}

func TestFailureManifestDeduplicatesResolvesAndRedacts(t *testing.T) {
	manifest := newFailureManifest("map", "PNG")
	tile := maptile.Tile{Z: 2, X: 1, Y: 3}
	for i := 0; i < 2; i++ {
		if err := manifest.Record(tile, manifestTestURL, fmt.Errorf("Get %s: status 503, client_secret=more-secret", manifestTestURL)); err != nil {
			t.Fatal(err)
		}
	}
	otherSource := strings.Replace(manifestTestURL, "style=blue", "style=red", 1)
	nested := fmt.Errorf("wrapped: %w", &url.Error{Op: "Get", URL: "https://user:password@tiles.example.test/secret-path?tk=very-secret", Err: errors.New("request failed")})
	if err := manifest.Record(tile, otherSource, nested); err != nil {
		t.Fatal(err)
	}
	if manifest.Len() != 2 {
		t.Fatalf("failure count=%d; want two distinct sources", manifest.Len())
	}
	encoded, err := json.Marshal(manifest.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-secret", "more-secret", "very-secret", "password", "secret-path", "token="} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("manifest exposed %q: %s", secret, encoded)
		}
	}
	current := strings.Replace(manifestTestURL, "old-secret", "rotated-secret", 1)
	if err := manifest.Resolve(tile, current); err != nil {
		t.Fatal(err)
	}
	if manifest.Len() != 1 {
		t.Fatal("successful repair did not resolve the matching source")
	}
	if err := manifest.Resolve(tile, otherSource); err != nil {
		t.Fatal(err)
	}
	if manifest.Len() != 0 {
		t.Fatal("manifest still contains resolved failures")
	}
	if err := manifest.Record(maptile.Tile{Z: 0, X: 1}, manifestTestURL, errors.New("error")); err == nil {
		t.Fatal("out of range tile accepted")
	}
	if err := manifest.Record(tile, manifestTestURL, nil); err == nil {
		t.Fatal("nil failure accepted")
	}
}

func TestFailureManifestConcurrentTracking(t *testing.T) {
	manifest := newFailureManifest("map", PNG)
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func(x uint32) {
			defer workers.Done()
			tile := maptile.Tile{Z: 6, X: x, Y: 0}
			for j := 0; j < 10; j++ {
				if err := manifest.Record(tile, manifestTestURL, errors.New("failed")); err != nil {
					t.Error(err)
				}
				_ = manifest.Snapshot()
			}
			if x%2 == 0 {
				if err := manifest.Resolve(tile, manifestTestURL); err != nil {
					t.Error(err)
				}
			}
		}(uint32(i))
	}
	workers.Wait()
	if got := manifest.Len(); got != 32 {
		t.Fatalf("unresolved=%d; want 32", got)
	}
}

func TestFailureManifestSaveLoadAndEmptyRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reports", "failures.json")
	manifest := newFailureManifest("map", PNG)
	target := FailureTarget{Kind: "file", Path: filepath.Join(dir, "tiles")}
	if err := manifest.SetTarget(target); err != nil {
		t.Fatal(err)
	}
	tile := maptile.Tile{Z: 2, X: 1, Y: 2}
	if err := manifest.Record(tile, manifestTestURL, errors.New("HTTP status 503")); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(path); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadFailureManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := saved.Target(); !ok || got != target {
		t.Fatalf("target=%+v, %v", got, ok)
	}
	if saved.Len() != 1 || saved.Snapshot().MapName != "map" || saved.Snapshot().Format != PNG {
		t.Fatal("round trip lost manifest data")
	}
	copy := saved.Snapshot()
	copy.Tiles[0].Error = "mutated"
	copy.Target.Path = "mutated"
	if saved.Snapshot().Tiles[0].Error == "mutated" || saved.Snapshot().Target.Path == "mutated" {
		t.Fatal("Snapshot exposed mutable state")
	}
	if err := saved.Resolve(tile, manifestTestURL); err != nil {
		t.Fatal(err)
	}
	if err := saved.Save(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"tiles": []`) {
		t.Fatalf("completed repair did not write an empty array: %s", after)
	}
	empty, err := LoadFailureManifest(path)
	if err != nil || empty.Len() != 0 {
		t.Fatalf("empty repair manifest=%v, %v", empty, err)
	}
}

func TestFailureManifestSaveFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "destination")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := newFailureManifest("map", PNG)
	if err := manifest.Save(path); err == nil {
		t.Fatal("replacing a directory should fail")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("existing destination was modified")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("failed save left temporary files: %v", files)
	}
}

func TestFailureManifestStrictInputValidation(t *testing.T) {
	source, _ := FailureSourceFingerprint(manifestTestURL)
	base := fmt.Sprintf(`{"version":1,"map_name":"map","format":"png","tiles":[{"z":1,"x":0,"y":1,"source":%q,"error":"failed"}]}`, source)
	tests := []struct{ name, data string }{
		{"missing version", strings.Replace(base, `"version":1,`, "", 1)},
		{"future version", strings.Replace(base, `"version":1`, `"version":2`, 1)},
		{"unknown field", strings.Replace(base, `"map_name"`, `"unknown"`, 1)},
		{"missing x", strings.Replace(base, `"x":0,`, "", 1)},
		{"negative x", strings.Replace(base, `"x":0`, `"x":-1`, 1)},
		{"fractional x", strings.Replace(base, `"x":0`, `"x":0.5`, 1)},
		{"overflow x", strings.Replace(base, `"x":0`, `"x":4294967296`, 1)},
		{"outside matrix", strings.Replace(base, `"x":0`, `"x":2`, 1)},
		{"negative zoom", strings.Replace(base, `"z":1`, `"z":-1`, 1)},
		{"large zoom", strings.Replace(base, `"z":1`, `"z":31`, 1)},
		{"bad source", strings.Replace(base, source, "not-a-fingerprint", 1)},
		{"unknown tile field", strings.Replace(base, `"error":`, `"other":`, 1)},
		{"null tiles", `{"version":1,"format":"png","tiles":null}`},
		{"missing tiles", `{"version":1,"format":"png"}`},
		{"bad format", strings.Replace(base, `"format":"png"`, `"format":"gif"`, 1)},
		{"bad target", strings.Replace(base, `"tiles":`, `"target":{"kind":"s3","bucket":""},"tiles":`, 1)},
		{"two documents", base + ` {}`},
		{"trailing garbage", base + ` garbage`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadFailureManifest(path)
			if err == nil || got != nil {
				t.Fatalf("invalid manifest accepted: %+v %v", got, err)
			}
		})
	}
}

func TestFailureManifestLoadDeduplicatesAndRepairUsesCurrentLayers(t *testing.T) {
	source, _ := FailureSourceFingerprint(manifestTestURL)
	entry := fmt.Sprintf(`{"z":2,"x":1,"y":2,"source":%q,"error":"failed"}`, source)
	data := fmt.Sprintf(`{"version":1,"format":"png","tiles":[%s,%s]}`, entry, entry)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadFailureManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Len() != 1 {
		t.Fatal("duplicate coordinates were not deduplicated")
	}
	current := strings.Replace(manifestTestURL, "old-secret", "new-secret", 1)
	plan, err := manifest.RecordsForLayers([]Layer{{Zoom: 2, URL: current}, {Zoom: 3, URL: current}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Tile != (maptile.Tile{Z: 2, X: 1, Y: 2}) || plan[0].URL != current {
		t.Fatalf("repair plan=%+v", plan)
	}
	for _, layers := range [][]Layer{
		{{Zoom: 3, URL: current}},
		{{Zoom: 2, URL: strings.Replace(current, "style=blue", "style=red", 1)}},
		{{Zoom: 2, URL: "bad-url"}},
	} {
		if _, err := manifest.RecordsForLayers(layers); err == nil {
			t.Fatal("mismatched source layers accepted")
		}
	}
}

func TestFailureTargetValidation(t *testing.T) {
	manifest := newFailureManifest("map", PNG)
	for _, target := range []FailureTarget{
		{Kind: "file"}, {Kind: "mbtiles", Path: "file.mbtiles", Bucket: "bucket"}, {Kind: "s3"}, {Kind: "s3", Bucket: "bucket", Path: "file"}, {Kind: "s3", Bucket: "bucket"}, {Kind: "s3", Bucket: "bucket", EndpointFingerprint: "invalid"}, {Kind: "unknown"},
	} {
		if err := manifest.SetTarget(target); err == nil {
			t.Fatalf("invalid target accepted: %+v", target)
		}
	}
	if err := manifest.SetTarget(FailureTarget{Kind: "file", Path: "relative-tiles"}); err != nil {
		t.Fatal(err)
	}
	target, _ := manifest.Target()
	if !filepath.IsAbs(target.Path) {
		t.Fatal("target path was not made absolute")
	}
	fingerprint, err := FailureS3EndpointFingerprint("https://minio.example.test", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.SetTarget(FailureTarget{Kind: "s3", Bucket: "bucket", EndpointFingerprint: strings.ToUpper(fingerprint)}); err != nil {
		t.Fatal(err)
	}
	target, _ = manifest.Target()
	if target.EndpointFingerprint != fingerprint {
		t.Fatal("S3 fingerprint was not normalized")
	}
}

func TestFailureS3EndpointFingerprint(t *testing.T) {
	base, err := FailureS3EndpointFingerprint("https://minio.example.test/api", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"https://MINIO.EXAMPLE.TEST/api/", " https://minio.example.test:443/api "} {
		got, err := FailureS3EndpointFingerprint(endpoint, " us-east-1 ")
		if err != nil || got != base {
			t.Fatalf("equivalent endpoint was not normalized: %s, %v", got, err)
		}
	}
	for _, test := range []struct{ endpoint, region string }{
		{"https://other.example.test/api", "us-east-1"},
		{"https://minio.example.test/api", "us-west-2"},
		{"https://minio.example.test/API", "us-east-1"},
		{"http://minio.example.test/api", "us-east-1"},
		{"", "us-east-1"},
	} {
		got, err := FailureS3EndpointFingerprint(test.endpoint, test.region)
		if err != nil || got == base {
			t.Fatalf("different endpoint or region lost its identity: %+v, %v", test, err)
		}
	}
	escaped, _ := FailureS3EndpointFingerprint("https://minio.example.test/a%2Fb", "us-east-1")
	decoded, _ := FailureS3EndpointFingerprint("https://minio.example.test/a/b", "us-east-1")
	if escaped == decoded {
		t.Fatal("escaped endpoint path separator lost its identity")
	}
	for _, endpoint := range []string{"https://user:secret@minio.example.test", "https://minio.example.test?token=secret", "https://minio.example.test#secret", "not-a-url"} {
		if _, err := FailureS3EndpointFingerprint(endpoint, "us-east-1"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe endpoint accepted or exposed in its error: %v", err)
		}
	}
	if _, err := FailureS3EndpointFingerprint("", ""); err == nil {
		t.Fatal("missing region was accepted")
	}
}
