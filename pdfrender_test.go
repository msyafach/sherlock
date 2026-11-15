package main

import (
	"os"
	"testing"
)

// SHERLOCK_TEST_PDF=<path> go test -run TestRenderPDFPage1 -v
func TestRenderPDFPage1(t *testing.T) {
	p := os.Getenv("SHERLOCK_TEST_PDF")
	if p == "" {
		t.Skip("SHERLOCK_TEST_PDF not set")
	}
	img, err := renderPDFPage1(p, 800)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		t.Fatalf("empty image %v", b)
	}
	t.Logf("rendered %dx%d", b.Dx(), b.Dy())
	if out := os.Getenv("SHERLOCK_TEST_OUT"); out != "" {
		data, _ := encodeJPEG(img, 85)
		os.WriteFile(out, data, 0o644)
	}
}
