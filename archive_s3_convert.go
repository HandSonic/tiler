package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/paulmach/orb/maptile"
)

type archiveS3Location struct {
	bucket, key string
	kind        string
}

// ConvertArchive accepts local paths and s3://bucket/key locations. S3 loose
// tiles are staged directly into SQLite, never into scattered image files.
func ConvertArchive(ctx context.Context, input, output string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var zero ArchiveConvertResult
	if ctx == nil {
		return zero, fmt.Errorf("conversion context is required")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if opts.Append && opts.Overwrite {
		return zero, fmt.Errorf("choose either append or overwrite")
	}
	if _, err := archiveFormat(opts.Format); err != nil {
		return zero, err
	}
	if opts.OutputKind != "" && opts.OutputKind != "file" && opts.OutputKind != "mbtiles" && opts.OutputKind != "pmtiles" && opts.OutputKind != "compactv2" {
		return zero, fmt.Errorf("unsupported output type %q", opts.OutputKind)
	}
	source, sourceRemote, err := parseArchiveS3Location(input)
	if err != nil {
		return zero, err
	}
	destination, destinationRemote, err := parseArchiveS3Location(output)
	if err != nil {
		return zero, err
	}
	if destinationRemote && opts.OutputKind != "" {
		destination.kind, err = archiveOutputKind(destination.key, opts.OutputKind)
		if err != nil {
			return zero, err
		}
	}
	if !sourceRemote && !destinationRemote {
		return convertLocalArchive(ctx, input, output, opts)
	}
	work, err := os.MkdirTemp("", ".tiler-convert-s3-*")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(work)
	if sourceRemote {
		out, err := archiveS3Client(ctx, source.bucket, opts.S3)
		if err != nil {
			return zero, err
		}
		if source.kind == "file" && source.key != "" && !strings.HasSuffix(source.key, "/") {
			kind, err := archiveS3DetectArchiveKind(ctx, out, source.key)
			if err != nil {
				return zero, err
			}
			if kind != "" {
				source.kind = kind
			}
		}
		if destinationRemote && source.bucket == destination.bucket {
			// An object and a same-named prefix are independent S3 locations.
			// Resolve ambiguous input keys before comparing their identities.
			destinationPrefix := destination.kind == "file" || destination.kind == "compactv2"
			if (source.kind == "file") == destinationPrefix && strings.TrimRight(source.key, "/") == strings.TrimRight(destination.key, "/") {
				return zero, fmt.Errorf("input and output refer to the same S3 location")
			}
			// Directory prefixes have the same overlap restriction as local
			// directories. In particular, nested Compact output would leave
			// bundles that make the original cache unreadable on its next scan.
			if source.kind == "file" && strings.HasPrefix(destination.key, archiveS3Key(source.key, "")) {
				return zero, fmt.Errorf("S3 input and output prefixes overlap: output cannot be inside the input prefix")
			}
			if destinationPrefix && strings.HasPrefix(source.key, archiveS3Key(destination.key, "")) {
				return zero, fmt.Errorf("S3 input and output prefixes overlap: output prefix cannot contain the input")
			}
		}
		if source.kind == "file" {
			_, compact, err := archiveS3Head(ctx, out, archiveS3Key(source.key, "conf.xml"))
			if err != nil {
				return zero, err
			}
			if compact {
				input = filepath.Join(work, "s3-input-compact")
				if err := archiveS3DownloadCompact(ctx, out, source.key, input); err != nil {
					return zero, err
				}
			} else {
				input = filepath.Join(work, "s3-input.mbtiles")
				if _, err := archiveS3TilesToMBTiles(ctx, out, source.key, input, opts); err != nil {
					return zero, err
				}
			}
		} else {
			input = filepath.Join(work, "s3-input."+source.kind)
			if err := archiveS3Download(ctx, out, source.key, input); err != nil {
				return zero, err
			}
		}
	}
	if !destinationRemote {
		return convertLocalArchive(ctx, input, output, opts)
	}
	out, err := archiveS3Client(ctx, destination.bucket, opts.S3)
	if err != nil {
		return zero, err
	}
	if destination.kind == "compactv2" {
		if destination.key == "" || !strings.HasSuffix(destination.key, "/") {
			return zero, fmt.Errorf("S3 Compact v2 output requires a named prefix ending in /")
		}
		return archiveS3ConvertToCompact(ctx, out, input, destination.key, work, opts)
	}
	if destination.kind != "file" {
		if destination.key == "" || strings.HasSuffix(destination.key, "/") {
			return zero, fmt.Errorf("S3 archive output requires an object key")
		}
		return archiveS3ConvertToObject(ctx, out, input, destination, work, opts)
	}
	if _, compact, err := archiveS3Head(ctx, out, archiveS3Key(destination.key, "conf.xml")); err != nil {
		return zero, err
	} else if compact {
		return zero, fmt.Errorf("S3 output is a Compact cache; use -output-type compactv2 and a prefix ending in /")
	}
	return archiveS3ConvertToTiles(ctx, out, input, destination.key, work, opts)
}

func parseArchiveS3Location(value string) (archiveS3Location, bool, error) {
	var location archiveS3Location
	if !strings.HasPrefix(strings.ToLower(value), "s3:") {
		return location, false, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Host, ":") {
		return location, true, fmt.Errorf("S3 location must be s3://bucket/key without credentials, port, query or fragment")
	}
	key := strings.TrimPrefix(u.Path, "/")
	segments := strings.Split(key, "/")
	for index, segment := range segments {
		if segment == "" && index < len(segments)-1 {
			return location, true, fmt.Errorf("S3 location cannot contain empty internal path segments")
		}
		if segment == "." || segment == ".." || strings.ContainsRune(segment, '\\') {
			return location, true, fmt.Errorf("S3 location cannot contain dot segments or backslashes")
		}
	}
	location = archiveS3Location{bucket: u.Host, key: key, kind: "file"}
	if !strings.HasSuffix(key, "/") {
		switch strings.ToLower(path.Ext(key)) {
		case ".mbtiles":
			location.kind = "mbtiles"
		case ".pmtiles":
			location.kind = "pmtiles"
		}
	}
	return location, true, nil
}

func archiveS3Client(ctx context.Context, bucket string, supplied *s3Output) (*s3Output, error) {
	if supplied == nil {
		return newS3OutputForBucket(ctx, bucket)
	}
	if supplied.client == nil {
		return nil, fmt.Errorf("conversion S3 client is not initialized")
	}
	clone := *supplied
	clone.bucket = bucket
	if clone.timeout <= 0 {
		clone.timeout = time.Minute
	}
	return &clone, nil
}

func archiveS3Missing(err error) bool {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	var api smithy.APIError
	return errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound")
}

func archiveS3Head(ctx context.Context, out *s3Output, key string) (*s3.HeadObjectOutput, bool, error) {
	request, cancel := context.WithTimeout(ctx, out.timeout)
	defer cancel()
	head, err := out.client.HeadObject(request, &s3.HeadObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(key)})
	if archiveS3Missing(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect S3 object %q: %w", key, err)
	}
	return head, true, nil
}

