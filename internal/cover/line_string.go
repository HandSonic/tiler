// Coverage algorithms adapted from atlasdatatech/orb v0.2.2.
// Copyright (c) 2017 Paul Mach. See LICENSE.md for the MIT license.
package cover

import (
	"context"
	"fmt"
	"math"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

func lineWalk(
	ctx context.Context,
	line orb.LineString,
	zoom maptile.Zoom,
	ring [][2]uint32,
	emit func(maptile.Tile) error,
) ([][2]uint32, error) {

	prevX := -1.0
	prevY := -1.0

	var x, y float64

	for i := 0; i < len(line)-1; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := gridFraction(line[i], zoom)
		stop := gridFraction(line[i+1], zoom)

		dx := stop[0] - start[0]
		dy := stop[1] - start[1]

		if dy == 0 && dx == 0 {
			x, y = math.Floor(start[0]), math.Floor(start[1])
			if x != prevX || y != prevY {
				if err := emit(maptile.New(uint32(x), uint32(y), zoom)); err != nil {
					return nil, err
				}
				if ring != nil && y != prevY {
					ring = append(ring, [2]uint32{uint32(x), uint32(y)})
					if len(ring) > maxBoundaryTiles {
						return nil, fmt.Errorf("polygon boundary exceeds memory budget; split the region or reduce zoom")
					}
				}
				prevX, prevY = x, y
			}
			continue
		}

		sx := -1.0
		if dx > 0 {
			sx = 1.0
		}
		sy := -1.0
		if dy > 0 {
			sy = 1.0
		}

		x = math.Floor(start[0])
		y = math.Floor(start[1])

		// Use one endpoint order for comparisons in either traversal direction.
		// The same grid corner then uses identical floating-point arithmetic,
		// rather than accumulating direction-dependent crossing times.
		origin, end := start, stop
		if origin[0] > end[0] || (origin[0] == end[0] && origin[1] > end[1]) {
			origin, end = end, origin
		}
		lineDX, lineDY := end[0]-origin[0], end[1]-origin[1]

		if x != prevX || y != prevY {
			// set[maptile.New(uint32(x), uint32(y), zoom)] = true
			if err := emit(maptile.New(uint32(x), uint32(y), zoom)); err != nil {
				return nil, err
			}
			if ring != nil && y != prevY {
				ring = append(ring, [2]uint32{uint32(x), uint32(y)})
				if len(ring) > maxBoundaryTiles {
					return nil, fmt.Errorf("polygon boundary exceeds memory budget; split the region or reduce zoom")
				}
			}
			prevX = x
			prevY = y
		}

		for x != math.Floor(stop[0]) || y != math.Floor(stop[1]) {
			nextX, nextY := x, y
			if sx > 0 {
				nextX++
			}
			if sy > 0 {
				nextY++
			}
			cross := (nextX-origin[0])*lineDY - (nextY-origin[1])*lineDX
			// At a grid corner, use the point's floor-based tile ownership.
			// Same-sign axes cross together; opposite-sign axes visit the
			// corner's tile by stepping the increasing axis first. Always
			// taking Y first emits different side tiles when reversed.
			if x != math.Floor(stop[0]) && y != math.Floor(stop[1]) && cross == 0 && sx == sy {
				x += sx
				y += sy
			} else if x != math.Floor(stop[0]) && (y == math.Floor(stop[1]) || cross*sy < 0 || (cross == 0 && sx > 0)) {
				x += sx
			} else {
				y += sy
			}

			// set[maptile.New(uint32(x), uint32(y), zoom)] = true
			if err := emit(maptile.New(uint32(x), uint32(y), zoom)); err != nil {
				return nil, err
			}
			if ring != nil && y != prevY {
				ring = append(ring, [2]uint32{uint32(x), uint32(y)})
				if len(ring) > maxBoundaryTiles {
					return nil, fmt.Errorf("polygon boundary exceeds memory budget; split the region or reduce zoom")
				}
			}
			prevX = x
			prevY = y
		}
	}

	if len(ring) > 0 && uint32(y) == ring[0][1] {
		ring = ring[:len(ring)-1]
	}

	return ring, nil
}
