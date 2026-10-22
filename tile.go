package main

import (
	"math"

	"github.com/paulmach/orb"

	"github.com/paulmach/orb/maptile"
)

// Tile 自定义瓦片存储
type Tile struct {
	T      maptile.Tile
	C      []byte
	Source string // current source template; never serialized into failure manifests
}

func (tile Tile) flipY() uint32 {
	zpower := math.Pow(2.0, float64(tile.T.Z))
	return uint32(zpower) - 1 - tile.T.Y
}

// Layer 级别&瓦片数
type Layer struct {
	URL        string
	Zoom       int
	Count      int64
	Collection orb.Collection
}

// Constants representing TileFormat types
const (
	PNG  string = "png"
	JPG         = "jpg"
	PBF         = "pbf"
	WEBP        = "webp"
)