// Explicit output kinds allow an archive to have an arbitrary object key. A
// trailing slash always selects loose tiles; other ambiguous keys are sniffed
// using a bounded header read so those outputs can be used as inputs again.
func archiveS3DetectArchiveKind(ctx context.Context, out *s3Output, key string) (string, error) {
	head, exists, err := archiveS3Head(ctx, out, key)
	if err != nil || !exists {
		return "", err
	}
	request, cancel := context.WithTimeout(ctx, out.timeout)
	defer cancel()
	input := &s3.GetObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(key)}
	// A range addresses encoded object bytes, not the decoded archive header.
	// For gzip, read only the decoded prefix from a normal streaming GET.
	if !strings.EqualFold(strings.TrimSpace(aws.ToString(head.ContentEncoding)), "gzip") {
		input.Range = aws.String("bytes=0-15")
	}
	object, err := out.client.GetObject(request, input)
	if err != nil {
		return "", fmt.Errorf("inspect S3 archive %q: %w", key, err)
	}
	defer object.Body.Close()
	decoded, err := archiveS3DecodeContent(object.Body, aws.ToString(object.ContentEncoding))
	if err != nil {
		return "", err
	}
	defer decoded.Close()
	header, err := io.ReadAll(io.LimitReader(decoded, 16))
	if err != nil {
		return "", fmt.Errorf("inspect S3 archive %q: %w", key, err)
	}
	switch {
	case bytes.HasPrefix(header, []byte("SQLite format 3\x00")):
		return "mbtiles", nil
	case bytes.HasPrefix(header, []byte("PMTiles")):
		return "pmtiles", nil
	default:
		return "", nil
	}
}

