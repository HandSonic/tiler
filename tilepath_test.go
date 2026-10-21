package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/paulmach/orb/maptile"
)

func TestTilePathRenderAndParse(t *testing.T) {
	tile := maptile.Tile{Z: 4, X: 3, Y: 5}
	for _, tt := range []struct {
		pattern, expected, parsedFormat string
	}{
		{"", "4/3/5.png", "png"},
		{"{z}/{y}/{x}.{format}", "4/5/3.png", "png"},
		{"{z}/{x}/{-y}.{format}", "4/3/10.png", "png"},
		{"{z}/{-y}/{x}.{format}", "4/10/3.png", "png"},
		{"maps/base/{z}/tile-{x}_{y}.{format}", "maps/base/4/tile-3_5.png", "png"},
		{"tiles/{z}_{x}_{y}.{format}", "tiles/4_3_5.png", "png"},
		{"地图 空间/{z}/{x}/{y}.{format}", "地图 空间/4/3/5.png", "png"},
		{"{z}/{x}/{y}", "4/3/5", ""},
		{"{z}/{x}/{y}.png", "4/3/5.png", ""},
		{"{z}/{x}/{y}/{z}_{x}_{y}.{format}", "4/3/5/4_3_5.png", "png"},
		{"{format}/{z}/{x}/{y}.{format}", "png/4/3/5.png", "png"},
	} {
		t.Run(tt.pattern, func(t *testing.T) {
			got, err := renderTilePath(tt.pattern, tile, "PNG")
			if err != nil || got != tt.expected {
				t.Fatalf("render = %q, %v; want %q", got, err, tt.expected)
			}
			parsed, format, matched, err := parseTilePath(tt.pattern, got)
			if err != nil || !matched || parsed != tile || format != tt.parsedFormat {
				t.Fatalf("parse = %+v, %q, %t, %v", parsed, format, matched, err)
			}
		})
	}
}

func TestTilePathNormalizesDefaultAndFormats(t *testing.T) {
	if normalized, err := normalizeTilePath(""); err != nil || normalized != defaultTilePath {
		t.Fatalf("default = %q, %v", normalized, err)
	}
	for _, format := range []string{"png", "jpg", "jpeg", "pbf", "webp"} {
		tile := maptile.Tile{Z: 0, X: 0, Y: 0}
		path, err := renderTilePath("{z}/{x}/{-y}.{format}", tile, format)
		if err != nil {
			t.Fatal(err)
		}
		parsed, parsedFormat, matched, err := parseTilePath("{z}/{x}/{-y}.{format}", strings.ToUpper(path))
		if err != nil || !matched || parsed != tile || parsedFormat != format {
			t.Fatalf("format %s: %+v %q %t %v", format, parsed, parsedFormat, matched, err)
		}
	}
	tile := maptile.Tile{Z: 30, X: (1 << 30) - 1, Y: (1 << 30) - 1}
	path, err := renderTilePath("{z}/{x}/{-y}.{format}", tile, "pbf")
	if err != nil || path != "30/1073741823/0.pbf" {
		t.Fatalf("highest zoom = %q, %v", path, err)
	}
	if parsed, _, matched, err := parseTilePath("{z}/{x}/{-y}.{format}", path); err != nil || !matched || parsed != tile {
		t.Fatalf("highest zoom did not round trip: %+v %t %v", parsed, matched, err)
	}
}

func TestTilePathRejectsInvalidTemplates(t *testing.T) {
	patterns := []string{
		"{z}/{x}.png", "{z}/{y}.png", "{x}/{y}.png",
		"{z}/{x}/{y}/{-y}.png", "{z}/{x}/{Y}.png", "{z}/{x}/{y}.{ext}",
		"{z}/{x}/{y", "{z}/{x}/y}.png", "{z}/{x}/{{y}}.png",
		"{z}{x}/{y}.png", "{z}/{x}{y}.png", "{z}/{x}0{y}.png",
		"/{z}/{x}/{y}.png", "//host/{z}/{x}/{y}.png", "C:/{z}/{x}/{y}.png",
		`{z}\{x}\{y}.png`, "../{z}/{x}/{y}.png", "a/../{z}/{x}/{y}.png",
		"./{z}/{x}/{y}.png", "{z}//{x}/{y}.png", "{z}/{x}/{y}.png/",
		"CON/{z}/{x}/{y}.png", "con.txt/{z}/{x}/{y}.png", "NUL/{z}/{x}/{y}.png",
		"COM1/{z}/{x}/{y}.png", "LPT9/{z}/{x}/{y}.png", "COM¹/{z}/{x}/{y}.png",
		"COM{x}/{z}/{y}.png", "tail. /{z}/{x}/{y}.png", "tail./{z}/{x}/{y}.png",
		"bad?/{z}/{x}/{y}.png", "bad:/{z}/{x}/{y}.png", "bad|/{z}/{x}/{y}.png",
		"bad*/{z}/{x}/{y}.png", "bad</{z}/{x}/{y}.png", "bad\x00/{z}/{x}/{y}.png",
		strings.Repeat("a", 256) + "/{z}/{x}/{y}.png",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			if _, err := normalizeTilePath(pattern); err == nil {
				t.Fatalf("accepted invalid pattern %q", pattern)
			}
		})
	}
}

