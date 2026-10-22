package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// compactCacheFile restricts cache transfer to its metadata and canonical
// bundle paths. In particular, a cache directory does not make arbitrary
// XML/JSON files or loose tile images eligible for upload.
func compactCacheFile(relative string) (metadata, allowed bool) {
	switch relative {
	case "conf.xml", "conf.cdi", "metadata.json":
		return true, true
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 3 || parts[0] != "_alllayers" || !strings.HasPrefix(parts[1], "L") {
		return false, false
	}
	z, err := strconv.Atoi(strings.TrimPrefix(parts[1], "L"))
	if err != nil || parts[1] != fmt.Sprintf("L%02d", z) {
		return false, false
	}
	coordinates := compactBundleName.FindStringSubmatch(parts[2])
	if coordinates == nil {
		return false, false
	}
	row, err := strconv.ParseUint(coordinates[1], 16, 32)
	if err != nil {
		return false, false
	}
	column, err := strconv.ParseUint(coordinates[2], 16, 32)
	if err != nil || row%compactPacketSize != 0 || column%compactPacketSize != 0 || compactCoordinate(z, uint32(column), uint32(row)) != nil {
		return false, false
	}
	return false, parts[2] == fmt.Sprintf("R%04xC%04x.bundle", row, column)
}

func archiveS3DownloadCompact(ctx context.Context, out *s3Output, prefix, directory string) error {
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	confSeen := false
	err := archiveS3WalkVersioned(ctx, out, prefix, func(key, relative string, size int64, etag string) error {
		// Never turn untrusted S3 keys into filesystem paths before checking
		// every segment, including keys that are not cache files.
		for _, segment := range strings.Split(strings.TrimSuffix(relative, "/"), "/") {
			if segment == "." || segment == ".." || strings.ContainsRune(segment, '\\') {
				return fmt.Errorf("invalid S3 Compact cache path %q", relative)
			}
		}
		metadata, allowed := compactCacheFile(relative)
		if !allowed {
			if strings.HasSuffix(relative, ".bundle") {
				return fmt.Errorf("invalid S3 Compact bundle path %q", relative)
			}
			return nil
		}
		filename := filepath.Join(directory, filepath.FromSlash(relative))
		if metadata {
			if size > maxMetadataBytes {
				return fmt.Errorf("S3 Compact metadata %q exceeds %d bytes", key, maxMetadataBytes)
			}
			body, _, _, err := archiveS3ReadObjectVersion(ctx, out, key, maxMetadataBytes, etag)
			if err != nil {
				return fmt.Errorf("read S3 Compact metadata %q: %w", key, err)
			}
			if err := os.WriteFile(filename, body, 0600); err != nil {
				return err
			}
			confSeen = confSeen || relative == "conf.xml"
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return err
		}
		return archiveS3DownloadVersion(ctx, out, key, filename, etag)
	})
	if err != nil {
		return err
	}
	if !confSeen {
		return fmt.Errorf("S3 Compact cache is missing conf.xml")
	}
	_, err = compactReadMetadata(directory)
	return err
}

func archiveS3CompactHasObjects(ctx context.Context, out *s3Output, prefix string) (bool, error) {
	request, cancel := context.WithTimeout(ctx, out.timeout)
	defer cancel()
	page, err := out.client.ListObjectsV2(request, &s3.ListObjectsV2Input{
		Bucket: aws.String(out.bucket), Prefix: aws.String(strings.TrimRight(prefix, "/") + "/"), MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("inspect S3 Compact destination: %w", err)
	}
	return len(page.Contents) != 0 || aws.ToBool(page.IsTruncated), nil
}

func archiveS3ConvertToCompact(ctx context.Context, out *s3Output, input, prefix, work string, opts ArchiveConvertOptions) (ArchiveConvertResult, error) {
	var zero ArchiveConvertResult
	exists, err := archiveS3CompactHasObjects(ctx, out, prefix)
	if err != nil {
		return zero, err
	}
	if exists && !opts.Append {
		// Replacing selected objects cannot remove bundles left over from the
		// old cache. Avoid hidden deletion or a mixed cache under new metadata.
		return zero, fmt.Errorf("S3 Compact destination is not empty; use -append to preserve its tiles or choose a new prefix (-overwrite is not supported for an existing Compact prefix)")
	}
	directory := filepath.Join(work, "s3-output-compact")
	var result ArchiveConvertResult
	if exists {
		// Fetch only the fixed metadata files. Bundle keys come from validated
		// incoming coordinates, never from a listing of the whole old cache.
		if err := os.Mkdir(directory, 0700); err != nil {
			return zero, err
		}
		for _, name := range []string{"conf.xml", "conf.cdi", "metadata.json"} {
			key := archiveS3Key(prefix, name)
			head, present, err := archiveS3Head(ctx, out, key)
			if err != nil {
				return zero, err
			}
			if !present {
				if name == "conf.xml" {
					return zero, fmt.Errorf("S3 Compact cache is missing conf.xml")
				}
				continue
			}
			body, _, _, err := archiveS3ReadObjectVersion(ctx, out, key, maxMetadataBytes, aws.ToString(head.ETag))
			if err != nil {
				return zero, fmt.Errorf("read S3 Compact metadata %q: %w", key, err)
			}
			if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
				return zero, err
			}
		}
		previous, err := compactReadMetadata(directory)
		if err != nil {
			return zero, err
		}
		populated, err := archiveS3CompactHasObjects(ctx, out, archiveS3Key(prefix, "_alllayers"))
		if err != nil {
			return zero, err
		}
		if !populated {
			// As with local download initialization, metadata written before
			// the first tile does not establish the actual image dimensions.
			previous["pixel_scale"] = ""
		}
		directory, result, err = archivePrepareCompactAppend(ctx, input, previous, work, opts, func(ctx context.Context, relative, filename string) error {
			key := archiveS3Key(prefix, relative)
			head, present, err := archiveS3Head(ctx, out, key)
			if err != nil || !present {
				return err
			}
			return archiveS3DownloadVersion(ctx, out, key, filename, aws.ToString(head.ETag))
		})
		if err != nil {
			return result, err
		}
	} else {
		local := opts
		local.OutputKind, local.OutputTilePath, local.Overwrite = "compactv2", "", false
		result, err = convertLocalArchive(ctx, input, directory, local)
		if err != nil {
			return result, err
		}
	}
	destination := *out
	destination.prefix = strings.TrimRight(prefix, "/")
	_, err = UploadArchives(ctx, &destination, []string{directory}, ArchiveUploadOptions{compactAppend: exists})
	return result, err
}
