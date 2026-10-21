package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

func configureS3Test(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_S3", "")
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	viper.Set("output.directory", t.TempDir())
	viper.Set("output.s3.bucket", "test-bucket")
	viper.Set("output.s3.force_path_style", true)
}

func TestS3Save(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("vector tile")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		format      string
		prefix      string
		body        []byte
		contentType string
		encoding    string
		key         string
	}{
		{"png", PNG, "maps", []byte("png tile"), "image/png", "", "maps/2/3/1.png"},
		{"jpg", JPG, "maps", []byte("jpg tile"), "image/jpeg", "", "maps/2/3/1.jpg"},
		{"pbf", PBF, "maps", compressed.Bytes(), "application/x-protobuf", "gzip", "maps/2/3/1.pbf"},
		{"empty prefix", PNG, "", []byte("tile"), "image/png", "", "2/3/1.png"},
		{"root prefix", PNG, "/", []byte("tile"), "image/png", "", "2/3/1.png"},
		{"prefix slashes", PNG, "/maps/imagery/", []byte("tile"), "image/png", "", "maps/imagery/2/3/1.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureS3Test(t)
			type requestData struct {
				method, path, contentType, encoding, authorization string
				body                                               []byte
				err                                                error
			}
			requests := make(chan requestData, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				requests <- requestData{
					method: r.Method, path: r.URL.Path,
					contentType: r.Header.Get("Content-Type"), encoding: r.Header.Get("Content-Encoding"),
					authorization: r.Header.Get("Authorization"), body: body, err: err,
				}
				w.Header().Set("ETag", `"test-etag"`)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			viper.Set("output.s3.endpoint", server.URL)
			viper.Set("output.s3.prefix", tt.prefix)
			out, err := newS3Output(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := out.save(Tile{T: maptile.Tile{Z: 2, X: 3, Y: 1}, C: tt.body}, tt.format); err != nil {
				t.Fatalf("save: %v", err)
			}
			select {
			case got := <-requests:
				if got.err != nil {
					t.Fatalf("read request: %v", got.err)
				}
				if got.method != http.MethodPut || got.path != "/test-bucket/"+tt.key {
					t.Errorf("request = %s %s; want PUT /test-bucket/%s", got.method, got.path, tt.key)
				}
				if got.contentType != tt.contentType {
					t.Errorf("Content-Type = %q; want %q", got.contentType, tt.contentType)
				}
				if got.encoding != tt.encoding {
					t.Errorf("Content-Encoding = %q; want %q", got.encoding, tt.encoding)
				}
				if !bytes.Equal(got.body, tt.body) {
					t.Errorf("uploaded body differs from the original tile")
				}
				if !strings.HasPrefix(got.authorization, "AWS4-HMAC-SHA256 Credential=test-access-key/") {
					t.Error("request is missing the expected SigV4 authorization")
				}
			default:
				t.Fatal("save completed without sending an S3 request")
			}
		})
	}
}

func TestS3SaveAccessDenied(t *testing.T) {
	configureS3Test(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>test-request</RequestId></Error>`)
	}))
	defer server.Close()
	viper.Set("output.s3.endpoint", server.URL)
	out, err := newS3Output(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = out.save(Tile{T: maptile.Tile{Z: 0}, C: []byte("tile")}, PNG)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("save error = %v; want an S3 AccessDenied/403 error", err)
	}
}

func TestNewS3OutputConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*testing.T)
		wantErr bool
		timeout time.Duration
	}{
		{"defaults and environment region", nil, false, 60 * time.Second},
		{"explicit region", func(t *testing.T) {
			t.Setenv("AWS_REGION", "")
			viper.Set("output.s3.region", "ap-southeast-1")
		}, false, 60 * time.Second},
		{"explicit timeout", func(t *testing.T) { viper.Set("output.s3.timeout_seconds", 7) }, false, 7 * time.Second},
		{"empty endpoint uses AWS", func(t *testing.T) { viper.Set("output.s3.endpoint", "") }, false, 60 * time.Second},
		{"missing bucket", func(t *testing.T) { viper.Set("output.s3.bucket", "") }, true, 0},
		{"missing region", func(t *testing.T) { t.Setenv("AWS_REGION", "") }, true, 0},
		{"endpoint without scheme", func(t *testing.T) { viper.Set("output.s3.endpoint", "localhost:9000") }, true, 0},
		{"unsupported endpoint scheme", func(t *testing.T) { viper.Set("output.s3.endpoint", "ftp://localhost:9000") }, true, 0},
		{"endpoint without host", func(t *testing.T) { viper.Set("output.s3.endpoint", "https://") }, true, 0},
		{"zero timeout", func(t *testing.T) { viper.Set("output.s3.timeout_seconds", 0) }, true, 0},
		{"negative timeout", func(t *testing.T) { viper.Set("output.s3.timeout_seconds", -1) }, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureS3Test(t)
			if tt.change != nil {
				tt.change(t)
			}
			out, err := newS3Output(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected invalid configuration to fail")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.bucket != "test-bucket" || out.client == nil {
				t.Error("output was not initialized with a bucket and client")
			}
			if out.timeout != tt.timeout {
				t.Errorf("timeout = %s; want %s", out.timeout, tt.timeout)
			}
		})
	}
}
