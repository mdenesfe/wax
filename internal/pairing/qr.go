package pairing

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// quietZone is the light border required around a QR code, in modules.
const quietZone = 2

// RenderTerminal draws a QR code with ANSI colours and half-block characters
// (two modules per character cell). Colours are explicit, so the code scans
// on both light and dark terminal themes.
func RenderTerminal(content string) (string, error) {
	code, err := qr.Encode(content, qr.L)
	if err != nil {
		return "", fmt.Errorf("pairing: encode qr: %w", err)
	}
	dark := func(x, y int) bool {
		x, y = x-quietZone, y-quietZone
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	const (
		fgBlack, fgWhite = "\x1b[30m", "\x1b[97m"
		bgBlack, bgWhite = "\x1b[40m", "\x1b[107m"
		reset            = "\x1b[0m"
	)
	size := code.Size + 2*quietZone
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		for x := range size {
			top, bottom := dark(x, y), dark(x, y+1) // dark is false outside the code
			// "▀" paints the upper half with the foreground colour and the
			// lower half with the background colour.
			fg, bg := fgWhite, bgWhite
			if top {
				fg = fgBlack
			}
			if bottom {
				bg = bgBlack
			}
			b.WriteString(fg + bg + "▀")
		}
		b.WriteString(reset + "\n")
	}
	return b.String(), nil
}

// RenderPNG returns the QR code as a PNG image.
func RenderPNG(content string) ([]byte, error) {
	code, err := qr.Encode(content, qr.L)
	if err != nil {
		return nil, fmt.Errorf("pairing: encode qr: %w", err)
	}
	code.Scale = 8
	return code.PNG(), nil
}
