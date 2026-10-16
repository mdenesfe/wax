package main

import (
	"bytes"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
)

// Pictures for the demo, drawn with the standard library: a person or a
// group silhouette on a gradient for profile pictures, and simple
// landscapes for photos. Each is derived from a seed, so it is stable.

func seed(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// hsl converts hue (degrees), saturation and lightness (0..1) to RGB.
func hsl(h, s, l float64) color.RGBA {
	c := (1 - math.Abs(2*l-1)) * s
	hp := math.Mod(h, 360) / 60
	x := c * (1 - math.Abs(math.Mod(hp, 2)-1))
	var r, g, b float64
	switch {
	case hp < 1:
		r, g = c, x
	case hp < 2:
		r, g = x, c
	case hp < 3:
		g, b = c, x
	case hp < 4:
		g, b = x, c
	case hp < 5:
		r, b = x, c
	default:
		r, b = c, x
	}
	m := l - c/2
	return color.RGBA{uint8((r + m) * 255), uint8((g + m) * 255), uint8((b + m) * 255), 255}
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	f := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 255}
}

// canvas is an image with a few drawing primitives.
type canvas struct{ *image.RGBA }

func newCanvas(w, h int) canvas { return canvas{image.NewRGBA(image.Rect(0, 0, w, h))} }

// gradient fills top to bottom from a to b.
func (c canvas) gradient(a, b color.RGBA) {
	bounds := c.Bounds()
	for y := range bounds.Dy() {
		col := mix(a, b, float64(y)/float64(bounds.Dy()-1))
		for x := range bounds.Dx() {
			c.SetRGBA(x, y, col)
		}
	}
}

// ellipse fills an ellipse with col at opacity alpha (0..1), antialiased.
func (c canvas) ellipse(cx, cy, rx, ry float64, col color.RGBA, alpha float64) {
	b := c.Bounds()
	for y := max(0, int(cy-ry-1)); y < min(b.Dy(), int(cy+ry+2)); y++ {
		for x := max(0, int(cx-rx-1)); x < min(b.Dx(), int(cx+rx+2)); x++ {
			dx, dy := (float64(x)+0.5-cx)/rx, (float64(y)+0.5-cy)/ry
			d := (math.Sqrt(dx*dx+dy*dy) - 1) * math.Min(rx, ry) // distance to the edge, in pixels
			if cover := math.Max(0, math.Min(1, 0.5-d)); cover > 0 {
				c.SetRGBA(x, y, mix(c.RGBAAt(x, y), col, cover*alpha))
			}
		}
	}
}

// ridge fills below a mountain line through the given peaks.
func (c canvas) ridge(base float64, peaks [][2]float64, col color.RGBA) {
	b := c.Bounds()
	w := float64(b.Dx())
	for x := range b.Dx() {
		fx := float64(x) / w
		top := base
		for _, p := range peaks {
			// Each peak is a soft triangle: height p[1] at position p[0].
			h := p[1] * math.Max(0, 1-math.Abs(fx-p[0])*3.2)
			top = math.Min(top, base-h*float64(b.Dy()))
		}
		for y := max(0, int(top)); y < b.Dy(); y++ {
			c.SetRGBA(x, y, col)
		}
	}
}

func (c canvas) png() []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, c)
	return buf.Bytes()
}

func (c canvas) jpeg() []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, c, &jpeg.Options{Quality: 86})
	return buf.Bytes()
}

// personPicture is a profile picture: a light silhouette on a gradient.
func personPicture(key string) []byte {
	s := seed(key)
	hue := float64(s % 360)
	c := newCanvas(256, 256)
	c.gradient(hsl(hue, 0.55, 0.62), hsl(hue+35, 0.6, 0.42))
	white := color.RGBA{255, 255, 255, 255}
	c.ellipse(128, 104, 50, 54, white, 0.85)
	c.ellipse(128, 262, 104, 92, white, 0.85)
	return c.png()
}

// groupPicture shows three silhouettes.
func groupPicture(key string) []byte {
	s := seed(key)
	hue := float64(s % 360)
	c := newCanvas(256, 256)
	c.gradient(hsl(hue, 0.5, 0.58), hsl(hue-30, 0.55, 0.38))
	white := color.RGBA{255, 255, 255, 255}
	for _, p := range [][3]float64{{70, 120, 0.6}, {186, 120, 0.6}, {128, 108, 0.9}} {
		c.ellipse(p[0], p[1], 30, 33, white, p[2])
		c.ellipse(p[0], p[1]+110, 62, 60, white, p[2])
	}
	return c.png()
}

// photo is a landscape: sky, sun and mountain ridges in a palette
// derived from the key.
func photo(key string, w, h int) []byte {
	s := seed(key)
	hue := float64(s % 360)
	c := newCanvas(w, h)
	c.gradient(hsl(hue+200, 0.6, 0.78), hsl(hue+20, 0.75, 0.72))
	fw, fh := float64(w), float64(h)
	c.ellipse(fw*(0.25+float64(s%50)/100), fh*0.38, fh*0.11, fh*0.11, hsl(hue+40, 0.9, 0.88), 0.95)
	c.ridge(fh*0.78, [][2]float64{{0.15, 0.32}, {0.5, 0.42}, {0.85, 0.3}}, hsl(hue+230, 0.25, 0.48))
	c.ridge(fh*0.9, [][2]float64{{0.3, 0.25}, {0.7, 0.2}}, hsl(hue+150, 0.35, 0.32))
	c.ridge(fh, [][2]float64{{0.55, 0.1}}, hsl(hue+140, 0.4, 0.22))
	return c.jpeg()
}
