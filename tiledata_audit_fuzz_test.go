package main

import (
	"bytes"
	"testing"
)

func FuzzWebPAuditDecode(f *testing.F) {
	f.Add([]byte("RIFF\x0c\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"))
	f.Add([]byte{0x52, 0x49, 0x46, 0x46, 0x1c, 0, 0, 0, 0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x4c, 0x0f, 0, 0, 0, 0x2f, 0x01, 0x40, 0, 0x10, 0x07, 0x10, 0xfd, 0x8f, 0x02, 0x06, 0x22, 0xa2, 0xff, 0x01, 0})
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 64<<10 {
			t.Skip()
		}
		config, _, _, err := webPTileFrame(body)
		if err != nil {
			return
		}
		// Keep the fuzz workload small even when a mutated header declares a
		// legal but large image; the large-frame guard has separate regression.
		if uint64(config.Width)*uint64(config.Height) > 4096 {
			t.Skip()
		}
		got, err := prepareTileData(body, WEBP, false)
		if err == nil && !bytes.Equal(got, body) {
			t.Fatal("validated WebP bytes changed")
		}
	})
}
