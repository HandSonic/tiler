package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Shared project defaults for CLI uploads and converted S3 archives. These
// tune transfer concurrency, not S3 format constraints or a heap guarantee.
const defaultArchivePartSize = 8 << 20
const defaultArchiveConcurrency = 3
const maxArchiveConcurrency = 32

const maxS3UploadPartSize int64 = 5 << 30

// ArchiveUploadOptions controls each archive upload. Zero values select a
// one-hour deadline, 8 MiB initial parts, and three concurrent part requests.
// The SDK increases the part size for large seekable files when necessary.
type ArchiveUploadOptions struct {
	Timeout     time.Duration
	PartSize    int64
	Concurrency int
	// Converted Compact caches publish their grid before bundles, allowing
	// both initial transfers and appends to be retried after a partial upload.
	compactMetadataFirst bool
}

// ArchiveUploadObject describes an archive whose upload has completed.
type ArchiveUploadObject struct {
	Path string
	Key  string
	Size int64
}

type ArchiveUploadResult struct {
	Uploaded int
	Bytes    int64
	Objects  []ArchiveUploadObject
}

type archiveUploadFile struct {
	ArchiveUploadObject
	info os.FileInfo
}

// UploadArchives uploads explicit .mbtiles/.pmtiles files, Compact v2 cache
// directories, or recursively finds archives below other input directories.
// By default a Compact cache uploads its bundles before metadata. A directory
// contributes its relative paths; an explicit file contributes its basename.
// All inputs and destination key collisions are checked before any network
// request. Existing S3 keys are replaced only when their complete upload succeeds.
//
// Files are processed in input order and bundle/archive entries in lexical order.
// On failure the result contains only the files already uploaded completely.
// Close SQLite writers and checkpoint MBTiles databases before uploading them;
// an uncheckpointed WAL or active rollback journal means the standalone
// database file is incomplete.
func UploadArchives(ctx context.Context, out *s3Output, inputs []string, opts ArchiveUploadOptions) (ArchiveUploadResult, error) {
	var result ArchiveUploadResult
	if ctx == nil {
		return result, fmt.Errorf("archive upload context is required")
	}
	if out == nil || out.client == nil || out.bucket == "" {
		return result, fmt.Errorf("S3 output must be initialized before uploading archives")
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	if opts.Timeout < 0 {
		return result, fmt.Errorf("archive upload timeout must be positive")
	}
	if opts.PartSize == 0 {
		opts.PartSize = defaultArchivePartSize
	}
	maxPartSize := maxS3UploadPartSize
	maxInt := int64(^uint(0) >> 1)
	if maxInt < maxPartSize {
		maxPartSize = maxInt
	}
	if opts.PartSize < manager.MinUploadPartSize || opts.PartSize > maxPartSize {
		return result, fmt.Errorf("archive upload part size must be between %d and %d bytes on this build", manager.MinUploadPartSize, maxPartSize)
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = defaultArchiveConcurrency
	}
	if opts.Concurrency < 1 || opts.Concurrency > maxArchiveConcurrency {
		return result, fmt.Errorf("archive upload concurrency must be between 1 and %d", maxArchiveConcurrency)
	}
	files, err := planArchiveUploads(ctx, out.prefix, inputs)
	if err != nil {
		return result, err
	}
	if opts.compactMetadataFirst {
		sort.SliceStable(files, func(i, j int) bool {
			left, _ := compactCacheFile(filepath.Base(files[i].Path))
			right, _ := compactCacheFile(filepath.Base(files[j].Path))
			return left && !right
		})
	}
	for _, file := range files {
		if err := checkArchiveUploadSize(file); err != nil {
			return result, err
		}
	}
	uploader := manager.NewUploader(out.client, func(u *manager.Uploader) {
		u.PartSize = opts.PartSize
		u.Concurrency = opts.Concurrency
		// Cleanup below uses a separate, bounded context. The older SDK's
		// built-in cleanup reuses the canceled upload context.
		u.LeavePartsOnError = true
	})
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := uploadArchiveFile(ctx, out, uploader, file, opts.Timeout); err != nil {
			return result, err
		}
		result.Uploaded++
		result.Bytes += file.Size
		result.Objects = append(result.Objects, file.ArchiveUploadObject)
	}
	return result, nil
}

