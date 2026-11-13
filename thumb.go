package main

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
)

const thumbSide = 400

// thumb returns a small JPEG preview of an image, cached on disk by path + modification time.
func (a *App) thumb(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%d", path, st.ModTime().UnixNano(), st.Size())
	dir := filepath.Join(a.dir, "thumbs")
	cache := filepath.Join(dir, fmt.Sprintf("%016x.jpg", h.Sum64()))
	if data, err := os.ReadFile(cache); err == nil {
		return data, nil
	}
	data, err := makeThumb(path)
	if err != nil {
		return nil, err
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(cache, data, 0o644)
	return data, nil
}

func makeThumb(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbSide || h > thumbSide {
		if w > h {
			h, w = max(1, h*thumbSide/w), thumbSide
		} else {
			w, h = max(1, w*thumbSide/h), thumbSide
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.Draw(dst, dst.Bounds(), image.White, image.Point{}, xdraw.Src)
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
