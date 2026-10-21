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
func Count(collection orb.Collection, zoom maptile.Zoom) (int64, error) {
	return CountContext(context.Background(), collection, zoom)
}

// Walk synchronously visits the covering tiles without creating a producer
// goroutine. It stops on cancellation, a visitor error, or malformed geometry.
// The scanline mathematics is retained from the original streaming algorithm.
func Walk(ctx context.Context, collection orb.Collection, zoom maptile.Zoom, visit func(maptile.Tile) error) error {
	if zoom > 30 {
		return fmt.Errorf("tile zoom %d is outside 0..30", zoom)
	}
	if ctx == nil {
		return fmt.Errorf("coverage context is required")
	}
	if err := validateGeometry(ctx, collection); err != nil {
		return err
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
		if err := walkGeometry(ctx, geometry, zoom, emit); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func walkGeometry(ctx context.Context, geometry orb.Geometry, zoom maptile.Zoom, emit func(maptile.Tile) error) error {
	if geometry == nil {
		return nil
	}
	switch g := geometry.(type) {
	case orb.Point:
		return emit(gridAt(g, zoom))
	case orb.MultiPoint:
		seen := make(maptile.Set)
		for _, point := range g {
			if err := ctx.Err(); err != nil {
				return err
			}
			tile := gridAt(point, zoom)
			if seen[tile] {
				continue
			}
			seen[tile] = true
			if len(seen) > maxBoundaryTiles {
				return fmt.Errorf("point coverage exceeds memory budget; split the region or reduce zoom")
			}
			if err := emit(tile); err != nil {
				return err
			}
		}
	case orb.LineString:
		_, err := lineWalk(ctx, g, zoom, nil, emit)
		return err
	case orb.MultiLineString:
		seen := make(maptile.Set)
		for _, line := range g {
			_, err := lineWalk(ctx, line, zoom, nil, func(tile maptile.Tile) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if seen[tile] {
					return nil
				}
				seen[tile] = true
				if len(seen) > maxBoundaryTiles {
					return fmt.Errorf("line coverage exceeds memory budget; split the region or reduce zoom")
				}
				return emit(tile)
			})
			if err != nil {
				return err
			}
		}
	case orb.Ring:
		if len(g) > 0 {
			return polygonWalkContext(ctx, orb.Polygon{g}, zoom, emit)
		}
	case orb.Polygon:
		return polygonWalkContext(ctx, g, zoom, emit)
	case orb.MultiPolygon:
		for _, polygon := range g {
			if err := polygonWalkContext(ctx, polygon, zoom, emit); err != nil {
				return err
			}
		}
	case orb.Collection:
		for _, child := range g {
			if err := walkGeometry(ctx, child, zoom, emit); err != nil {
				return err
			}
		}
	case orb.Bound:
		lo, hi := gridAt(g.Min, zoom), gridAt(g.Max, zoom)
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