func planArchiveUploads(ctx context.Context, prefix string, inputs []string) ([]archiveUploadFile, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("provide at least one .mbtiles/.pmtiles archive or directory (including Compact v2)")
	}
	var err error
	prefix, err = normalizeS3Prefix(prefix)
	if err != nil {
		return nil, err
	}
	var files []archiveUploadFile
	keys := make(map[string]string)
	add := func(filename, relative string, info os.FileInfo, compact bool) error {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive input %s is not a regular file", filename)
		}
		if !compact && !isArchiveFilename(filename) {
			return fmt.Errorf("archive input %s must have a .mbtiles or .pmtiles extension", filename)
		}
		if info.Size() == 0 {
			return fmt.Errorf("archive input %s is empty", filename)
		}
		if err := checkArchiveUploadWAL(filename); err != nil {
			return err
		}
		file, err := os.Open(filename)
		if err != nil {
			return fmt.Errorf("open archive input %s: %w", filename, err)
		}
		openedInfo, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return fmt.Errorf("stat archive input %s: %w", filename, statErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archive input %s: %w", filename, closeErr)
		}
		if !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() || !openedInfo.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("archive input %s changed during validation", filename)
		}
		key := path.Join(prefix, filepath.ToSlash(relative))
		if previous, exists := keys[key]; exists {
			return fmt.Errorf("archive inputs %s and %s map to the same S3 key %q", previous, filename, key)
		}
		keys[key] = filename
		files = append(files, archiveUploadFile{ArchiveUploadObject: ArchiveUploadObject{Path: filename, Key: key, Size: info.Size()}, info: info})
		return nil
	}
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(input)
		if err != nil {
			return nil, fmt.Errorf("resolve archive input %s: %w", input, err)
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, fmt.Errorf("stat archive input %s: %w", input, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("archive input %s is a symbolic link", input)
		}
		if !info.IsDir() {
			if err := add(absolute, filepath.Base(absolute), info, false); err != nil {
				return nil, err
			}
			continue
		}
		compact, err := compactIsDirectory(absolute)
		if err != nil {
			return nil, err
		}
		if compact {
			if _, err := compactReadMetadata(absolute); err != nil {
				return nil, err
			}
		}
		cacheMetadata := make(map[string]os.FileInfo)
		err = filepath.WalkDir(absolute, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive directory contains symbolic link %s", filename)
			}
			if entry.IsDir() {
				return nil
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if !entryInfo.Mode().IsRegular() {
				return fmt.Errorf("archive directory contains non-regular file %s", filename)
			}
			relative, err := filepath.Rel(absolute, filename)
			if err != nil {
				return err
			}
			if compact {
				metadata, allowed := compactCacheFile(filepath.ToSlash(relative))
				if !allowed {
					if strings.HasSuffix(relative, ".bundle") {
						return fmt.Errorf("invalid Compact bundle path %q", relative)
					}
					return nil
				}
				if metadata {
					if entryInfo.Size() > maxMetadataBytes {
						return fmt.Errorf("Compact metadata %s exceeds %d bytes", filename, maxMetadataBytes)
					}
					cacheMetadata[relative] = entryInfo
					return nil
				}
			} else if !isArchiveFilename(filename) {
				return nil
			}
			return add(filename, relative, entryInfo, compact)
		})
		if err != nil {
			return nil, fmt.Errorf("scan archive directory %s: %w", input, err)
		}
		// conf.xml is also the remote cache detection marker. Upload it last;
		// this is not an atomic transaction across all objects of a cache.
		for _, relative := range []string{"metadata.json", "conf.cdi", "conf.xml"} {
			if info, ok := cacheMetadata[relative]; ok {
				if err := add(filepath.Join(absolute, relative), relative, info, true); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .mbtiles/.pmtiles archives or Compact v2 cache found in the inputs")
	}
	return files, nil
}

func isArchiveFilename(filename string) bool {
	extension := strings.ToLower(filepath.Ext(filename))
	return extension == ".mbtiles" || extension == ".pmtiles"
}

func checkArchiveUploadWAL(filename string) error {
	if !strings.EqualFold(filepath.Ext(filename), ".mbtiles") {
		return nil
	}
	return checkSQLiteSidecars(filename)
}

func checkArchiveUploadSize(input archiveUploadFile) error {
	// The SDK uses int for each part's length, including auto-grown parts.
	partSize := input.Size/int64(manager.MaxUploadParts) + 1
	if partSize > maxS3UploadPartSize || partSize > int64(^uint(0)>>1) {
		return fmt.Errorf("archive %s is too large for multipart uploads on this build; use an amd64 build or split the archive", input.Path)
	}
	return nil
}

func uploadArchiveFile(parent context.Context, out *s3Output, uploader *manager.Uploader, input archiveUploadFile, timeout time.Duration) error {
	if err := checkArchiveUploadSize(input); err != nil {
		return err
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", input.Path, err)
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat archive %s: %w", input.Path, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(input.info, current) || current.Size() != input.Size || !current.ModTime().Equal(input.info.ModTime()) {
		return fmt.Errorf("archive %s changed after upload validation", input.Path)
	}
	if err := checkArchiveUploadWAL(input.Path); err != nil {
		return err
	}
	contentType := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(input.Path)) {
	case ".mbtiles":
		contentType = "application/vnd.sqlite3"
	case ".pmtiles":
		contentType = "application/vnd.pmtiles"
	case ".xml", ".cdi":
		contentType = "application/xml"
	case ".json":
		contentType = "application/json"
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(out.bucket), Key: aws.String(input.Key),
		Body: file, ContentType: aws.String(contentType),
	})
	if err != nil {
		var multipart manager.MultiUploadFailure
		if errors.As(err, &multipart) && multipart.UploadID() != "" {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, cleanupErr := out.client.AbortMultipartUpload(cleanupCtx, &s3.AbortMultipartUploadInput{
				Bucket: aws.String(out.bucket), Key: aws.String(input.Key), UploadId: aws.String(multipart.UploadID()),
			})
			cleanupCancel()
			if cleanupErr != nil {
				var status interface{ HTTPStatusCode() int }
				if !errors.As(cleanupErr, &status) || status.HTTPStatusCode() != 404 {
					return fmt.Errorf("upload s3://%s/%s failed and multipart cleanup also failed (%v): %w", out.bucket, input.Key, cleanupErr, err)
				}
			}
		}
		return fmt.Errorf("upload %s to s3://%s/%s: %w", input.Path, out.bucket, input.Key, err)
	}
	current, err = file.Stat()
	if err != nil || current.Size() != input.Size || !current.ModTime().Equal(input.info.ModTime()) {
		return fmt.Errorf("archive %s changed while uploading; replace the remote object with a stable archive", input.Path)
	}
	if err := checkArchiveUploadWAL(input.Path); err != nil {
		return fmt.Errorf("MBTiles became uncheckpointed while uploading; replace the remote object with a stable archive: %w", err)
	}
	return nil
}
