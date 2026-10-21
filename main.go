package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shiena/ansicolor"
	log "github.com/sirupsen/logrus"

	nested "github.com/antonfisher/nested-logrus-formatter"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/viper"
)

func init() {
	//InitLog 初始化日志
	log.SetFormatter(&nested.Formatter{
		HideKeys:        true,
		ShowFullLevel:   true,
		TimestampFormat: "2006-01-02 15:04:05.000",
		// FieldsOrder: []string{"component", "category"},
	})
	// then wrap the log output with it
	log.SetOutput(ansicolor.NewAnsiColorWriter(os.Stdout))
	log.SetLevel(log.DebugLevel)
}

// initConf 初始化配置
func initConf(cfgFile string) error {
	const maxConfigSize = 16 * 1024 * 1024
	info, err := os.Stat(cfgFile)
	if err != nil {
		return fmt.Errorf("inspect config file %s: %w", cfgFile, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config file must be a regular file: %s", cfgFile)
	}
	if info.Size() > maxConfigSize {
		return fmt.Errorf("config file exceeds 16 MiB: %s", cfgFile)
	}
	file, err := os.Open(cfgFile)
	if err != nil {
		return fmt.Errorf("open config file %s: %w", cfgFile, err)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxConfigSize+1))
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("read config file %s: %w", cfgFile, err)
	}
	if closeErr != nil {
		return fmt.Errorf("close config file %s: %w", cfgFile, closeErr)
	}
	if len(body) > maxConfigSize {
		return fmt.Errorf("config file exceeds 16 MiB: %s", cfgFile)
	}
	viper.SetConfigType("toml")
	viper.SetConfigFile(cfgFile)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match
	err = viper.ReadConfig(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("read config file %s: %w", cfgFile, err)
	}
	viper.SetDefault("app.version", "v 0.1.0")
	viper.SetDefault("app.title", "MapCloud Tiler")
	viper.SetDefault("output.format", "mbtiles")
	viper.SetDefault("output.directory", "output")
	viper.SetDefault("task.workers", 4)
	viper.SetDefault("task.savepipe", 1)
	viper.SetDefault("task.timedelay", 0)
	viper.SetDefault("task.timeout_seconds", 30)
	viper.SetDefault("task.retries", 3)
	return nil
}

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(safeFailureError(err))
	}
}
