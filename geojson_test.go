package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmach/orb"
)

func writeGeoJSONTestFile(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "region.geojson")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadCollectionFormats(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		want orb.Collection
	}{
		{
			"FeatureCollection",
			`{"type":"FeatureCollection","features":[{"type":"Feature","properties":{},"geometry":{"type":"Point","coordinates":[120,30]}}]}`,
			orb.Collection{orb.Point{120, 30}},
		},
		{
			"Feature",
			`{"type":"Feature","properties":{"name":"region"},"geometry":{"type":"Point","coordinates":[120,30]}}`,
			orb.Collection{orb.Point{120, 30}},
		},
		{
			"bare Geometry",
			`{"type":"Polygon","coordinates":[[[120,30],[121,30],[121,31],[120,30]]]}`,
			orb.Collection{orb.Polygon{orb.Ring{{120, 30}, {121, 30}, {121, 31}, {120, 30}}}},
		},
		{
			"position with elevation",
			`{"type":"Point","coordinates":[120,30,150]}`,
			orb.Collection{orb.Point{120, 30}},
		},
		{
			"longitude latitude boundaries",
			`{"type":"MultiPoint","coordinates":[[-180,-90],[180,90]]}`,
			orb.Collection{orb.MultiPoint{{-180, -90}, {180, 90}}},
		},
		{
			"Nominatim array",
			`[{"place_id":1,"display_name":"region","geojson":{"type":"Point","coordinates":[120,30]}}]`,
			orb.Collection{orb.Point{120, 30}},
		},
		{
			"multiple Nominatim results",
			`[{"geojson":{"type":"Point","coordinates":[120,30]}},{"geojson":{"type":"Point","coordinates":[121,31]}}]`,
			orb.Collection{orb.Point{120, 30}, orb.Point{121, 31}},
		},
		{
			"multiple Features",
			`{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Point","coordinates":[120,30]}},{"type":"Feature","geometry":{"type":"Point","coordinates":[121,31]}}]}`,
			orb.Collection{orb.Point{120, 30}, orb.Point{121, 31}},
		},
		{
			"GeometryCollection",
			`{"type":"GeometryCollection","geometries":[{"type":"Point","coordinates":[120,30]},{"type":"Point","coordinates":[121,31]}]}`,
			orb.Collection{orb.Collection{orb.Point{120, 30}, orb.Point{121, 31}}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readCollection(writeGeoJSONTestFile(t, tt.data))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("collection = %#v; want %#v", got, tt.want)
			}
		})
	}
}

func TestReadCollectionRejectsInvalidInput(t *testing.T) {
	for _, tt := range []struct {
		name, data, message string
	}{
		{"empty file", " \n\t", "empty"},
		{"broken JSON", `{"type":`, "parse GeoJSON"},
		{"null document", `null`, "missing"},
		{"unknown type", `{"type":"NotGeometry"}`, "unsupported"},
		{"empty FeatureCollection", `{"type":"FeatureCollection","features":[]}`, "empty"},
		{"null Feature", `{"type":"FeatureCollection","features":[null]}`, "feature 1"},
		{"null geometry", `{"type":"Feature","geometry":null}`, "geometry"},
		{"empty GeometryCollection", `{"type":"GeometryCollection","geometries":[]}`, "empty"},
		{"invalid geometry child", `{"type":"GeometryCollection","geometries":[null]}`, "geometry 1"},
		{"damaged coordinates", `{"type":"Point","coordinates":"not coordinates"}`, "invalid Point coordinates"},
		{"short coordinates", `{"type":"Point","coordinates":[120]}`, "longitude and latitude"},
		{"null coordinate", `{"type":"Point","coordinates":[null,30]}`, "numbers, not null"},
		{"projected meter coordinates", `{"type":"Point","coordinates":[13500000,3500000]}`, "longitude [-180,180]"},
		{"longitude out of range", `{"type":"Point","coordinates":[181,30]}`, "longitude [-180,180]"},
		{"latitude out of range", `{"type":"Point","coordinates":[120,-91]}`, "latitude [-90,90]"},
		{"empty polygon", `{"type":"Polygon","coordinates":[]}`, "empty"},
		{"empty Nominatim", `[]`, "empty"},
		{"missing Nominatim geojson", `[{"display_name":"region"}]`, "polygon_geojson=1"},
		{"null Nominatim geojson", `[{"geojson":null}]`, "polygon_geojson=1"},
		{"one missing Nominatim geojson", `[{"geojson":{"type":"Point","coordinates":[120,30]}},{"display_name":"missing"}]`, "result 2"},
		{"damaged Nominatim geometry", `[{"geojson":{"type":"Point","coordinates":"invalid"}}]`, "Nominatim result 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeGeoJSONTestFile(t, tt.data)
			got, err := readCollection(path)
			if err == nil || !strings.Contains(err.Error(), tt.message) || !strings.Contains(err.Error(), path) {
				t.Fatalf("readCollection = %#v, %v; want error containing %q and path", got, err, tt.message)
			}
			if got != nil {
				t.Error("invalid input must not return a partially parsed region")
			}
		})
	}
}

func TestReadCollectionMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.geojson")
	if _, err := readCollection(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing file error = %v; want the missing path", err)
	}
}
