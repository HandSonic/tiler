// Coverage algorithms adapted from atlasdatatech/orb v0.2.2.
// Copyright (c) 2017 Paul Mach. See LICENSE.md for the MIT license.
package cover

import (
	"fmt"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

func Geometry(g orb.Geometry, z maptile.Zoom) maptile.Set {
	if g == nil {
		return nil
	}

	switch g := g.(type) {
	case orb.Point:
		return Point(g, z)
	case orb.MultiPoint:
		return MultiPoint(g, z)
	case orb.LineString:
		return LineString(g, z)
	case orb.MultiLineString:
		return MultiLineString(g, z)
	case orb.Ring:
		return Ring(g, z)
	case orb.Polygon:
		return Polygon(g, z)
	case orb.MultiPolygon:
		return MultiPolygon(g, z)
	case orb.Collection:
		return Collection(g, z)
	case orb.Bound:
		return Bound(g, z)
	}

	panic(fmt.Sprintf("geometry type not supported: %T", g))
}

func GeometryCount(g orb.Geometry, z maptile.Zoom) int64 {
	if g == nil {
		return 0
	}

	switch g := g.(type) {
	case orb.Point:
		return int64(len(Point(g, z)))
	case orb.MultiPoint:
		return int64(len(MultiPoint(g, z)))
	case orb.LineString:
		return LineStringCount(g, z)
	case orb.MultiLineString:
		return MultiLineStringCount(g, z)
	case orb.Ring:
		return RingCount(g, z)
	case orb.Polygon:
		return PolygonCount(g, z)
	case orb.MultiPolygon:
		return MultiPolygonCount(g, z)
	case orb.Collection:
		return CollectionCount(g, z)
	case orb.Bound:
		return BoundCount(g, z)
	}

	panic(fmt.Sprintf("geometry type not supported: %T", g))
}

func Point(ll orb.Point, z maptile.Zoom) maptile.Set {
	return maptile.Set{
		gridAt(ll, z): true,
	}
}

func MultiPoint(mp orb.MultiPoint, z maptile.Zoom) maptile.Set {
	set := make(maptile.Set)
	for _, p := range mp {
		set[gridAt(p, z)] = true
	}

	return set
}

func Bound(b orb.Bound, z maptile.Zoom) maptile.Set {
	lo := gridAt(b.Min, z)
	hi := gridAt(b.Max, z)

	result := make(maptile.Set, (hi.X-lo.X+1)*(lo.Y-hi.Y+1))

	for x := lo.X; x <= hi.X; x++ {
		for y := hi.Y; y <= lo.Y; y++ {
			result[maptile.Tile{X: x, Y: y, Z: z}] = true
		}
	}

	return result
}

func BoundCount(b orb.Bound, z maptile.Zoom) int64 {
	lo := gridAt(b.Min, z)
	hi := gridAt(b.Max, z)

	cnt := int64(hi.X-lo.X+1) * int64(lo.Y-hi.Y+1)

	if cnt == 0 {
		return 1
	}
	return cnt
}

func Collection(c orb.Collection, z maptile.Zoom) maptile.Set {
	set := make(maptile.Set)
	for _, g := range c {
		set.Merge(Geometry(g, z))
	}

	return set
}

func CollectionCount(c orb.Collection, z maptile.Zoom) int64 {
	var cnt int64
	for _, g := range c {
		cnt += GeometryCount(g, z)
	}
	return cnt
}
