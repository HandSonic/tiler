package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

func runCLI(args []string, stdout, stderr io.Writer) error {
	command := "download"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	var err error
	switch command {
	case "help":
		fmt.Fprintln(stdout, "Usage: tiler [download|convert|upload|repair] [options]")
		fmt.Fprintln(stdout, "Run tiler <command> -h for command options.")
		return nil
	case "download", "repair":
		err = runDownloadCLI(command, args, stdout, stderr)
	case "convert":
		err = runConvertCLI(args, stdout, stderr)
	case "upload":
		err = runUploadCLI(args, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q; use download, convert, upload or repair", command)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runConvertCLI(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "tile directory, MBTiles, PMTiles or s3:// input")
	output := fs.String("output", "", "tile directory, .mbtiles, .pmtiles or s3:// destination")
	config := fs.String("c", "", "S3 configuration file (defaults to conf.toml for S3 URLs)")
	inputPath := fs.String("input-tile-path", "", "input tile layout (defaults to metadata or XYZ)")
	outputPath := fs.String("tile-path", "", "output tile layout (defaults to {z}/{x}/{y}.{format})")
	outputKind := fs.String("output-type", "", "output type: file, mbtiles or pmtiles; otherwise inferred")
	appendTiles := fs.Bool("append", false, "merge into an existing destination and preserve overlapping tiles")
	metadataFile := fs.String("metadata", "", "additional TileJSON metadata JSON file")
	format := fs.String("format", "", "tile format override: png, jpg, pbf or webp")
	name := fs.String("name", "", "optional archive name")
	overwrite := fs.Bool("overwrite", false, "replace an existing destination after conversion succeeds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *output == "" || fs.NArg() != 0 {
		return fmt.Errorf("convert requires -input and -output")
	}
	if *overwrite && *appendTiles {
		return fmt.Errorf("choose either -append or -overwrite")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *config != "" || strings.HasPrefix(strings.ToLower(*input), "s3://") || strings.HasPrefix(strings.ToLower(*output), "s3://") {
		if *config == "" {
			*config = "conf.toml"
		}
		if err := initConf(*config); err != nil {
			return err
		}
	}
	metadata := map[string]string{}
	if *config != "" && viper.GetString("tm.json") != "" {
		metadata["json"] = viper.GetString("tm.json")
	}
	if *metadataFile != "" {
		info, err := os.Stat(*metadataFile)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("metadata must be a regular file")
		}
		if info.Size() > maxMetadataBytes {
			return fmt.Errorf("metadata exceeds 16 MiB")
		}
		file, err := os.Open(*metadataFile)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(body) > maxMetadataBytes {
			return fmt.Errorf("metadata exceeds 16 MiB")
		}
		additional, err := archiveDecodeJSONMetadata(body)
		if err != nil {
			return err
		}
		if err := archiveApplyMetadata(metadata, additional); err != nil {
			return err
		}
	}
	if *name != "" {
		metadata["name"] = *name
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := ConvertArchive(ctx, *input, *output, ArchiveConvertOptions{Format: *format, Overwrite: *overwrite, Append: *appendTiles, Metadata: metadata, InputTilePath: *inputPath, OutputTilePath: *outputPath, OutputKind: *outputKind})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Converted %d %s tiles to %s\n", result.Tiles, result.Format, *output)
	return nil
}

func runUploadCLI(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("c", "conf.toml", "configuration file containing output.s3")
	timeout := fs.Duration("timeout", time.Hour, "total timeout per archive, including retries")
	partMiB := fs.Int64("part-size-mib", defaultArchivePartSize>>20, "multipart upload part size in MiB (at least 5)")
	concurrency := fs.Int("concurrency", defaultArchiveConcurrency, "parallel parts per archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("upload requires one or more archive files or directories")
	}
	if *partMiB < 5 || *partMiB > (1<<63-1)/(1024*1024) {
		return fmt.Errorf("part-size-mib must be at least 5 and fit a 64-bit byte size")
	}
	if *timeout <= 0 || *concurrency < 1 || *concurrency > maxArchiveConcurrency {
		return fmt.Errorf("timeout must be positive and concurrency must be between 1 and %d", maxArchiveConcurrency)
	}
	if err := initConf(*config); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	out, err := newS3Output(ctx)
	if err != nil {
		return err
	}
	result, err := UploadArchives(ctx, out, fs.Args(), ArchiveUploadOptions{Timeout: *timeout, PartSize: *partMiB * 1024 * 1024, Concurrency: *concurrency})
	fmt.Fprintf(stdout, "Uploaded %d archive(s), %d bytes\n", result.Uploaded, result.Bytes)
	return err
}

func configuredLayers() ([]Layer, TileMap, error) {
	tm := TileMap{Name: viper.GetString("tm.name"), Description: viper.GetString("tm.description"), Min: viper.GetInt("tm.min"), Max: viper.GetInt("tm.max"), Format: strings.ToLower(viper.GetString("tm.format")), JSON: viper.GetString("tm.json"), URL: viper.GetString("tm.url")}
	type cfgLayer struct {
		Min, Max     int
		Geojson, URL string
	}
	var cfg []cfgLayer
	if err := viper.UnmarshalKey("lrs", &cfg); err != nil {
		return nil, tm, fmt.Errorf("invalid lrs: %w", err)
	}
	if len(cfg) == 0 {
		return nil, tm, fmt.Errorf("no download layers configured")
	}
	var layers []Layer
	for i, c := range cfg {
		if c.Min < 0 || c.Max > 30 || c.Min > c.Max {
			return nil, tm, fmt.Errorf("lrs[%d] must satisfy 0 <= min <= max <= 30", i)
		}
		collection, err := readCollection(c.Geojson)
		if err != nil {
			return nil, tm, err
		}
		for z := c.Min; z <= c.Max; z++ {
			layers = append(layers, Layer{URL: c.URL, Zoom: z, Collection: collection})
		}
	}
	return layers, tm, nil
}

func runDownloadCLI(command string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("c", "conf.toml", "configuration file")
	interactive := fs.Bool("i", false, "interactive controls: p/r/q then Enter")
	failures := fs.String("failures", "", "failed-tile JSON manifest (required for repair)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if command == "repair" && *failures == "" {
		return fmt.Errorf("repair requires -failures")
	}
	if command == "download" && *failures != "" {
		return fmt.Errorf("use repair -failures to replay a failed-tile manifest")
	}
	if err := initConf(*config); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	layers, tm, err := configuredLayers()
	if err != nil {
		return err
	}
	var task *Task
	if command == "repair" {
		manifest, err := LoadFailureManifest(*failures)
		if err != nil {
			return err
		}
		task, err = NewRepairTaskContext(ctx, layers, tm, manifest, *failures)
		if err != nil {
			return err
		}
	} else {
		task = NewTaskContext(ctx, layers, tm)
	}
	if task == nil {
		return fmt.Errorf("no download layers configured")
	}
	defer task.Cancel()
	if command == "repair" && task.Total == 0 {
		fmt.Fprintln(stdout, "Failure manifest is empty; no tiles need repair.")
		return nil
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			task.Cancel()
		case <-finished:
		}
	}()
	if *interactive {
		fmt.Fprintln(stdout, "Controls: p + Enter = pause, r + Enter = resume, q + Enter = stop")
		go task.readControls(os.Stdin, stdout)
	}
	start := time.Now()
	err = task.Download()
	log.Printf("%.3fs elapsed", time.Since(start).Seconds())
	return err
}