func TestTilePathRejectsInvalidRenderedValues(t *testing.T) {
	for _, tile := range []maptile.Tile{
		{Z: 31}, {Z: 0, X: 1}, {Z: 1, Y: 2}, {Z: 30, X: 1 << 30},
	} {
		if _, err := renderTilePath("", tile, "png"); err == nil {
			t.Fatalf("accepted invalid coordinate %+v", tile)
		}
	}
	for _, format := range []string{"", "../../png", "png/extra", "tiff", "{z}"} {
		if _, err := renderTilePath("{z}/{x}/{y}", maptile.Tile{}, format); err == nil {
			t.Fatalf("accepted invalid format %q", format)
		}
	}
	if _, err := renderTilePath(strings.Repeat("a", 254)+"{z}/{x}/{y}", maptile.Tile{Z: 30}, "png"); err == nil {
		t.Fatal("accepted a rendered segment exceeding the Windows limit")
	}
}

func TestTilePathParseUnrelatedAndInvalidCoordinates(t *testing.T) {
	for _, name := range []string{"metadata.json", "1/0/0.txt", "0/0/0.png/extra", "base/1/0/0.png"} {
		if _, _, matched, err := parseTilePath("", name); err != nil || matched {
			t.Fatalf("unrelated name %q: matched=%t error=%v", name, matched, err)
		}
	}
	for _, name := range []string{"31/0/0.png", "0/1/0.png", "1/0/2.png", "-1/0/0.png", "a/0/0.png", "1/0/y.png", "1/0/4294967296.png", "1/../0.png"} {
		if _, _, matched, err := parseTilePath("", name); !matched || err == nil {
			t.Fatalf("invalid coordinate %q: matched=%t error=%v", name, matched, err)
		}
	}
	if _, _, matched, err := parseTilePath("{z}/{x}/{-y}.{format}", "1/0/2.png"); !matched || err == nil {
		t.Fatalf("invalid inverted Y: matched=%t error=%v", matched, err)
	}
	for _, tt := range []struct{ pattern, name string }{
		{"{z}/{x}/{y}/{x}.{format}", "1/0/0/1.png"},
		{"{z}/{x}/{-y}/{-y}.{format}", "1/0/0/1.png"},
		{"{format}/{z}/{x}/{y}.{format}", "png/1/0/0.jpg"},
	} {
		if _, _, matched, err := parseTilePath(tt.pattern, tt.name); !matched || err == nil {
			t.Fatalf("inconsistent repeated value %q: matched=%t error=%v", tt.name, matched, err)
		}
	}
}

func TestTilePathConcurrentTemplateReuse(t *testing.T) {
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			tile := maptile.Tile{Z: 2, X: 3, Y: 1}
			path, err := renderTilePath("cached/{z}/{y}/{x}.{format}", tile, "jpg")
			if err != nil {
				t.Error(err)
				return
			}
			parsed, format, matched, err := parseTilePath("cached/{z}/{y}/{x}.{format}", path)
			if err != nil || !matched || parsed != tile || format != "jpg" {
				t.Errorf("concurrent round trip failed: %+v %q %t %v", parsed, format, matched, err)
			}
		}()
	}
	workers.Wait()
}

func TestTilePathLocalReservedFileConflicts(t *testing.T) {
	for _, tt := range []struct{ pattern, format, reserved string }{
		{"metadata.json/{z}/{x}/{y}.{format}", "png", "metadata.json"},
		{"failures.json/{z}/{x}/{y}.{format}", "png", "failures.json"},
		{"METADATA.JSON/{z}/{x}/{y}.{format}", "png", "metadata.json"},
		{"maps/{z}.json/{x}/{y}.{format}", "jpg", "MAPS/0.JSON"},
		{"{z}.json/{x}/{y}.png", "png", "0.json"},
		{"{z}/{x}/{y}.{format}", "png", "1/2/3.png"},
		{"{z}/{x}/{y}.{format}", "png", "1/2/3.png/failures.json"},
		{"{z}/{x}/{y}.{format}", "png", "1/2"},
		{"{z}/{x}/{-y}.{format}", "png", "1/2/3.PNG"},
		{"{z}/{x}/{y}.{format}", "", "1/2/3.webp"},
		{"{z}/{x}/{y}.{format}", "", "1/2/3.jpeg/failures.json"},
	} {
		t.Run(tt.pattern+":"+tt.reserved, func(t *testing.T) {
			if err := validateTilePathFileConflicts(tt.pattern, tt.format, tt.reserved); err == nil {
				t.Fatalf("accepted reserved-file conflict: %+v", tt)
			}
		})
	}
	for _, tt := range []struct {
		pattern, format string
		reserved        []string
	}{
		{"", "png", []string{"metadata.json", "failures.json"}},
		{"{z}/{y}/{x}.{format}", "pbf", []string{"metadata.json", "failures.json"}},
		{"tiles/{z}/{x}/{-y}.{format}", "jpg", []string{"metadata.json", "failures.json", "tiles/report.json"}},
		{"{z}/{x}/{y}.{format}", "png", []string{"1/2/3.jpg"}},
		{"{z}/{x}/{y}.{format}", "", []string{"1/2/3.txt"}},
		{"{z}/{x}/{y}", "png", []string{"1/2/failures.json"}},
		{"metadata.json-extra/{z}/{x}/{y}.png", "png", []string{"metadata.json"}},
	} {
		if err := validateTilePathFileConflicts(tt.pattern, tt.format, tt.reserved...); err != nil {
			t.Fatalf("rejected unrelated reserved files for %+v: %v", tt, err)
		}
	}
	for _, relative := range []string{"../failures.json", "/failures.json", `a\failures.json`} {
		if err := validateTilePathFileConflicts("", "png", relative); err == nil {
			t.Fatalf("accepted non-relative reserved file %q", relative)
		}
	}
	if err := validateTilePathFileConflicts("", "bad-format", "metadata.json"); err == nil {
		t.Fatal("accepted invalid tile format")
	}
}
