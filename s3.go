package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/paulmach/orb/maptile"
	"github.com/spf13/viper"
)

// s3Output is shared by the download workers. Each tile is uploaded before its
// worker is released, so task.workers also bounds the number of S3 requests.
type s3Output struct {
	client              *s3.Client
	bucket              string
	prefix              string
	tilePath            string
	allowUnmarkedTiles  bool
	endpointFingerprint string
	timeout             time.Duration
}

func newS3Output(ctx context.Context) (*s3Output, error) {
	return newS3OutputForBucket(ctx, viper.GetString("output.s3.bucket"))
}

func newS3OutputForBucket(ctx context.Context, bucket string) (*s3Output, error) {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" {
		return nil, fmt.Errorf("output.s3.bucket is required")
	}
	prefix, err := normalizeS3Prefix(viper.GetString("output.s3.prefix"))
	if err != nil {
		return nil, fmt.Errorf("output.s3.prefix: %w", err)
	}
	endpoint := strings.TrimSpace(viper.GetString("output.s3.endpoint"))
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("output.s3.endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	timeoutSeconds := 60
	if viper.IsSet("output.s3.timeout_seconds") {
		timeoutSeconds = viper.GetInt("output.s3.timeout_seconds")
	}
	if timeoutSeconds <= 0 || int64(timeoutSeconds) > int64((1<<63-1)/time.Second) {
		return nil, fmt.Errorf("output.s3.timeout_seconds must be a positive duration in seconds")
	}
	opts := []func(*config.LoadOptions) error{}
	if region := strings.TrimSpace(viper.GetString("output.s3.region")); region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("S3 region is required: set output.s3.region or AWS_REGION")
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = viper.GetBool("output.s3.force_path_style")
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	// Options includes endpoint overrides from the AWS environment/profile as
	// well as output.s3.endpoint, so a repair cannot silently change services.
	endpointFingerprint, err := FailureS3EndpointFingerprint(aws.ToString(client.Options().BaseEndpoint), cfg.Region)
	if err != nil {
		return nil, err
	}
	return &s3Output{
		client:              client,
		bucket:              bucket,
		prefix:              prefix,
		tilePath:            viper.GetString("output.tile_path"),
		endpointFingerprint: endpointFingerprint,
		timeout:             time.Duration(timeoutSeconds) * time.Second,
	}, nil
}

func (out *s3Output) saveContext(parent context.Context, tile Tile, format string) error {
	key, err := out.tileKey(tile.T, format)
	if err != nil {
		return err
	}
	pattern, err := normalizeTilePath(out.tilePath)
	if err != nil {
		return err
	}
	contentType := http.DetectContentType(tile.C)
	switch strings.ToLower(format) {
	case "png":
		contentType = "image/png"
	case "jpg", "jpeg":
		contentType = "image/jpeg"
	case "webp":
		contentType = "image/webp"
	case "pbf":
		contentType = "application/x-protobuf"
	}
	input := &s3.PutObjectInput{
		Bucket:      aws.String(out.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(tile.C),
		ContentType: aws.String(contentType),
		Metadata: map[string]string{
			"tiler-path":   fmt.Sprintf("%x", sha256.Sum256([]byte(pattern))),
			"tiler-xyz":    fmt.Sprintf("%d/%d/%d", tile.T.Z, tile.T.X, tile.T.Y),
			"tiler-format": strings.ToLower(format),
		},
	}
	if strings.EqualFold(format, PBF) && len(tile.C) >= 2 && tile.C[0] == 0x1f && tile.C[1] == 0x8b {
		input.ContentEncoding = aws.String("gzip")
	}
	ctx, cancel := context.WithTimeout(parent, out.timeout)
	defer cancel()
	if _, err := out.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("upload s3://%s/%s: %w", out.bucket, key, err)
	}
	return nil
}

func (out *s3Output) exists(parent context.Context, tile maptile.Tile, format string) (bool, error) {
	key, err := out.tileKey(tile, format)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(parent, out.timeout)
	defer cancel()
	result, err := out.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(key)})
	if err != nil {
		var responseError interface{ HTTPStatusCode() int }
		if errors.As(err, &responseError) && responseError.HTTPStatusCode() == http.StatusNotFound {
			return false, nil
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
			return false, nil
		}
		return false, fmt.Errorf("check s3://%s/%s: %w", out.bucket, key, err)
	}
	if aws.ToInt64(result.ContentLength) <= 0 {
		return false, nil
	}
	if err := out.validateTileIdentity(result.Metadata, tile, format); err != nil {
		return false, err
	}
	return true, nil
}

func (out *s3Output) validateTileIdentity(metadata map[string]string, tile maptile.Tile, format string) error {
	pattern, err := normalizeTilePath(out.tilePath)
	if err != nil {
		return err
	}
	pathHash := metadata["tiler-path"]
	if pathHash != "" && pathHash != fmt.Sprintf("%x", sha256.Sum256([]byte(pattern))) {
		return fmt.Errorf("existing S3 tile uses a different tile_path; keep the original template or choose a new prefix")
	}
	if pathHash == "" && pattern != defaultTilePath && !out.allowUnmarkedTiles {
		return fmt.Errorf("cannot confirm tile_path of an unmarked S3 tile; use the original template or a new prefix")
	}
	if xyz := metadata["tiler-xyz"]; xyz != "" && xyz != fmt.Sprintf("%d/%d/%d", tile.Z, tile.X, tile.Y) {
		return fmt.Errorf("existing S3 tile coordinates do not match the requested tile")
	}
	if storedFormat := metadata["tiler-format"]; storedFormat != "" && !sameTileEncoding(storedFormat, format) {
		return fmt.Errorf("existing S3 tile format does not match the requested format")
	}
	return nil
}

func sameTileEncoding(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	a, b = strings.ToLower(a), strings.ToLower(b)
	return (a == "jpeg" || a == "jpg") && (b == "jpeg" || b == "jpg")
}

func (out *s3Output) tileKey(tile maptile.Tile, format string) (string, error) {
	prefix, err := normalizeS3Prefix(out.prefix)
	if err != nil {
		return "", err
	}
	relative, err := renderTilePath(out.tilePath, tile, format)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return relative, nil
	}
	return prefix + "/" + relative, nil
}

func normalizeS3Prefix(prefix string) (string, error) {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, '\\') {
			return "", fmt.Errorf("S3 prefix cannot contain empty segments, dot segments or backslashes")
		}
	}
	return prefix, nil
}
