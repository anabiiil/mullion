//go:build ignore

// winicon.go renders Mullion's Windows icons in the app-icon style the
// macOS icon uses: a midnight tile with the coral mark set into it, the
// mullion knocked out to the tile. The bare coral square looked nothing
// like the app icon (or Windows' other tile icons) in the taskbar,
// Explorer and the notification area.
//
//	go run tools/brand/winicon.go <ico-png-dir> <repo-root>
//
// Writes <ico-png-dir>/icon-<N>.png (the frames icons.go packs into
// mullion.ico and the .syso) and internal/ui/favicon.png (the panel's
// app-window icon). Pure Go, so it runs on Windows as well as macOS.
// Sizes up to 32px snap every edge to the pixel grid so they stay crisp.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

var sizes = []int{16, 24, 32, 48, 64, 128, 256}

// Brand colours, as in assets/brand and render.swift.
var (
	tileTop    = rgb(0x2A3349)
	tileBottom = rgb(0x161C28)
	coral      = rgb(0xFF6B4A)
	midnight   = rgb(0x1C2333)
)

type fcolor struct{ r, g, b float64 }

func rgb(hex uint32) fcolor {
	return fcolor{float64(hex>>16&0xff) / 255, float64(hex>>8&0xff) / 255, float64(hex&0xff) / 255}
}

func (c fcolor) over(d fcolor, a float64) fcolor {
	return fcolor{c.r*a + d.r*(1-a), c.g*a + d.g*(1-a), c.b*a + d.b*(1-a)}
}

type rrect struct{ x0, y0, x1, y1, r float64 }

func (q rrect) has(x, y float64) bool {
	if x < q.x0 || x > q.x1 || y < q.y0 || y > q.y1 {
		return false
	}
	dx := math.Max(math.Max(q.x0+q.r-x, x-(q.x1-q.r)), 0)
	dy := math.Max(math.Max(q.y0+q.r-y, y-(q.y1-q.r)), 0)
	return dx*dx+dy*dy <= q.r*q.r
}

type layout struct {
	tile, inner, mark, bar, transom rrect
	transomAlpha                    float64
	hairline                        bool
}

func geometry(px int) layout {
	s := float64(px)
	var l layout
	if px <= 32 {
		// Pixel-aligned: whole-pixel tile, an even-sized mark so the
		// 2px mullion sits dead centre, a 1px transom.
		margin := 1.0
		if px == 16 {
			margin = 0
		}
		ts := s - 2*margin
		side := 2 * math.Round(ts*0.62/2)
		m0 := (s - side) / 2
		l.tile = rrect{margin, margin, s - margin, s - margin, ts * 0.225}
		l.mark = rrect{m0, m0, m0 + side, m0 + side, side * 6.5 / 24}
		l.bar = rrect{s/2 - 1, m0, s/2 + 1, m0 + side, 0}
		ty := m0 + math.Round(side*8.5/24)
		l.transom = rrect{m0, ty, s/2 - 1, ty + 1, 0}
		l.transomAlpha = 0.5
		return l
	}
	margin := s / 32
	if px <= 64 {
		margin = math.Round(margin) // a whole-pixel edge, no grey fringe
	}
	ts := s - 2*margin
	side := ts * 0.62
	m0 := (s - side) / 2
	u := side / 24 // one unit of the 24-unit mark grid
	l.tile = rrect{margin, margin, s - margin, s - margin, ts * 0.225}
	hw := s / 128 // top-edge highlight width, as on the macOS icon
	l.inner = rrect{margin + hw, margin + hw, s - margin - hw, s - margin - hw, ts*0.225 - hw}
	l.hairline = px >= 128 // thinner than a pixel below that: just a grey fringe
	l.mark = rrect{m0, m0, m0 + side, m0 + side, 6.5 * u}
	l.bar = rrect{m0 + 10.75*u, m0, m0 + 13.25*u, m0 + side, 0}
	l.transom = rrect{m0, m0 + 8.5*u, m0 + 10.75*u, m0 + 10.5*u, 0}
	l.transomAlpha = 0.34
	if px <= 64 {
		// Still small enough for a half-covered pixel column to read as
		// a blurry mullion: snap the bars to whole pixels.
		snap := func(q *rrect) { q.x0, q.y0, q.x1, q.y1 = math.Round(q.x0), math.Round(q.y0), math.Round(q.x1), math.Round(q.y1) }
		snap(&l.bar)
		snap(&l.transom)
		l.transom.x1 = l.bar.x0
	}
	return l
}

// shade is the colour of the icon at one sample point, and whether the
// point is on the icon at all.
func (l layout) shade(x, y float64) (fcolor, bool) {
	if !l.tile.has(x, y) {
		return fcolor{}, false
	}
	t := (y - l.tile.y0) / (l.tile.y1 - l.tile.y0)
	c := tileBottom.over(tileTop, t)
	if l.hairline && !l.inner.has(x, y) {
		c = fcolor{1, 1, 1}.over(c, 0.07)
	}
	if l.mark.has(x, y) && !l.bar.has(x, y) {
		c = coral
		if l.transom.has(x, y) {
			c = midnight.over(c, l.transomAlpha)
		}
	}
	return c, true
}

func render(px int) *image.NRGBA {
	l := geometry(px)
	img := image.NewNRGBA(image.Rect(0, 0, px, px))
	const ss = 8 // ss×ss samples per pixel
	for py := 0; py < px; py++ {
		for pxl := 0; pxl < px; pxl++ {
			var sum fcolor
			var cover float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(pxl) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if c, ok := l.shade(x, y); ok {
						sum.r, sum.g, sum.b = sum.r+c.r, sum.g+c.g, sum.b+c.b
						cover++
					}
				}
			}
			if cover == 0 {
				continue
			}
			to8 := func(v float64) uint8 { return uint8(math.Round(math.Min(v/cover, 1) * 255)) }
			img.SetNRGBA(pxl, py, color.NRGBA{to8(sum.r), to8(sum.g), to8(sum.b), uint8(math.Round(cover / (ss * ss) * 255))})
		}
	}
	return img
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	check(err)
	check(png.Encode(f, img))
	check(f.Close())
	fmt.Println("wrote", path)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run tools/brand/winicon.go <ico-png-dir> <repo-root>")
		os.Exit(2)
	}
	dir, root := os.Args[1], os.Args[2]
	check(os.MkdirAll(dir, 0o755))
	for _, px := range sizes {
		img := render(px)
		writePNG(filepath.Join(dir, fmt.Sprintf("icon-%d.png", px)), img)
		if px == 256 {
			writePNG(filepath.Join(root, "internal", "ui", "favicon.png"), img)
		}
	}
}
