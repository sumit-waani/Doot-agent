// Command genicons writes the PWA icon set.
//
// The mark is a single accent dot on a dark field: it reads at 16px in a browser
// tab and at home-screen size without depending on a font being available at
// build time.
//
// Run from the repo root:
//
//	go run ./tools/genicons
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const outDir = "internal/web/static/icons"

var (
	bg     = color.NRGBA{R: 0x0b, G: 0x0d, B: 0x10, A: 0xff}
	panel  = color.NRGBA{R: 0x12, G: 0x16, B: 0x1b, A: 0xff}
	accent = color.NRGBA{R: 0x4d, G: 0xd4, B: 0xac, A: 0xff}
)

func main() {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fail(err)
	}

	// "any" icons keep a margin so the mark is not clipped by a browser's own
	// rounding. The maskable icon uses a much smaller mark, because launchers
	// crop to a safe zone of roughly the middle 80%.
	jobs := []struct {
		name      string
		size      int
		dotFrac   float64
		roundedBg bool
	}{
		{"icon-192.png", 192, 0.30, true},
		{"icon-512.png", 512, 0.30, true},
		{"icon-maskable-512.png", 512, 0.22, false},
	}

	for _, j := range jobs {
		img := renderIcon(j.size, j.dotFrac, j.roundedBg)
		path := filepath.Join(outDir, j.name)
		if err := writePNG(path, img); err != nil {
			fail(err)
		}
		fmt.Printf("wrote %s (%dx%d)\n", path, j.size, j.size)
	}
}

func renderIcon(size int, dotFrac float64, roundedBg bool) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	// Maskable icons must bleed to the edges, since the launcher supplies the
	// shape. Non-maskable icons draw their own rounded square.
	if roundedBg {
		draw.Draw(img, img.Bounds(), &image.Uniform{color.NRGBA{}}, image.Point{}, draw.Src)
		fillRoundedRect(img, 0, 0, float64(size), float64(size), float64(size)*0.22, bg)
		inset := float64(size) * 0.07
		fillRoundedRect(img,
			inset, inset, float64(size)-inset, float64(size)-inset,
			float64(size)*0.17, panel)
	} else {
		draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	}

	c := float64(size) / 2
	fillCircle(img, c, c, float64(size)*dotFrac/2, accent)
	return img
}

// fillCircle draws an antialiased disc by supersampling the boundary.
func fillCircle(img *image.NRGBA, cx, cy, r float64, col color.NRGBA) {
	minX := int(math.Floor(cx - r - 1))
	maxX := int(math.Ceil(cx + r + 1))
	minY := int(math.Floor(cy - r - 1))
	maxY := int(math.Ceil(cy + r + 1))

	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			if !image.Pt(x, y).In(img.Bounds()) {
				continue
			}
			if a := coverageCircle(float64(x), float64(y), cx, cy, r); a > 0 {
				blend(img, x, y, col, a)
			}
		}
	}
}

// fillRoundedRect draws an antialiased rounded rectangle.
func fillRoundedRect(img *image.NRGBA, x0, y0, x1, y1, radius float64, col color.NRGBA) {
	for y := int(math.Floor(y0)); y < int(math.Ceil(y1)); y++ {
		for x := int(math.Floor(x0)); x < int(math.Ceil(x1)); x++ {
			if !image.Pt(x, y).In(img.Bounds()) {
				continue
			}
			if a := coverageRoundedRect(float64(x), float64(y), x0, y0, x1, y1, radius); a > 0 {
				blend(img, x, y, col, a)
			}
		}
	}
}

const samples = 4

func coverageCircle(px, py, cx, cy, r float64) float64 {
	var hits int
	for sy := 0; sy < samples; sy++ {
		for sx := 0; sx < samples; sx++ {
			x := px + (float64(sx)+0.5)/samples
			y := py + (float64(sy)+0.5)/samples
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				hits++
			}
		}
	}
	return float64(hits) / float64(samples*samples)
}

func coverageRoundedRect(px, py, x0, y0, x1, y1, radius float64) float64 {
	var hits int
	for sy := 0; sy < samples; sy++ {
		for sx := 0; sx < samples; sx++ {
			x := px + (float64(sx)+0.5)/samples
			y := py + (float64(sy)+0.5)/samples
			if insideRoundedRect(x, y, x0, y0, x1, y1, radius) {
				hits++
			}
		}
	}
	return float64(hits) / float64(samples*samples)
}

func insideRoundedRect(x, y, x0, y0, x1, y1, radius float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}

	// Clamp toward the nearest corner centre; if the point is outside the
	// corner's arc it is outside the shape.
	cx, cy := x, y
	switch {
	case x < x0+radius:
		cx = x0 + radius
	case x > x1-radius:
		cx = x1 - radius
	}
	switch {
	case y < y0+radius:
		cy = y0 + radius
	case y > y1-radius:
		cy = y1 - radius
	}

	if cx == x || cy == y {
		return true // along an edge, not in a corner region
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

func blend(img *image.NRGBA, x, y int, col color.NRGBA, alpha float64) {
	if alpha >= 1 {
		img.SetNRGBA(x, y, col)
		return
	}
	dst := img.NRGBAAt(x, y)
	mix := func(s, d uint8) uint8 {
		return uint8(math.Round(float64(s)*alpha + float64(d)*(1-alpha)))
	}
	img.SetNRGBA(x, y, color.NRGBA{
		R: mix(col.R, dst.R),
		G: mix(col.G, dst.G),
		B: mix(col.B, dst.B),
		A: mix(col.A, dst.A),
	})
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := png.Encoder{CompressionLevel: png.BestCompression}
	return enc.Encode(f, img)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genicons:", err)
	os.Exit(1)
}
