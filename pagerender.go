package main

// Fallback preview when no Office render or stored thumbnail exists: the document's opening text
// typeset on a white A4-shaped page, so the card still shows what the file is about.

import (
	"image"
	"image/color"
	"image/draw"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	faceOnce            sync.Once
	titleFace, bodyFace font.Face
)

func loadFaces() {
	faceOnce.Do(func() {
		mk := func(ttf []byte, size float64) font.Face {
			f, err := opentype.Parse(ttf)
			if err != nil {
				return nil
			}
			face, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
			return face
		}
		titleFace = mk(gobold.TTF, 26)
		bodyFace = mk(goregular.TTF, 15)
	})
}

func textPage(path string) (image.Image, error) {
	loadFaces()
	ext := strings.ToLower(filepath.Ext(path))
	body := ""
	if plainDocExt[ext] {
		body = plainText(path)
	} else {
		body = officeText(path, ext)
	}
	const w, h, margin = 600, 848, 52
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	ink := image.NewUniform(color.RGBA{40, 37, 35, 255})
	grey := image.NewUniform(color.RGBA{120, 113, 108, 255})

	y := margin + 26
	title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	for i, line := range wrap(title, titleFace, w-2*margin) {
		if i == 2 {
			break
		}
		drawText(img, titleFace, ink, margin, y, line)
		y += 34
	}
	y += 14
	if strings.TrimSpace(body) == "" {
		drawText(img, bodyFace, grey, margin, y, "Isi dokumen tidak dapat dibaca.")
		return img, nil
	}
	for _, para := range strings.Split(body, "\n") {
		para = strings.Join(strings.Fields(para), " ")
		if para == "" {
			continue
		}
		for _, line := range wrap(para, bodyFace, w-2*margin) {
			if y > h-margin {
				return img, nil
			}
			drawText(img, bodyFace, ink, margin, y, line)
			y += 22
		}
		y += 8
	}
	return img, nil
}

func drawText(dst draw.Image, face font.Face, src image.Image, x, y int, s string) {
	if face == nil {
		return
	}
	d := font.Drawer{Dst: dst, Src: src, Face: face, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// wrap breaks s into lines no wider than maxW pixels.
func wrap(s string, face font.Face, maxW int) []string {
	if face == nil {
		return []string{s}
	}
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		try := word
		if cur != "" {
			try = cur + " " + word
		}
		if font.MeasureString(face, try).Ceil() <= maxW || cur == "" {
			cur = try
			continue
		}
		lines = append(lines, cur)
		cur = word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
