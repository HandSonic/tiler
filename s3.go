package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
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
	endpointFingerprint string
	timeout             time.Duration
}

func newS3Output(ctx context.Context) (*s3Output, error) {
	bucket := strings.TrimSpace(viper.GetString("output.s3.bucket"))
	if bucket == "" {
		return nil, fmt.Errorf("output.s3.bucket is required")
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
		prefix:              strings.Trim(viper.GetString("output.s3.prefix"), "/"),
		endpointFingerprint: endpointFingerprint,
		timeout:             time.Duration(timeoutSeconds) * time.Second,
	}, nil
}

func (out *s3Output) save(tile Tile, format string) error {
	return out.saveContext(context.Background(), tile, format)
}

func (out *s3Output) saveContext(parent context.Context, tile Tile, format string) error {
	key := path.Join(out.prefix, fmt.Sprintf("%d/%d/%d.%s", tile.T.Z, tile.T.X, tile.T.Y, format))
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
	key := path.Join(out.prefix, fmt.Sprintf("%d/%d/%d.%s", tile.Z, tile.X, tile.Y, format))
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
	return aws.ToInt64(result.ContentLength) > 0, nil
}
