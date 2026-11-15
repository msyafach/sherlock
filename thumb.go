package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	fmt.Fprintf(h, "v2|%s|%d|%d", path, st.ModTime().UnixNano(), st.Size())
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

// hasPreview reports whether a page-1 / image preview can be made for this file type.
func hasPreview(path string) bool {
	switch kindOf(path) {
	case "gambar", "pdf":
		return true
	case "dokumen":
		return true // Office render, stored thumbnail, or a typeset text page as the last resort
	}
	return false
}

// officeThumbnail returns the first-page preview Office/LibreOffice store inside the file, if any.
func officeThumbnail(path string) (image.Image, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, name := range []string{"docProps/thumbnail.jpeg", "docProps/thumbnail.jpg", "docProps/thumbnail.png", "Thumbnails/thumbnail.png"} {
		for _, f := range zr.File {
			if !strings.EqualFold(f.Name, name) {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			img, _, err := image.Decode(io.LimitReader(rc, 8<<20))
			rc.Close()
			return img, err
		}
	}
	return nil, errors.New("tidak ada preview tersimpan")
}

func loadPreview(path string) (image.Image, error) {
	switch kindOf(path) {
	case "pdf":
		return renderPDFPage1(path, thumbSide*2)
	case "dokumen":
		if img, err := renderOfficePage1(path); err == nil {
			return img, nil
		}
		if img, err := officeThumbnail(path); err == nil {
			return img, nil
		}
		return textPage(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func makeThumb(path string) ([]byte, error) {
	img, err := loadPreview(path)
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
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
