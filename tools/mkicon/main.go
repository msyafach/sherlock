// mkicon draws Sherlock's icon (a magnifier on a dark rounded square) and writes icon.ico + icon.png.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func render(size int) *image.RGBA {
	const ss = 4 // supersampling
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{28, 25, 23, 255}
	fg := color.RGBA{250, 250, 249, 255}
	s := float64(size)
	radius := s * 0.22
	cx, cy, r := s*0.44, s*0.44, s*0.20 // lens
	stroke := s * 0.075
	hx0, hy0 := cx+r*0.72, cy+r*0.72 // handle
	hx1, hy1 := s*0.76, s*0.76
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var bgCov, fgCov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					if inRoundRect(px, py, s, radius) {
						bgCov++
						d := math.Abs(math.Hypot(px-cx, py-cy) - r)
						if d <= stroke/2 || segDist(px, py, hx0, hy0, hx1, hy1) <= stroke*0.62 {
							fgCov++
						}
					}
				}
			}
			n := float64(ss * ss)
			a := bgCov / n
			if a == 0 {
				continue
			}
			t := fgCov / math.Max(bgCov, 1)
			mix := func(b, f uint8) uint8 { return uint8(float64(b)*(1-t) + float64(f)*t) }
			// premultiplied alpha
			img.SetRGBA(x, y, color.RGBA{
				uint8(float64(mix(bg.R, fg.R)) * a), uint8(float64(mix(bg.G, fg.G)) * a),
				uint8(float64(mix(bg.B, fg.B)) * a), uint8(255 * a),
			})
		}
	}
	return img
}

func inRoundRect(x, y, s, r float64) bool {
	qx := math.Max(math.Abs(x-s/2)-(s/2-r), 0)
	qy := math.Max(math.Abs(y-s/2)-(s/2-r), 0)
	return math.Hypot(qx, qy) <= r
}

func segDist(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	t := math.Max(0, math.Min(1, ((px-x0)*dx+(py-y0)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}

func main() {
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var pngs [][]byte
	for _, sz := range sizes {
		var b bytes.Buffer
		png.Encode(&b, render(sz))
		pngs = append(pngs, b.Bytes())
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, sz := range sizes {
		w := uint8(sz)
		if sz == 256 {
			w = 0
		}
		ico.Write([]byte{w, w, 0, 0})
		binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(pngs[i])), uint32(offset)})
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		ico.Write(p)
	}
	os.WriteFile(os.Args[1], ico.Bytes(), 0o644)
	os.WriteFile(os.Args[2], pngs[len(pngs)-1], 0o644)
}