func archiveS3Download(ctx context.Context, out *s3Output, key, filename string) error {
	return archiveS3DownloadVersion(ctx, out, key, filename, "")
}

func archiveS3DownloadVersion(ctx context.Context, out *s3Output, key, filename, expectedETag string) error {
	request, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	input := &s3.GetObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(key)}
	if expectedETag != "" {
		input.IfMatch = aws.String(expectedETag)
	}
	object, err := out.client.GetObject(request, input)
	if err != nil {
		return fmt.Errorf("read S3 archive %q: %w", key, err)
	}
	defer object.Body.Close()
	if actual := aws.ToString(object.ETag); expectedETag != "" && actual != "" && actual != expectedETag {
		return fmt.Errorf("S3 object %q changed since it was listed", key)
	}
	encoded := &archiveS3CountingReader{Reader: object.Body}
	decoded, err := archiveS3DecodeContent(encoded, aws.ToString(object.ContentEncoding))
	if err != nil {
		return fmt.Errorf("read S3 archive %q: %w", key, err)
	}
	defer decoded.Close()
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = archiveCopy(request, file, decoded)
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("download S3 archive %q: %w", key, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if object.ContentLength != nil && *object.ContentLength >= 0 {
		if encoded.bytes != *object.ContentLength {
			return fmt.Errorf("S3 archive %q was truncated", key)
		}
	}
	return nil
}

type archiveS3CountingReader struct {
	io.Reader
	bytes int64
}

func (reader *archiveS3CountingReader) Read(p []byte) (int, error) {
	n, err := reader.Reader.Read(p)
	reader.bytes += int64(n)
	return n, err
}

// S3's SDK returns encoded object bytes. Decode the HTTP representation before
// parsing tiles or metadata; PBF's own content compression is handled separately.
func archiveS3DecodeContent(body io.Reader, encoding string) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return io.NopCloser(body), nil
	case "gzip":
		reader, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("decode S3 gzip content: %w", err)
		}
		return reader, nil
	default:
		return nil, fmt.Errorf("unsupported S3 Content-Encoding %q; supported: identity and gzip", encoding)
	}
}

func archiveS3ReadObject(ctx context.Context, out *s3Output, key string, limit int64) ([]byte, error) {
	body, _, _, err := archiveS3ReadObjectVersion(ctx, out, key, limit, "")
	return body, err
}

func archiveS3ReadObjectVersion(ctx context.Context, out *s3Output, key string, limit int64, expectedETag string) ([]byte, map[string]string, string, error) {
	request, cancel := context.WithTimeout(ctx, out.timeout)
	defer cancel()
	input := &s3.GetObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(key)}
	if expectedETag != "" {
		input.IfMatch = aws.String(expectedETag)
	}
	object, err := out.client.GetObject(request, input)
	if err != nil {
		return nil, nil, "", err
	}
	defer object.Body.Close()
	if actual := aws.ToString(object.ETag); expectedETag != "" && actual != "" && actual != expectedETag {
		return nil, nil, "", fmt.Errorf("S3 object %q changed since it was listed", key)
	}
	if object.ContentLength != nil && *object.ContentLength > limit {
		return nil, nil, "", fmt.Errorf("S3 object %q exceeds %d bytes", key, limit)
	}
	encoded := &archiveS3CountingReader{Reader: io.LimitReader(object.Body, limit+1)}
	contentEncoding := aws.ToString(object.ContentEncoding)
	decoded, err := archiveS3DecodeContent(encoded, contentEncoding)
	if decoded != nil {
		defer decoded.Close()
	}
	if encoded.bytes > limit {
		return nil, nil, "", fmt.Errorf("S3 object %q exceeds %d encoded bytes", key, limit)
	}
	if err != nil {
		return nil, nil, "", err
	}
	body, err := io.ReadAll(io.LimitReader(decoded, limit+1))
	if encoded.bytes > limit {
		return nil, nil, "", fmt.Errorf("S3 object %q exceeds %d encoded bytes", key, limit)
	}
	if err != nil {
		return nil, nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, nil, "", fmt.Errorf("S3 object %q exceeds %d bytes", key, limit)
	}
	return body, object.Metadata, contentEncoding, nil
}

