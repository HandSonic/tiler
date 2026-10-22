package cover

import (
	"context"
	"errors"
	"testing"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/maptile"
	upstream "github.com/paulmach/orb/maptile/tilecover"
)

func TestWalkMatchesUpstreamCoverage(t *testing.T) {
	outer := orb.Ring{{70, 0}, {70, 55}, {140, 55}, {140, 0}, {70, 0}}
	hole := orb.Ring{{95, 15}, {95, 35}, {115, 35}, {115, 15}, {95, 15}}
	tests := []struct {
		name     string
		geometry orb.Geometry
	}{
		{"point", orb.Point{118.8, 32.1}},
		{"multipoint", orb.MultiPoint{{118, 32}, {119, 33}, {118, 32}}},
		{"line", orb.LineString{{70, 10}, {100, 40}, {135, 20}}},
		{"multiline", orb.MultiLineString{{{70, 10}, {100, 40}}, {{100, 40}, {135, 20}}}},
		{"ring", outer},
		{"polygon with hole", orb.Polygon{outer, hole}},
		{"nearly rectangular polygon", orb.Polygon{{{70, 0}, {70, 55}, {135, 55}, {140, 0}, {70, 0}}}},
		{"multipolygon", orb.MultiPolygon{{outer}, {{{145, 10}, {145, 20}, {160, 20}, {160, 10}, {145, 10}}}}},
		{"nested collection", orb.Collection{orb.Point{10, 20}, orb.Collection{orb.Polygon{outer}, orb.Point{118, 32}}}},
		{"bound", orb.Bound{Min: orb.Point{70, 0}, Max: orb.Point{140, 55}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collection := orb.Collection{tt.geometry}
			want := upstream.Collection(collection, 6)
			got := make(maptile.Set)
			var visits int64
			err := Walk(context.Background(), collection, 6, func(tile maptile.Tile) error {
				visits++
				got[tile] = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("unique tiles = %d; upstream = %d", len(got), len(want))
			}
			for tile := range want {
				if !got[tile] {
					t.Errorf("missing upstream tile %v", tile)
				}
			}
			count, err := Count(collection, 6)
			if err != nil || count != visits {
				t.Errorf("Count = %d, %v; visits = %d", count, err, visits)
			}
		})
	}
}

func TestIssue13WebMercatorRectangle(t *testing.T) {
	// The rectangle supplied in issue #13 covers these Web Mercator tiles.
	// Its provider's vec_c URL uses a different matrix; polygon and multipolygon
	// parsing themselves must produce the same coverage for vec_w.
	for _, polygon := range []orb.Polygon{
		{{{71.815883, 0.000001}, {71.815883, 53.759980}, {135.668005, 53.759980}, {135.668005, 0.000001}, {71.815883, 0.000001}}},
		// Grid-aligned corners, opposite winding and a different start corner.
		{{{90, 66.51326044311186}, {0, 66.51326044311186}, {0, 0}, {90, 0}, {90, 66.51326044311186}}},
		// A rectangle smaller than a tile still includes its tile once.
		{{{1, 1}, {1.001, 1}, {1.001, 1.001}, {1, 1.001}, {1, 1}}},
	} {
		for _, zoom := range []maptile.Zoom{0, 3, 6, 9} {
			want := upstream.Bound(polygon.Bound(), zoom)
			for _, geometry := range []orb.Geometry{polygon, orb.MultiPolygon{polygon}} {
				got := make(maptile.Set)
				if err := Walk(context.Background(), orb.Collection{geometry}, zoom, func(tile maptile.Tile) error {
					got[tile] = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if len(got) != len(want) {
					t.Fatalf("zoom %d: %T tiles = %d; rectangle = %d", zoom, geometry, len(got), len(want))
				}
				for tile := range want {
					if !got[tile] {
						t.Errorf("zoom %d: missing tile %v", zoom, tile)
					}
				}
			}
		}
	}
}

func TestWalkCancellationAndVisitorErrors(t *testing.T) {
	collection := orb.Collection{orb.Bound{Min: orb.Point{70, 10}, Max: orb.Point{140, 55}}}
	t.Run("cancelled before traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Walk(ctx, collection, 10, func(maptile.Tile) error { t.Fatal("visited after cancellation"); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("cancelled during traversal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		visits := 0
		err := Walk(ctx, collection, 10, func(maptile.Tile) error { visits++; cancel(); return nil })
		if !errors.Is(err, context.Canceled) || visits != 1 {
			t.Fatalf("error = %v, visits = %d", err, visits)
		}
	})
	t.Run("visitor error", func(t *testing.T) {
		want := errors.New("stop saving")
		visits := 0
		err := Walk(context.Background(), collection, 10, func(maptile.Tile) error { visits++; return want })
		if !errors.Is(err, want) || visits != 1 {
			t.Fatalf("error = %v, visits = %d", err, visits)
		}
	})
}

func TestMalformedCoverageReportsErrors(t *testing.T) {
	// The old channel helpers recovered this panic and silently ended a layer.
	malformed := orb.Collection{orb.Polygon{orb.Ring{}}}
	if _, err := Count(malformed, 3); err == nil {
		t.Error("Count should report a malformed polygon")
	}
	if err := Walk(context.Background(), malformed, 3, func(maptile.Tile) error { return nil }); err == nil {
		t.Error("Walk should report a malformed polygon")
	}
	if _, err := Count(nil, 31); err == nil {
		t.Error("Count accepted an unsupported zoom")
	}
	if err := Walk(context.Background(), nil, 31, func(maptile.Tile) error { return nil }); err == nil {
		t.Error("Walk accepted an unsupported zoom")
	}
}
