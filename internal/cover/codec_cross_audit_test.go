package cover

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

func TestCodecCrossAuditHighZoomShortLinesTerminate(t *testing.T) {
	const zoom maptile.Zoom = 30
	const n = float64(uint64(1) << zoom)
	point := func(x, y float64) orb.Point {
		return orb.Point{x/n*360 - 180, math.Atan(math.Sinh(math.Pi*(1-2*y/n))) * 180 / math.Pi}
	}
	base := n / 2
	for _, delta := range []float64{0, math.Nextafter(base, math.Inf(1)) - base, -1, 1} {
		for _, line := range []orb.LineString{
			{point(base+delta, base), point(base+2, base+2)},
			{point(base+2, base+2), point(base+delta, base)},
			{point(base, base), point(base+delta, base)},
			{point(0, 1), point(2, 0)},
			{point(n, n-1), point(n-2, n)},
		} {
			ctx := &auditCancelContext{Context: context.Background(), checks: 200}
			count, err := CountContext(ctx, orb.Collection{line}, zoom)
			if err != nil || count < 1 || count > 12 {
				t.Fatalf("short line did not terminate: count=%d err=%v line=%v", count, err, line)
			}
			var visits int64
			ctx = &auditCancelContext{Context: context.Background(), checks: 200}
			err = Walk(ctx, orb.Collection{line}, zoom, func(tile maptile.Tile) error {
				if !tile.Valid() {
					t.Fatalf("short line emitted invalid tile: %+v", tile)
				}
				visits++
				return nil
			})
			if err != nil || visits != count {
				t.Fatalf("short line count=%d visits=%d err=%v", count, visits, err)
			}
		}
	}
}

func TestCodecCrossAuditCountRejectsAggregateOverflowAndCycles(t *testing.T) {
	world := orb.Bound{Min: orb.Point{-180, -90}, Max: orb.Point{180, 90}}
	collection := make(orb.Collection, 8)
	for i := range collection {
		collection[i] = world
	}
	if _, err := Count(collection, 30); err == nil || !strings.Contains(err.Error(), "int64") {
		t.Fatalf("aggregate count overflow was not rejected: %v", err)
	}
	cycle := make(orb.Collection, 1)
	cycle[0] = cycle
	if _, err := Count(cycle, 1); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("cyclic geometry was not rejected: %v", err)
	}
	if err := Walk(context.Background(), cycle, 1, func(maptile.Tile) error { return nil }); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("cyclic geometry reached the visitor: %v", err)
	}
}
