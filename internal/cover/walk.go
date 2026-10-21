// Coverage algorithms adapted from atlasdatatech/orb v0.2.2.
// Copyright (c) 2017 Paul Mach. See LICENSE.md for the MIT license.
package cover

import (
	"context"
	"fmt"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

// Count reports the number of visits produced by Walk. Like the original
// streaming implementation, different geometries may visit a shared tile more
// than once. Unlike the old API, malformed geometry cannot silently panic.
func Count(collection orb.Collection, zoom maptile.Zoom) (count int64, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			count = 0
			err = fmt.Errorf("count tile coverage: %v", failure)
		}
	}()
	if zoom > 30 {
		return 0, fmt.Errorf("tile zoom %d is outside 0..30", zoom)
	}
	return CollectionCount(collection, zoom), nil
}

// Walk synchronously visits the covering tiles without creating a producer
// goroutine. It stops on cancellation, a visitor error, or malformed geometry.
// The scanline mathematics is retained from the original streaming algorithm.
func Walk(ctx context.Context, collection orb.Collection, zoom maptile.Zoom, visit func(maptile.Tile) error) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("walk tile coverage: %v", failure)
		}
	}()
	if zoom > 30 {
		return fmt.Errorf("tile zoom %d is outside 0..30", zoom)
	}
	if visit == nil {
		return fmt.Errorf("tile coverage visitor is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	emit := func(tile maptile.Tile) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(tile)
	}
	for _, geometry := range collection {
		if err := walkGeometry(geometry, zoom, emit); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func walkGeometry(geometry orb.Geometry, zoom maptile.Zoom, emit func(maptile.Tile) error) error {
	if geometry == nil {
		return nil
	}
	switch g := geometry.(type) {
	case orb.Point:
		return emit(maptile.At(g, zoom))
	case orb.MultiPoint:
		// Match Count and the upstream geometry set for duplicate points.
		for tile := range MultiPoint(g, zoom) {
			if err := emit(tile); err != nil {
				return err
			}
		}
	case orb.LineString:
		_, err := lineWalk(g, zoom, nil, emit)
		return err
	case orb.MultiLineString:
		for tile := range MultiLineString(g, zoom) {
			if err := emit(tile); err != nil {
				return err
			}
		}
	case orb.Ring:
		if len(g) > 0 {
			return polygonWalk(orb.Polygon{g}, zoom, emit)
		}
	case orb.Polygon:
		return polygonWalk(g, zoom, emit)
	case orb.MultiPolygon:
		for _, polygon := range g {
			if err := polygonWalk(polygon, zoom, emit); err != nil {
				return err
			}
		}
	case orb.Collection:
		for _, child := range g {
			if err := walkGeometry(child, zoom, emit); err != nil {
				return err
			}
		}
	case orb.Bound:
		lo, hi := maptile.At(g.Min, zoom), maptile.At(g.Max, zoom)
		for x := lo.X; x <= hi.X; x++ {
			for y := hi.Y; y <= lo.Y; y++ {
				if err := emit(maptile.Tile{X: x, Y: y, Z: zoom}); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("unsupported coverage geometry %T", geometry)
	}
	return nil
}