func archiveS3InferFormat(body []byte, contentEncoding string) (string, error) {
	format, err := archiveInferFormat(body)
	// Legacy unmarked vector objects used their gzip layer as the format hint.
	// Preserve that hint after HTTP decoding; normal tile validation still runs.
	if err != nil && strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") {
		if _, err := prepareTileData(body, PBF, false); err != nil {
			return "", err
		}
		return PBF, nil
	}
	return format, err
}

func archiveS3Metadata(ctx context.Context, out *s3Output, prefix string) (map[string]string, bool, error) {
	data, err := archiveS3ReadObject(ctx, out, archiveS3Key(prefix, "metadata.json"), maxMetadataBytes)
	if archiveS3Missing(err) {
		return map[string]string{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read S3 tile metadata: %w", err)
	}
	metadata, err := archiveDecodeJSONMetadata(data)
	if err != nil {
		return nil, false, fmt.Errorf("read S3 tile metadata: %w", err)
	}
	return metadata, true, nil
}

func archiveS3Key(prefix, relative string) string {
	if prefix == "" {
		return relative
	}
	return strings.TrimRight(prefix, "/") + "/" + relative
}

func archiveS3Walk(ctx context.Context, out *s3Output, prefix string, visit func(key, relative string, size int64) error) error {
	return archiveS3WalkVersioned(ctx, out, prefix, func(key, relative string, size int64, _ string) error {
		return visit(key, relative, size)
	})
}

func archiveS3WalkVersioned(ctx context.Context, out *s3Output, prefix string, visit func(key, relative string, size int64, etag string) error) error {
	prefix = strings.TrimRight(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	var token *string
	seen := make(map[string]bool)
	for {
		request, cancel := context.WithTimeout(ctx, out.timeout)
		page, err := out.client.ListObjectsV2(request, &s3.ListObjectsV2Input{Bucket: aws.String(out.bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		cancel()
		if err != nil {
			return fmt.Errorf("list S3 tiles: %w", err)
		}
		for _, object := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := aws.ToString(object.Key)
			if !strings.HasPrefix(key, prefix) {
				return fmt.Errorf("S3 listing returned a key outside the requested prefix")
			}
			if err := visit(key, strings.TrimPrefix(key, prefix), aws.ToInt64(object.Size), aws.ToString(object.ETag)); err != nil {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			return nil
		}
		next := aws.ToString(page.NextContinuationToken)
		if next == "" || seen[next] {
			return fmt.Errorf("S3 listing returned an invalid continuation token")
		}
		seen[next], token = true, page.NextContinuationToken
	}
}

func archiveS3TilesToMBTiles(ctx context.Context, out *s3Output, prefix, filename string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var result ArchiveConvertResult
	metadata, _, err := archiveS3Metadata(ctx, out, prefix)
	if err != nil {
		return result, err
	}
	pattern := opts.InputTilePath
	if pattern == "" {
		pattern, err = tilePathFromMetadata(metadata)
	} else {
		pattern, err = normalizeTilePath(pattern)
	}
	if err != nil {
		return result, err
	}
	if err := archiveApplyMetadata(metadata, opts.Metadata); err != nil {
		return result, err
	}
	target, err := archiveFormat(opts.Format)
	if err != nil {
		return result, err
	}
	if target == "" {
		target, err = archiveFormat(metadata["format"])
		if err != nil {
			return result, err
		}
	}
	db, err := archiveOpenSQLite(filename, false)
	if err != nil {
		return result, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := archiveS3CreateTables(ctx, tx); err != nil {
		return result, err
	}
	extent := archiveExtent{}
	err = archiveS3WalkVersioned(ctx, out, prefix, func(key, relative string, size int64, etag string) error {
		if strings.HasSuffix(key, "/") || relative == "metadata.json" {
			return nil
		}
		tile, sourceFormat, matches, err := parseTilePath(pattern, relative)
		if err != nil {
			return fmt.Errorf("S3 tile %q: %w", key, err)
		}
		if !matches {
			return nil
		}
		if size > maxTileDataBytes {
			return fmt.Errorf("S3 tile %q exceeds the 64 MiB limit", key)
		}
		body, identity, contentEncoding, err := archiveS3ReadObjectVersion(ctx, out, key, maxTileDataBytes, etag)
		if err != nil {
			return fmt.Errorf("read S3 tile %q: %w", key, err)
		}
		if hash := identity["tiler-path"]; hash != "" && hash != fmt.Sprintf("%x", sha256.Sum256([]byte(pattern))) {
			return fmt.Errorf("S3 tile %q has a different layout; set -input-tile-path to its original template", key)
		}
		if xyz := identity["tiler-xyz"]; xyz != "" && xyz != fmt.Sprintf("%d/%d/%d", tile.Z, tile.X, tile.Y) {
			return fmt.Errorf("S3 tile %q coordinates do not match the input tile path", key)
		}
		if stored := identity["tiler-format"]; stored != "" {
			markedFormat, err := archiveFormat(stored)
			if err != nil || markedFormat == "" {
				return fmt.Errorf("S3 tile %q has invalid format metadata", key)
			}
			parsedFormat, err := archiveFormat(sourceFormat)
			if err != nil || (parsedFormat != "" && parsedFormat != markedFormat) {
				return fmt.Errorf("S3 tile %q format does not match its path", key)
			}
			sourceFormat = markedFormat
		}
		if sourceFormat == "" {
			sourceFormat, err = archiveFormat(metadata["format"])
			if err != nil {
				return err
			}
			if sourceFormat == "" {
				sourceFormat, err = archiveS3InferFormat(body, contentEncoding)
				if err != nil && target == "" {
					return err
				}
			}
		} else {
			sourceFormat, err = archiveFormat(sourceFormat)
			if err != nil {
				return err
			}
		}
		if target == "" {
			target = sourceFormat
		}
		if opts.Format == "" && sourceFormat != "" && sourceFormat != target {
			return fmt.Errorf("mixed S3 tile formats require an explicit -format")
		}
		body, err = archivePrepareTileData(body, sourceFormat, target)
		if err != nil {
			return fmt.Errorf("S3 tile %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tiles(zoom_level,tile_column,tile_row,tile_data) VALUES(?,?,?,?)", tile.Z, tile.X, (Tile{T: tile}).flipY(), body); err != nil {
			return fmt.Errorf("insert S3 tile (duplicate coordinates are not allowed): %w", err)
		}
		extent.add(int(tile.Z), tile.X, tile.Y)
		return nil
	})
	if err != nil {
		return result, err
	}
	if extent.count == 0 {
		return result, fmt.Errorf("S3 prefix contains no supported tiles for the configured input tile path")
	}
	delete(metadata, "tile_path")
	if err := archiveCompleteMetadata(metadata, target, path.Base(strings.TrimRight(prefix, "/")), extent); err != nil {
		return result, err
	}
	if err := archiveS3WriteMetadata(ctx, tx, metadata); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return ArchiveConvertResult{Tiles: extent.count, Format: target}, nil
}

func archiveS3CreateTables(ctx context.Context, tx *sql.Tx) error {
	for _, query := range []string{
		"CREATE TABLE metadata(name TEXT PRIMARY KEY,value TEXT)",
		"CREATE TABLE tiles(zoom_level INTEGER,tile_column INTEGER,tile_row INTEGER,tile_data BLOB,PRIMARY KEY(zoom_level,tile_column,tile_row))",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func archiveS3WriteMetadata(ctx context.Context, tx *sql.Tx, metadata map[string]string) error {
	for key, value := range metadata {
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO metadata(name,value) VALUES(?,?)", key, value); err != nil {
			return err
		}
	}
	return nil
}

func archiveS3ConvertToObject(ctx context.Context, out *s3Output, input string, destination archiveS3Location, work string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var zero ArchiveConvertResult
	_, exists, err := archiveS3Head(ctx, out, destination.key)
	if err != nil {
		return zero, err
	}
	if exists && !opts.Overwrite && !opts.Append {
		return zero, fmt.Errorf("S3 output already exists; use -overwrite or -append")
	}
	filename := filepath.Join(work, "s3-output."+destination.kind)
	if opts.Append && exists {
		if err := archiveS3Download(ctx, out, destination.key, filename); err != nil {
			return zero, err
		}
	}
	local := opts
	local.OutputKind, local.OutputTilePath = destination.kind, ""
	local.Overwrite = false
	result, err := convertLocalArchive(ctx, input, filename, local)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return result, err
	}
	uploader := manager.NewUploader(out.client, func(u *manager.Uploader) {
		u.PartSize, u.Concurrency, u.LeavePartsOnError = defaultArchivePartSize, defaultArchiveConcurrency, true
	})
	upload := archiveUploadFile{ArchiveUploadObject: ArchiveUploadObject{Path: filename, Key: destination.key, Size: info.Size()}, info: info}
	if err := uploadArchiveFile(ctx, out, uploader, upload, time.Hour); err != nil {
		return result, err
	}
	return result, nil
}

func archiveS3ConvertToTiles(ctx context.Context, out *s3Output, input, prefix, work string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var result ArchiveConvertResult
	pattern := opts.OutputTilePath
	if pattern == "" {
		pattern = out.tilePath
	}
	pattern, err := normalizeTilePath(pattern)
	if err != nil {
		return result, err
	}
	if err := validateTilePathFileConflicts(pattern, PNG, "metadata.json"); err != nil {
		return result, err
	}
	out.prefix, out.tilePath = strings.TrimRight(prefix, "/"), pattern
	old, hasMetadata, err := archiveS3Metadata(ctx, out, prefix)
	if err != nil {
		return result, err
	}
	oldPattern, err := tilePathFromMetadata(old)
	if err != nil {
		return result, fmt.Errorf("invalid existing S3 tile path: %w", err)
	}
	listedFormat, err := archiveFormat(old["format"])
	if err != nil {
		return result, err
	}
	hasDeclaredFormat := listedFormat != ""
	oldExtent := archiveExtent{}
	anyObjects := hasMetadata
	firstExisting := (*maptile.Tile)(nil)
	err = archiveS3Walk(ctx, out, prefix, func(key, relative string, size int64) error {
		anyObjects = true
		tile, format, matched, err := parseTilePath(pattern, relative)
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		if format != "" {
			canonical, err := archiveFormat(format)
			if err != nil {
				return err
			}
			if listedFormat != "" && canonical != listedFormat {
				return fmt.Errorf("existing S3 tiles contain mixed formats; use a new output prefix")
			}
			listedFormat, old["format"] = canonical, canonical
		}
		renderFormat := listedFormat
		if renderFormat == "" {
			// A template without {format} renders identically for every encoding.
			renderFormat = PNG
		}
		canonical, err := renderTilePath(pattern, tile, renderFormat)
		if err != nil {
			return err
		}
		if relative != canonical {
			return fmt.Errorf("existing S3 tile %q uses a noncanonical key; convert the complete dataset to a new prefix before extending it", relative)
		}
		if size <= 0 {
			return nil
		}
		if firstExisting == nil {
			copy := tile
			firstExisting = &copy
		}
		oldExtent.add(int(tile.Z), tile.X, tile.Y)
		return nil
	})
	if err != nil {
		return result, err
	}
	if anyObjects && !opts.Overwrite && !opts.Append {
		return result, fmt.Errorf("S3 output prefix already contains objects; use -overwrite or -append")
	}
	if anyObjects && hasMetadata && oldPattern != pattern {
		return result, fmt.Errorf("S3 output tile path must match the existing dataset")
	}
	out.allowUnmarkedTiles = hasMetadata && oldPattern == pattern
	if firstExisting != nil && listedFormat == "" {
		key := mustArchiveS3TileKey(out, *firstExisting, PNG)
		head, _, err := archiveS3Head(ctx, out, key)
		if err != nil {
			return result, err
		}
		if head != nil {
			listedFormat, err = archiveFormat(head.Metadata["tiler-format"])
			if err != nil {
				return result, err
			}
		}
		if listedFormat == "" {
			body, _, contentEncoding, err := archiveS3ReadObjectVersion(ctx, out, key, maxTileDataBytes, "")
			if err != nil {
				return result, err
			}
			listedFormat, err = archiveS3InferFormat(body, contentEncoding)
			if err != nil {
				return result, fmt.Errorf("existing extensionless S3 tiles need a format in metadata.json: %w", err)
			}
		}
		old["format"] = listedFormat
	}
	staged := filepath.Join(work, "s3-tiles.mbtiles")
	local := opts
	local.OutputKind, local.Append, local.Overwrite, local.OutputTilePath = "mbtiles", false, false, ""
	local.allowMissingVectorMetadata = true
	result, err = convertLocalArchive(ctx, input, staged, local)
	if err != nil {
		return result, err
	}
	oldFormat, err := archiveFormat(old["format"])
	if err != nil {
		return result, err
	}
	if oldFormat != "" && oldFormat != result.Format {
		return result, fmt.Errorf("S3 output tile format must match the existing dataset; use a new prefix for a different encoding")
	}
	if !hasMetadata && firstExisting != nil && pattern != defaultTilePath {
		// New direct-download objects carry a path marker, allowing an existing
		// custom-layout dataset to be extended without a prefix metadata file.
		head, _, err := archiveS3Head(ctx, out, mustArchiveS3TileKey(out, *firstExisting, result.Format))
		if err != nil {
			return result, err
		}
		if head == nil || head.Metadata["tiler-path"] == "" {
			return result, fmt.Errorf("existing custom-layout S3 tiles need layout metadata before they can be extended")
		}
		if _, err := out.exists(ctx, *firstExisting, result.Format); err != nil {
			return result, err
		}
	}
	// Validate every retained tile, not just an overlapping input tile or the
	// first listed object. Otherwise mixed layouts from older direct downloads
	// could be hidden behind the new prefix metadata after a successful append.
	if anyObjects {
		if err := archiveS3Walk(ctx, out, prefix, func(key, relative string, size int64) error {
			tile, pathFormat, matched, err := parseTilePath(pattern, relative)
			if err != nil || !matched || size <= 0 {
				return err
			}
			head, exists, err := archiveS3Head(ctx, out, key)
			if err != nil {
				return err
			}
			if !exists || head.ContentLength == nil || aws.ToInt64(head.ContentLength) != size {
				return fmt.Errorf("existing S3 tile %q changed while preparing the conversion", key)
			}
			if err := out.validateTileIdentity(head.Metadata, tile, result.Format); err != nil {
				return fmt.Errorf("existing S3 tile %q: %w", key, err)
			}
			// Without an extension, prefix format, or per-object marker, the
			// first tile's detected encoding cannot describe every retained
			// tile. Check those legacy payloads before publishing one format.
			if pathFormat == "" && !hasDeclaredFormat && head.Metadata["tiler-format"] == "" {
				body, _, contentEncoding, err := archiveS3ReadObjectVersion(ctx, out, key, maxTileDataBytes, aws.ToString(head.ETag))
				if err != nil {
					return fmt.Errorf("read existing S3 tile %q: %w", key, err)
				}
				format, err := archiveS3InferFormat(body, contentEncoding)
				if err != nil || !sameTileEncoding(format, result.Format) {
					return fmt.Errorf("existing S3 tile %q has a different or invalid format; use a new output prefix", key)
				}
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	db, err := archiveOpenSQLite(staged, true)
	if err != nil {
		return result, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	metadata, err := archiveReadMBMetadata(ctx, tx)
	if err != nil {
		return result, err
	}
	newExtent, err := archiveS3MBExtent(ctx, tx)
	if err != nil {
		return result, err
	}
	// Explicit overrides must also replace old values, which otherwise win
	// the merge. Coverage and output layout are derived again below.
	if err := archiveApplyMetadata(old, opts.Metadata); err != nil {
		return result, err
	}
	metadata, err = archiveS3MergeMetadata(old, metadata, oldExtent, newExtent, result.Format)
	if err != nil {
		return result, err
	}
	metadata["tile_path"], metadata["scheme"] = pattern, "xyz"
	if strings.Contains(pattern, "{-y}") {
		metadata["scheme"] = "tms"
	}
	metadataBody, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return result, err
	}
	if len(metadataBody) > maxMetadataBytes {
		return result, fmt.Errorf("S3 output metadata.json exceeds 16 MiB")
	}
	rows, err := tx.QueryContext(ctx, "SELECT zoom_level,tile_column,tile_row,tile_data FROM tiles ORDER BY zoom_level,tile_column,tile_row")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var overlaps int64
	for rows.Next() {
		var z, x, y int64
		var body []byte
		if err := rows.Scan(&z, &x, &y, &body); err != nil {
			return result, err
		}
		if err := archiveCoordinate(z, x, y); err != nil {
			return result, err
		}
		tile := maptile.Tile{Z: maptile.Zoom(z), X: uint32(x), Y: uint32((int64(1) << uint(z)) - 1 - y)}
		exists := false
		if anyObjects {
			exists, err = out.exists(ctx, tile, result.Format)
			if err != nil {
				return result, err
			}
		}
		if exists {
			overlaps++
		}
		if !opts.Append || !exists {
			if err := out.saveContext(ctx, Tile{T: tile, C: body}, result.Format); err != nil {
				return result, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	request, cancel := context.WithTimeout(ctx, out.timeout)
	defer cancel()
	if _, err := out.client.PutObject(request, &s3.PutObjectInput{Bucket: aws.String(out.bucket), Key: aws.String(archiveS3Key(prefix, "metadata.json")), Body: bytes.NewReader(metadataBody), ContentType: aws.String("application/json")}); err != nil {
		return result, fmt.Errorf("write S3 tile metadata: %w", err)
	}
	result.Tiles = oldExtent.count + newExtent.count - overlaps
	return result, nil
}

func mustArchiveS3TileKey(out *s3Output, tile maptile.Tile, format string) string {
	relative, _ := renderTilePath(out.tilePath, tile, format)
	return archiveS3Key(out.prefix, relative)
}

func archiveS3MergeMetadata(old, incoming map[string]string, before, added archiveExtent, format string) (map[string]string, error) {
	metadata, err := archiveMergeMetadata(old, incoming)
	if err != nil {
		return nil, err
	}
	combined := added
	if before.count > 0 {
		if combined.count == 0 {
			combined = before
		} else {
			combined.count += before.count
			combined.minZ = minArchiveS3Int(combined.minZ, before.minZ)
			combined.maxZ = maxArchiveS3Int(combined.maxZ, before.maxZ)
			combined.west, combined.south = math.Min(combined.west, before.west), math.Min(combined.south, before.south)
			combined.east, combined.north = math.Max(combined.east, before.east), math.Max(combined.north, before.north)
		}
	}
	// Retain metadata bounds that describe clipped edges while always expanding
	// to cover new tiles. Rebuilding an appended dataset must not hide region A.
	bounds := []float64{combined.west, combined.south, combined.east, combined.north}
	for _, values := range []map[string]string{old, incoming} {
		if values["bounds"] != "" {
			parsed, err := archiveNumbers(values["bounds"], 4)
			if err != nil {
				return nil, err
			}
			bounds[0], bounds[1] = math.Min(bounds[0], parsed[0]), math.Min(bounds[1], parsed[1])
			bounds[2], bounds[3] = math.Max(bounds[2], parsed[2]), math.Max(bounds[3], parsed[3])
		}
	}
	metadata["bounds"] = archiveBoundsText(bounds)
	delete(metadata, "center")
	if err := archiveCompleteMetadata(metadata, format, metadata["name"], combined); err != nil {
		return nil, err
	}
	metadata["minzoom"], metadata["maxzoom"] = strconv.Itoa(combined.minZ), strconv.Itoa(combined.maxZ)
	return metadata, nil
}

func archiveS3MBExtent(ctx context.Context, tx *sql.Tx) (archiveExtent, error) {
	extent := archiveExtent{}
	rows, err := tx.QueryContext(ctx, "SELECT zoom_level,tile_column,tile_row FROM tiles")
	if err != nil {
		return extent, err
	}
	defer rows.Close()
	for rows.Next() {
		var z, x, y int64
		if err := rows.Scan(&z, &x, &y); err != nil {
			return extent, err
		}
		if err := archiveCoordinate(z, x, y); err != nil {
			return extent, err
		}
		extent.add(int(z), uint32(x), uint32((int64(1)<<uint(z))-1-y))
	}
	return extent, rows.Err()
}

func minArchiveS3Int(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxArchiveS3Int(a, b int) int {
	if a > b {
		return a
	}
	return b
}
