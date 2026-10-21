package cover

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

// Boundaries must fit on a 32-bit process. Interiors are counted as scanline
// spans and streamed during Walk, so this does not limit the covered area.
const maxBoundaryTiles = 1 << 20

// Typed in-memory collections have no JSON byte budget. Cap nesting to catch
// cycles and pathological recursive input before walking/counting it.
const maxGeometryDepth = 128

func gridFraction(p orb.Point, z maptile.Zoom) orb.Point {
	f := maptile.Fraction(p, z)
	limit := math.Nextafter(math.Exp2(float64(z)), 0)
	for i := range f {
		if f[i] < 0 {
			f[i] = 0
		} else if f[i] > limit {
			f[i] = limit
		}
	}
	return f
}

func gridAt(p orb.Point, z maptile.Zoom) maptile.Tile {
	f := gridFraction(p, z)
	return maptile.Tile{X: uint32(f[0]), Y: uint32(f[1]), Z: z}
}

func validateGeometry(ctx context.Context, geometry orb.Geometry) error {
	return validateGeometryDepth(ctx, geometry, 0)
}

func validateGeometryDepth(ctx context.Context, geometry orb.Geometry, depth int) error {
	if depth > maxGeometryDepth {
		return fmt.Errorf("coverage geometry exceeds nesting budget")
	}
	point := func(p orb.Point) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if math.IsNaN(p[0]) || math.IsNaN(p[1]) || math.IsInf(p[0], 0) || math.IsInf(p[1], 0) || p[0] < -180 || p[0] > 180 || p[1] < -90 || p[1] > 90 {
			return fmt.Errorf("coverage coordinates must be finite longitude/latitude values")
		}
		return nil
	}
	switch g := geometry.(type) {
	case nil:
		return nil
	case orb.Point:
		return point(g)
	case orb.MultiPoint:
		for _, p := range g {
			if err := point(p); err != nil {
				return err
			}
		}
	case orb.LineString:
		if len(g) < 2 {
			return fmt.Errorf("coverage LineString needs at least two positions")
		}
		for _, p := range g {
			if err := point(p); err != nil {
				return err
			}
		}
	case orb.Ring:
		if len(g) < 4 || g[0] != g[len(g)-1] {
			return fmt.Errorf("coverage polygon rings must contain at least four positions and be closed")
		}
		for _, p := range g {
			if err := point(p); err != nil {
				return err
			}
		}
	case orb.MultiLineString:
		for _, line := range g {
			if err := validateGeometryDepth(ctx, line, depth+1); err != nil {
				return err
			}
		}
	case orb.Polygon:
		if len(g) == 0 {
			return fmt.Errorf("coverage polygon needs an outer ring")
		}
		for _, ring := range g {
			if err := validateGeometryDepth(ctx, ring, depth+1); err != nil {
				return err
			}
		}
	case orb.MultiPolygon:
		for _, polygon := range g {
			if err := validateGeometryDepth(ctx, polygon, depth+1); err != nil {
				return err
			}
		}
	case orb.Collection:
		for _, child := range g {
			if err := validateGeometryDepth(ctx, child, depth+1); err != nil {
				return err
			}
		}
	case orb.Bound:
		if g.IsEmpty() {
			return fmt.Errorf("coverage bounds must not be inverted")
		}
		if err := point(g.Min); err != nil {
			return err
		}
		return point(g.Max)
	default:
		return fmt.Errorf("unsupported coverage geometry %T", geometry)
	}
	return ctx.Err()
}

// CountContext can be canceled even while preparing polygon boundaries. It
// adds interior scanline lengths instead of iterating through every tile.
func CountContext(ctx context.Context, collection orb.Collection, zoom maptile.Zoom) (int64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("coverage context is required")
	}
	if zoom > 30 {
		return 0, fmt.Errorf("tile zoom %d is outside 0..30", zoom)
	}
	if err := validateGeometry(ctx, collection); err != nil {
		return 0, err
	}
	return countGeometry(ctx, collection, zoom)
}

