package media

import (
	"errors"
	"os"
	"testing"
)

// TestWebPInfo reads files written by libwebp (testdata) in each layout.
func TestWebPInfo(t *testing.T) {
	for _, tc := range []struct {
		file     string
		w, h     uint32
		animated bool
	}{
		{"lossy.webp", 512, 512, false},
		{"lossless.webp", 100, 80, false},
		{"alpha.webp", 320, 200, false},
		{"animated.webp", 64, 48, true},
	} {
		data, err := os.ReadFile("testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		w, h, anim, err := WebPInfo(data)
		if err != nil || w != tc.w || h != tc.h || anim != tc.animated {
			t.Errorf("%s: %dx%d animated=%v err=%v; want %dx%d animated=%v", tc.file, w, h, anim, err, tc.w, tc.h, tc.animated)
		}
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01")
	for _, bad := range [][]byte{nil, png, []byte("RIFF\x10\x00\x00\x00WEBPVP8 \xff\xff\xff\xff")} {
		if _, _, _, err := WebPInfo(bad); !errors.Is(err, ErrNotWebP) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
