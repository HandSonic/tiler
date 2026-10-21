package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmach/orb"
	"github.com/spf13/viper"
)

func TestBoundsAndZoomMetadataFollowLayers(t *testing.T) {
	configureRegressionOutput(t)
	task := NewTask([]Layer{{Zoom: 6, Collection: orb.Collection{orb.Point{118, 32}}}}, TileMap{Name: "map", URL: "https://example.test/{z}/{x}/{y}", Format: PNG, Min: 0, Max: 11})
	defer task.Cancel()
	if task.initErr != nil {
		t.Fatal(task.initErr)
	}
	if task.Bound() != (orb.Bound{Min: orb.Point{118, 32}, Max: orb.Point{118, 32}}) || task.Center() != (orb.Point{118, 32}) {
		t.Fatalf("bad metadata bounds/center: %v %v", task.Bound(), task.Center())
	}
	if task.Min != 6 || task.Max != 6 {
		t.Fatalf("zoom metadata does not reflect layer: %d-%d", task.Min, task.Max)
	}
}

func TestUnsupportedProjectionHasActionableError(t *testing.T) {
	for _, host := range []string{"t0.tianditu.gov.cn", "T0.TIANDITU.GOV.CN"} {
		err := validateTileURL("https://" + host + "/DataServer?T=vec_c&x={x}&y={y}&l={z}")
		if err == nil || !strings.Contains(err.Error(), "*_w") {
			t.Fatalf("host=%s error=%v", host, err)
		}
	}
	if err := validateTileURL("https://t0.tianditu.gov.cn/DataServer?T=vec_w&x={x}&y={y}&l={z}"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigErrorsAndNoLayers(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	dir := t.TempDir()
	if err := initConf(filepath.Join(dir, "missing.toml")); err == nil {
		t.Fatal("missing config accepted")
	}
	file := filepath.Join(dir, "invalid.toml")
	if err := os.WriteFile(file, []byte("[invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := initConf(file); err == nil {
		t.Fatal("invalid TOML accepted")
	}
	if NewTask(nil, TileMap{}) != nil {
		t.Fatal("empty layers should not produce a runnable task")
	}
}

func TestOutputNamesAreWindowsCompatible(t *testing.T) {
	for _, name := range []string{"CON", "lpt1.txt", "a:b/c", "name. "} {
		got := safeOutputName(name)
		if got == name || strings.ContainsAny(got, `<>:"/\|?*`) || strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
			t.Errorf("invalid normalized name: %q -> %q", name, got)
		}
	}
}