func countGeometry(ctx context.Context, geometry orb.Geometry, zoom maptile.Zoom) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var count int64
	add := func(n int64) error {
		if n > math.MaxInt64-count {
			return fmt.Errorf("tile coverage count exceeds int64")
		}
		count += n
		return nil
	}
	switch g := geometry.(type) {
	case nil:
	case orb.Point:
		return 1, nil
	case orb.MultiPoint, orb.LineString, orb.MultiLineString:
		err := walkGeometry(ctx, g, zoom, func(maptile.Tile) error { return add(1) })
		return count, err
	case orb.Ring:
		return countPolygon(ctx, orb.Polygon{g}, zoom)
	case orb.Polygon:
		return countPolygon(ctx, g, zoom)
	case orb.MultiPolygon:
		for _, polygon := range g {
			n, err := countPolygon(ctx, polygon, zoom)
			if err != nil {
				return 0, err
			}
			if err := add(n); err != nil {
				return 0, err
			}
		}
	case orb.Collection:
		for _, child := range g {
			n, err := countGeometry(ctx, child, zoom)
			if err != nil {
				return 0, err
			}
			if err := add(n); err != nil {
				return 0, err
			}
		}
	case orb.Bound:
		lo, hi := gridAt(g.Min, zoom), gridAt(g.Max, zoom)
		return (int64(hi.X) - int64(lo.X) + 1) * (int64(lo.Y) - int64(hi.Y) + 1), nil
	default:
		return 0, fmt.Errorf("unsupported coverage geometry %T", geometry)
	}
	return count, ctx.Err()
}

func polygonBoundary(ctx context.Context, p orb.Polygon, zoom maptile.Zoom) (maptile.Set, [][2]uint32, error) {
	set := make(maptile.Set)
	var intersections [][2]uint32
	for _, r := range p {
		ring, err := lineWalk(ctx, orb.LineString(r), zoom, make([][2]uint32, 0), func(tile maptile.Tile) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			set[tile] = true
			if len(set) > maxBoundaryTiles {
				return fmt.Errorf("polygon boundary exceeds memory budget; split the region or reduce zoom")
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		for i := range ring {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			prev, next := (i+len(ring)-1)%len(ring), (i+1)%len(ring)
			y := ring[i][1]
			if (y > ring[prev][1] || y > ring[next][1]) && (y < ring[prev][1] || y < ring[next][1]) && y != ring[next][1] {
				intersections = append(intersections, ring[i])
				if len(intersections) > maxBoundaryTiles {
					return nil, nil, fmt.Errorf("polygon intersections exceed memory budget; split the region or reduce zoom")
				}
			}
		}
	}
	if len(set) == 0 {
		return nil, nil, fmt.Errorf("polygon has no nondegenerate boundary")
	}
	sort.Slice(intersections, func(i, j int) bool {
		if intersections[i][1] != intersections[j][1] {
			return intersections[i][1] < intersections[j][1]
		}
		return intersections[i][0] < intersections[j][0]
	})
	if len(intersections)%2 != 0 {
		return nil, nil, fmt.Errorf("polygon has unpaired scanline intersections")
	}
	for i := 0; i < len(intersections); i += 2 {
		if intersections[i][1] != intersections[i+1][1] {
			return nil, nil, fmt.Errorf("polygon has unpaired scanline intersections")
		}
	}
	return set, intersections, ctx.Err()
}

func countPolygon(ctx context.Context, p orb.Polygon, zoom maptile.Zoom) (int64, error) {
	set, intersections, err := polygonBoundary(ctx, p, zoom)
	if err != nil {
		return 0, err
	}
	count := int64(len(set))
	for i := 0; i < len(intersections); i += 2 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		width := int64(intersections[i+1][0]) - int64(intersections[i][0]) - 1
		if width > 0 {
			count += width
		}
	}
	return count, nil
}

func polygonWalkContext(ctx context.Context, p orb.Polygon, zoom maptile.Zoom, emit func(maptile.Tile) error) error {
	set, intersections, err := polygonBoundary(ctx, p, zoom)
	if err != nil {
		return err
	}
	for tile := range set {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(tile); err != nil {
			return err
		}
	}
	for i := 0; i < len(intersections); i += 2 {
		for x := intersections[i][0] + 1; x < intersections[i+1][0]; x++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := emit(maptile.New(x, intersections[i][1], zoom)); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
