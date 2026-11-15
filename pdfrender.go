package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

// PDFium (Chrome's PDF engine) compiled to WebAssembly: renders pages without cgo or extra DLLs.
var (
	pdfOnce sync.Once
	pdfMu   sync.Mutex // one PDFium instance, shared by the indexer and thumbnail requests
	pdfInst pdfium.Pdfium
	pdfErr  error
)

func pdfiumInstance() (pdfium.Pdfium, error) {
	pdfOnce.Do(func() {
		// A windowsgui exe has no console: PDFium's stdout/stderr must go somewhere valid or it fails.
		pool, err := webassembly.Init(webassembly.Config{MinIdle: 1, MaxIdle: 1, MaxTotal: 1, Stdout: io.Discard, Stderr: io.Discard})
		if err != nil {
			pdfErr = err
			return
		}
		pdfInst, pdfErr = pool.GetInstance(60 * time.Second)
	})
	return pdfInst, pdfErr
}

// renderPDFPage1 renders the first page so it fits in maxSide×maxSide, flattened onto white.
func renderPDFPage1(path string, maxSide int) (out image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("pdfium panic: %v", r)
		}
	}()
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > 150<<20 {
		return nil, errors.New("pdf terlalu besar untuk dirender")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pdfMu.Lock()
	defer pdfMu.Unlock()
	inst, err := pdfiumInstance()
	if err != nil {
		return nil, err
	}
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return nil, err
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	r, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page:   requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}},
		Width:  maxSide,
		Height: maxSide,
	})
	if err != nil {
		return nil, err
	}
	defer r.Cleanup()
	src := r.Result.RenderedImage
	if src == nil {
		src = r.Result.Image
	}
	if src == nil {
		return nil, errors.New("render kosong")
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy())) // copy out: the wasm buffer dies on Cleanup
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst, nil
}

func encodeJPEG(img image.Image, q int) ([]byte, error) {
	var buf bytes.Buffer
	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q})
	return buf.Bytes(), err
}
