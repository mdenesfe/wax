package media

import (
	"encoding/binary"
	"errors"
)

// ErrNotWebP means data is not a WebP image this package can read.
var ErrNotWebP = errors.New("media: not a WebP image")

// WebPInfo reads the canvas size of a WebP image and whether it is
// animated, from its RIFF header (the WebP container specification): a
// simple lossy ("VP8 ") or lossless ("VP8L") image, or an extended one
// ("VP8X") whose flags mark animation. Stickers are WebP images.
func WebPInfo(data []byte) (width, height uint32, animated bool, err error) {
	if len(data) < 20 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, false, ErrNotWebP
	}
	size := binary.LittleEndian.Uint32(data[16:20])
	p := data[20:]
	if uint64(len(p)) < uint64(size) {
		return 0, 0, false, ErrNotWebP
	}
	p = p[:size]
	switch string(data[12:16]) {
	case "VP8 ":
		// Frame tag (3 bytes), start code 9d 01 2a, then 14-bit width and
		// height (the top two bits are scaling).
		if len(p) < 10 || p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
			return 0, 0, false, ErrNotWebP
		}
		width = uint32(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff)
		height = uint32(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff)
	case "VP8L":
		// Signature 0x2f, then width-1 and height-1 in 14 bits each.
		if len(p) < 5 || p[0] != 0x2f {
			return 0, 0, false, ErrNotWebP
		}
		bits := binary.LittleEndian.Uint32(p[1:5])
		width, height = bits&0x3fff+1, (bits>>14)&0x3fff+1
	case "VP8X":
		// Flags, 3 reserved bytes, then canvas width-1 and height-1 in 24
		// bits each. The animation flag is bit 1.
		if len(p) < 10 {
			return 0, 0, false, ErrNotWebP
		}
		animated = p[0]&0x02 != 0
		width = uint32(p[4]) | uint32(p[5])<<8 | uint32(p[6])<<16 + 1
		height = uint32(p[7]) | uint32(p[8])<<8 | uint32(p[9])<<16 + 1
	default:
		return 0, 0, false, ErrNotWebP
	}
	if width == 0 || height == 0 {
		return 0, 0, false, ErrNotWebP
	}
	return width, height, animated, nil
}
