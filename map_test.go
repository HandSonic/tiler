package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuiltinTileMapsReturnUsableTemplatesWithoutEmbeddedCredentials(t *testing.T) {
	maps := GetTileMapList()
	if len(maps) == 0 {
		t.Fatal("no builtin map examples")
	}
	for id, m := range maps {
		if m.ID != id || m.URL == "" {
			t.Errorf("builtin map %d is empty: %+v", id, m)
		}
		for _, token := range []string{"{z}", "{x}", "{y}"} {
			if !strings.Contains(m.URL, token) {
				t.Errorf("builtin map %d lacks %s", id, token)
			}
		}
		u, err := url.Parse(m.URL)
		if err != nil {
			t.Error(err)
			continue
		}
		for key, values := range u.Query() {
			if key == "access_token" || key == "tk" || key == "api_key" {
				for _, value := range values {
					if value != "YOUR_API_TOKEN" {
						t.Errorf("builtin map %d includes a non-placeholder credential", id)
					}
				}
			}
		}
	}
}
