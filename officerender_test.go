package main

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SHERLOCK_TEST_OFFICE="a.docx;b.pptx;c.xlsx" SHERLOCK_TEST_OUT=<dir> go test -run TestOfficePreview -v
func TestOfficePreview(t *testing.T) {
	files := os.Getenv("SHERLOCK_TEST_OFFICE")
	if files == "" {
		t.Skip("SHERLOCK_TEST_OFFICE not set")
	}
	out := os.Getenv("SHERLOCK_TEST_OUT")
	startOffice(t.TempDir())
	for _, p := range strings.Split(files, ";") {
		start := time.Now()
		img, err := renderOfficePage1(p)
		if err != nil {
			t.Errorf("office %s: %v", filepath.Base(p), err)
		} else {
			t.Logf("office %s: %v in %v", filepath.Base(p), img.Bounds().Size(), time.Since(start).Round(time.Millisecond))
			save(out, "office-"+filepath.Base(p)+".jpg", img)
		}
		tp, err := textPage(p)
		if err != nil {
			t.Errorf("textPage %s: %v", filepath.Base(p), err)
			continue
		}
		save(out, "text-"+filepath.Base(p)+".jpg", tp)
	}
	stopOffice()
}

func save(dir, name string, img image.Image) {
	if dir == "" {
		return
	}
	if data, err := encodeJPEG(img, 85); err == nil {
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
}
