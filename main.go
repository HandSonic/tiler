package main

import (
	"database/sql"
	"fmt"
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
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		return fmt.Errorf("config file %s does not exist", cfgFile)
	}
	viper.SetConfigType("toml")
	viper.SetConfigFile(cfgFile)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match
	err := viper.ReadInConfig()
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

type TileData struct {
	Z    int
	X    int
	Y    int
	Flag bool
}

// 插入瓦片数据
func insertTiles(db *sql.Tx, tiles []TileData) error {
	// 准备插入语句
	stmt, err := db.Prepare("INSERT INTO tiles (z, x, y,flag) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	// 执行插入
	for _, tile := range tiles {
		_, err = stmt.Exec(tile.Z, tile.X, tile.Y, tile.Flag)
		if err != nil {
			return err
		}
	}
	return nil
}

func testDbTask() {

	db, err := sql.Open("sqlite3", "./tiles.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	createTableSQL := `CREATE TABLE IF NOT EXISTS tiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		z INTEGER,
		x INTEGER,
		y INTEGER,
		flag BOOLEAN
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal(err)
	}

	// 开始事务
	tx, err := db.Begin()
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback() // 如果提交事务前发生错误，则回滚事务

	batchSize := 1 << 10
	// 插入数据
	tileBatch := make([]TileData, 0, batchSize)
	total := 0
	i := 0
	for z := 0; z <= 12; z++ {
		numTiles := 1 << uint(z) // 计算每个缩放级别的瓦片数量
		total += numTiles * numTiles
		log.Printf("级别%d,瓦片数量：%d\n", z, numTiles*numTiles)
		log.Printf("总瓦片数量：%d\n", total)
		for x := 0; x < numTiles; x++ {
			for y := 0; y < numTiles; y++ {
				tile := TileData{Z: z, X: x, Y: y, Flag: false}
				tileBatch = append(tileBatch, tile)
				// 批量插入
				if len(tileBatch) >= batchSize {
					err := insertTiles(tx, tileBatch) // 使用事务执行批量插入操作
					if err != nil {
						log.Fatal(err)
					}
					tileBatch = nil // 清空批次
				}
				i++
				if i > 99999999 {
					goto last
				}
			}

		}
	}
last:
	// 处理剩余的批次
	if len(tileBatch) > 0 {
		err := insertTiles(tx, tileBatch) // 使用事务执行批量插入操作
		if err != nil {
			log.Fatal(err)
		}
	}
	// 提交事务
	err = tx.Commit()
	if err != nil {
		log.Fatal(safeFailureError(err))
	}
}

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(safeFailureError(err))
	}
}
