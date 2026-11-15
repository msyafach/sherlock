package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	_ "image/gif"
	_ "image/png"

	"github.com/ledongthuc/pdf"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const maxTextChars = 20000

var kindByExt = map[string]string{}

func init() {
	add := func(kind string, exts ...string) {
		for _, e := range exts {
			kindByExt["."+e] = kind
		}
	}
	// Only these types are indexed; everything else (code, archives, data files...) is skipped.
	add("gambar", "png", "jpg", "jpeg", "jfif", "webp", "gif", "bmp") // embedded as images
	add("pdf", "pdf")                                                 // text
	add("dokumen", "docx", "doc", "xlsx", "xlsm", "xls", "csv", "pptx", "ppt",
		"rtf", "odt", "ods", "odp", "txt", "md", "epub") // text
	add("audio", "mp3", "wav", "m4a", "flac", "ogg", "aac") // name only
	add("video", "mp4", "mov", "mkv", "avi", "webm", "wmv") // name only
	add("program", "exe", "msi", "msix")                    // name only
}

func kindOf(path string) string {
	return kindByExt[strings.ToLower(filepath.Ext(path))]
}

func indexable(path string) bool { return kindOf(path) != "" }

var plainDocExt = map[string]bool{".txt": true, ".md": true, ".csv": true, ".rtf": true}

// nameOnlyReq embeds just the file name and folder (fallback for anything unreadable).
func nameOnlyReq(path, root string) EmbedReq {
	name := filepath.Base(path)
	rel, _ := filepath.Rel(root, filepath.Dir(path))
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ext == "" {
		ext = "tanpa extension"
	}
	txt := fmt.Sprintf("title: %s | text: Berkas %s bernama %s di folder %s", name, ext, cleanName(name), rel)
	return EmbedReq{Content: Content{Parts: []Part{{Text: txt}}}}
}

// buildReq returns the best request for the file's content, and mode "konten" or "nama".
func buildReq(path, root string, size int64) (req EmbedReq, mode string) {
	defer func() {
		if r := recover(); r != nil {
			req, mode = nameOnlyReq(path, root), "nama"
		}
	}()
	name := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(path))
	media := func(mime string, data []byte) EmbedReq {
		return EmbedReq{Content: Content{Parts: []Part{
			{Text: cleanName(name)},
			{InlineData: &Blob{MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}},
		}}}
	}
	textReq := func(t string) EmbedReq {
		return EmbedReq{Content: Content{Parts: []Part{{Text: "title: " + name + " | text: " + t}}}}
	}
	switch kindOf(path) {
	case "gambar":
		if size > 40<<20 {
			break
		}
		if data, mime, err := prepImage(path, ext, size); err == nil {
			return media(mime, data), "konten"
		}
	case "pdf":
		if size > 200<<20 {
			break
		}
		// Page 1 as an image only: mixing in the text dilutes the vector, so the page's look
		// (layout, logos, stamps, scans) is what gets embedded. Text is only a fallback.
		img, err := renderPDFPage1(path, 1024)
		if err != nil && os.Getenv("SHERLOCK_DEBUG") == "1" {
			fmt.Fprintf(os.Stderr, "render %s: %v\n", name, err)
		}
		if err == nil {
			if jpg, err := encodeJPEG(img, 85); err == nil {
				return EmbedReq{Content: Content{Parts: []Part{
					{InlineData: &Blob{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(jpg)}},
				}}}, "visual"
			}
		}
		if t := strings.TrimSpace(pdfText(path)); t != "" {
			return textReq(t), "konten"
		}
	case "dokumen":
		t := ""
		if plainDocExt[ext] {
			t = plainText(path)
		} else {
			t = officeText(path, ext)
		}
		if strings.TrimSpace(t) != "" {
			return textReq(t), "konten"
		}
	}
	return nameOnlyReq(path, root), "nama"
}

func cleanName(name string) string {
	n := strings.TrimSuffix(name, filepath.Ext(name))
	return strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(n)
}

func prepImage(path, ext string, size int64) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	const maxSide = 1536
	small := cfg.Width <= maxSide && cfg.Height <= maxSide && size <= 3<<20
	if small && (ext == ".png") {
		return data, "image/png", nil
	}
	if small && (ext == ".jpg" || ext == ".jpeg" || ext == ".jfif") {
		return data, "image/jpeg", nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxSide || h > maxSide {
		if w > h {
			h, w = h*maxSide/w, maxSide
		} else {
			w, h = w*maxSide/h, maxSide
		}
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.Draw(dst, dst.Bounds(), image.White, image.Point{}, xdraw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

// pdfText extracts plain text from the first pages of a PDF (empty for scanned/encrypted PDFs).
func pdfText(path string) string {
	f, r, err := pdf.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var sb strings.Builder
	for i := 1; i <= r.NumPage() && i <= 15 && sb.Len() < maxTextChars; i++ {
		func() {
			defer func() { recover() }() // one broken page shouldn't lose the rest
			p := r.Page(i)
			if p.V.IsNull() {
				return
			}
			if t, err := p.GetPlainText(nil); err == nil {
				sb.WriteString(t)
				sb.WriteString("\n")
			}
		}()
	}
	s := spaceRe.ReplaceAllString(sb.String(), " ")
	return truncate(nlRe.ReplaceAllString(s, "\n"))
}

var tagRe = regexp.MustCompile(`<[^>]+>`)
var spaceRe = regexp.MustCompile(`[ \t\r\f\v]+`)
var nlRe = regexp.MustCompile(`\n\s*\n+`)

func xmlToText(s string) string {
	s = strings.NewReplacer("</w:p>", "\n", "</a:p>", "\n", "</si>", "\n", "<w:tab/>", " ").Replace(s)
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = spaceRe.ReplaceAllString(s, " ")
	return nlRe.ReplaceAllString(s, "\n")
}

func officeText(path, ext string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	var targets []string
	for _, f := range zr.File {
		n := f.Name
		switch {
		case ext == ".docx" && (n == "word/document.xml"),
			ext == ".pptx" && strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml"),
			(ext == ".xlsx" || ext == ".xlsm") && (n == "xl/sharedStrings.xml" || n == "xl/workbook.xml"),
			(ext == ".odt" || ext == ".ods" || ext == ".odp") && n == "content.xml",
			ext == ".epub" && (strings.HasSuffix(n, ".xhtml") || strings.HasSuffix(n, ".html")):
			targets = append(targets, n)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return natLess(targets[i], targets[j]) })
	var sb strings.Builder
	for _, t := range targets {
		f, err := zr.Open(t)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(f, 4<<20))
		f.Close()
		sb.WriteString(xmlToText(string(data)))
		sb.WriteString("\n")
		if sb.Len() > maxTextChars*2 {
			break
		}
	}
	return truncate(sb.String())
}

func plainText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	if bytes.IndexByte(data, 0) >= 0 && !utf8.Valid(data) {
		return ""
	}
	s := strings.ToValidUTF8(string(data), "")
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".html" || ext == ".htm" || ext == ".shtml" || ext == ".xml" {
		s = xmlToText(s)
	}
	return truncate(s)
}

func truncate(s string) string { return truncateTo(s, maxTextChars) }

func truncateTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// natLess sorts slide2.xml before slide10.xml.
func natLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
