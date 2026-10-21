package cover

import (
	"context"
	"errors"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
)

func TestAuditCoverageWorldEdgesStayInsideTileMatrix(t *testing.T) {
	for _, z := range []maptile.Zoom{0, 1, 6} {
		for _, geometry := range []orb.Geometry{
			orb.Point{180, 90},
			orb.LineString{{170, 80}, {180, -90}},
			orb.Bound{Min: orb.Point{-180, -90}, Max: orb.Point{180, 90}},
			orb.Polygon{{{-180, -90}, {-180, 90}, {180, 90}, {180, -90}, {-180, -90}}},
		} {
			var visits int64
			err := Walk(context.Background(), orb.Collection{geometry}, z, func(tile maptile.Tile) error {
				visits++
				if !tile.Valid() {
					t.Errorf("%T at z%d emitted invalid tile %+v", geometry, z, tile)
				}
				return nil
			})
			count, countErr := Count(orb.Collection{geometry}, z)
			if err != nil || countErr != nil || count != visits {
				t.Errorf("%T at z%d count=%d visits=%d errors=%v/%v", geometry, z, count, visits, err, countErr)
			}
		}
	}
}

// This context cancels after a deterministic number of checks, including the
// polygon boundary preparation that happens before the first tile is visited.
type auditCancelContext struct {
	context.Context
	checks int
}

func (c *auditCancelContext) Err() error {
	c.checks--
	if c.checks <= 0 {
		return context.Canceled
	}
	return nil
}

func TestAuditCoverageCountPreparationIsCancelable(t *testing.T) {
	polygon := orb.Polygon{{{-179, -80}, {-179, 80}, {179, 80}, {179, -80}, {-179, -80}}}
	ctx := &auditCancelContext{Context: context.Background(), checks: 30}
	if _, err := CountContext(ctx, orb.Collection{polygon}, 20); !errors.Is(err, context.Canceled) {
		t.Fatalf("count boundary did not cancel: %v", err)
	}
	ctx = &auditCancelContext{Context: context.Background(), checks: 30}
	visits := 0
	err := Walk(ctx, orb.Collection{polygon}, 20, func(maptile.Tile) error { visits++; return nil })
	if !errors.Is(err, context.Canceled) || visits != 0 {
		t.Fatalf("walk boundary did not cancel before visiting: %v, visits %d", err, visits)
	}
}

func TestAuditCoverageCountLargeBoundWithoutEnumeration(t *testing.T) {
	count, err := CountContext(context.Background(), orb.Collection{orb.Bound{Min: orb.Point{-180, -90}, Max: orb.Point{180, 90}}}, 30)
	if err != nil || count != int64(1)<<60 {
		t.Fatalf("world count=%d error=%v", count, err)
	}
}

func TestAuditCoverageNestedCollectionsAndDegenerateLines(t *testing.T) {
	line := orb.LineString{{12, 34}, {12, 34}}
	count, err := Count(orb.Collection{line}, 4)
	if err != nil || count != 1 {
		t.Fatalf("zero-length line count=%d error=%v", count, err)
	}
	var geometry orb.Geometry = orb.Point{0, 0}
	for i := 0; i < 140; i++ {
		geometry = orb.Collection{geometry}
	}
	if _, err := Count(orb.Collection{geometry}, 4); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
}

func FuzzAuditCoverageCountMatchesWalk(f *testing.F) {
	f.Add([]byte{0, 0, 255, 255, 6, 1, 127, 50})
	f.Add([]byte{128, 128, 128, 128, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 8 {
			return
		}
		point := func(x, y byte) orb.Point { return orb.Point{float64(x)*360/255 - 180, float64(y)*180/255 - 90} }
		a, b, c := point(data[0], data[1]), point(data[2], data[3]), point(data[6], data[7])
		z := maptile.Zoom(data[4] % 7)
		var g orb.Geometry
		switch data[5] % 4 {
		case 0:
			g = orb.LineString{a, b, c}
		case 1:
			g = orb.Polygon{{a, b, c, a}}
		case 2:
			g = orb.MultiLineString{{a, b, c}, {c, b, a}}
		default:
			g = orb.MultiPoint{a, b, c, a}
		}
		var visits int64
		walkErr := Walk(context.Background(), orb.Collection{g}, z, func(tile maptile.Tile) error {
			if !tile.Valid() {
				t.Fatalf("invalid tile %v from %v", tile, g)
			}
			visits++
			return nil
		})
		count, countErr := Count(orb.Collection{g}, z)
		if (walkErr == nil) != (countErr == nil) || (walkErr == nil && count != visits) {
			t.Fatalf("count %d/%v vs walk %d/%v", count, countErr, visits, walkErr)
		}
	})
}
