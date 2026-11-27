// Command gen draws the menu bar icon's image files from the SeaTime mark
// (a disc with a wave across it) and writes them next to itself:
//
//	go run ./internal/client/tray/icons/gen internal/client/tray/icons
//
//	menubar.png  macOS template image: black with the wave cut out, 32 px
//	             (the menu bar tints it for light and dark)
//	tray.png     Linux: the mark in colour, 48 px
//	tray.ico     Windows: the mark in colour at 32, 48 and 64 px
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// The mark on a 32-unit square, as on the website: a disc of radius 15 and
// the path M5 18 q5.5 -5 11 0 t11 0.
var wave = flatten([][3][2]float64{
	{{5, 18}, {10.5, 13}, {16, 18}},
	{{16, 18}, {21.5, 23}, {27, 18}},
})

// flatten turns quadratic curves into a line of points.
func flatten(qs [][3][2]float64) [][2]float64 {
	var pts [][2]float64
	for _, q := range qs {
		for i := 0; i <= 32; i++ {
			t := float64(i) / 32
			a, b, c := (1-t)*(1-t), 2*t*(1-t), t*t
			pts = append(pts, [2]float64{a*q[0][0] + b*q[1][0] + c*q[2][0], a*q[0][1] + b*q[1][1] + c*q[2][1]})
		}
	}
	return pts
}

// onWave reports whether (x, y) is within half a stroke of the wave.
func onWave(x, y, stroke float64) bool {
	r2 := stroke * stroke / 4
	for i := 1; i < len(wave); i++ {
		ax, ay, bx, by := wave[i-1][0], wave[i-1][1], wave[i][0], wave[i][1]
		dx, dy := bx-ax, by-ay
		t := 0.0
		if l := dx*dx + dy*dy; l > 0 {
			t = math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/l))
		}
		px, py := ax+t*dx-x, ay+t*dy-y
		if px*px+py*py <= r2 {
			return true
		}
	}
	return false
}

var (
	hi  = [3]float64{0x8f, 0xd3, 0xf3}
	mid = [3]float64{0x3c, 0xb4, 0xe6}
	lo  = [3]float64{0x1e, 0x9b, 0xd4}
)

// shade is the disc's colour at height y (0 top, 1 bottom).
func shade(y float64) [3]float64 {
	a, b, t := hi, mid, y/0.55
	if y > 0.55 {
		a, b, t = mid, lo, (y-0.55)/0.45
	}
	return [3]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t}
}

// draw renders the mark at size px. template: black, the wave cut out.
func draw(size int, template bool, stroke float64) *image.NRGBA {
	const ss = 8 // samples per pixel side
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * 32 / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) * 32 / float64(size)
					if (x-16)*(x-16)+(y-16)*(y-16) > 15*15 {
						continue
					}
					w := onWave(x, y, stroke)
					switch {
					case template && w:
					case template:
						a++
					case w:
						r, g, b, a = r+255, g+255, b+255, a+1
					default:
						c := shade(y / 32)
						r, g, b, a = r+c[0], g+c[1], b+c[2], a+1
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r/a + 0.5), uint8(g/a + 0.5), uint8(b/a + 0.5), uint8(a/(ss*ss)*255 + 0.5)})
		}
	}
	return img
}

// ico packs images as a Windows icon file (32-bit bitmaps).
func ico(imgs ...*image.NRGBA) []byte {
	var dir, data bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&dir, le, [3]uint16{0, 1, uint16(len(imgs))})
	offset := 6 + 16*len(imgs)
	for _, im := range imgs {
		n := im.Bounds().Dx()
		var b bytes.Buffer
		binary.Write(&b, le, struct {
			Size          uint32
			W, H          int32
			Planes, Bits  uint16
			Comp, ImgSize uint32
			XPPM, YPPM    int32
			Used, Imp     uint32
		}{Size: 40, W: int32(n), H: int32(2 * n), Planes: 1, Bits: 32})
		for y := n - 1; y >= 0; y-- { // bottom row first
			for x := 0; x < n; x++ {
				c := im.NRGBAAt(x, y)
				b.Write([]byte{c.B, c.G, c.R, c.A})
			}
		}
		b.Write(make([]byte, n*((n+31)/32*4))) // the 1-bit mask: all drawn, alpha decides
		binary.Write(&dir, le, struct {
			W, H, Colors, Reserved uint8
			Planes, Bits           uint16
			Size, Offset           uint32
		}{W: uint8(n), H: uint8(n), Planes: 1, Bits: 32, Size: uint32(b.Len()), Offset: uint32(offset)})
		offset += b.Len()
		data.Write(b.Bytes())
	}
	return append(dir.Bytes(), data.Bytes()...)
}

func pngBytes(im image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen DIR")
		os.Exit(2)
	}
	files := map[string][]byte{
		"menubar.png": pngBytes(draw(32, true, 3.4)),
		"tray.png":    pngBytes(draw(48, false, 2.8)),
		"tray.ico":    ico(draw(32, false, 3), draw(48, false, 2.8), draw(64, false, 2.6)),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(os.Args[1], name), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
