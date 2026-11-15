package main

// Page-1 previews for Office files, rendered by the user's own Microsoft Office over COM.
// Word and Excel export page 1 to a temporary PDF (then PDFium renders it); PowerPoint exports
// slide 1 straight to PNG. All Office calls run on one locked OS thread, as COM requires.

import (
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

var officeApp = map[string]string{
	".docx": "Word", ".doc": "Word", ".rtf": "Word", ".odt": "Word",
	".xlsx": "Excel", ".xlsm": "Excel", ".xls": "Excel", ".ods": "Excel",
	".pptx": "PowerPoint", ".ppt": "PowerPoint", ".odp": "PowerPoint",
}

type officeJob struct {
	path  string
	reply chan officeResult
}

type officeResult struct {
	img image.Image
	err error
}

type officeWorker struct {
	jobs    chan officeJob
	tmpDir  string
	apps    map[string]*ole.IDispatch
	missing map[string]bool // Office app not installed / failed to start
	stuck   atomic.Bool     // Office stopped answering (e.g. a hidden dialog): stop sending it work
}

var office *officeWorker

func startOffice(tmpDir string) {
	os.MkdirAll(tmpDir, 0o755)
	office = &officeWorker{jobs: make(chan officeJob), tmpDir: tmpDir, apps: map[string]*ole.IDispatch{}, missing: map[string]bool{}}
	go office.loop()
}

// renderOfficePage1 asks the Office worker for a page-1 image; it gives up after 60 seconds.
func renderOfficePage1(path string) (image.Image, error) {
	if office == nil || office.stuck.Load() {
		return nil, errors.New("office tidak aktif")
	}
	if _, ok := officeApp[strings.ToLower(filepath.Ext(path))]; !ok {
		return nil, errors.New("bukan file office")
	}
	reply := make(chan officeResult, 1)
	select {
	case office.jobs <- officeJob{path, reply}:
	case <-time.After(60 * time.Second):
		return nil, errors.New("office sibuk")
	}
	select {
	case r := <-reply:
		return r.img, r.err
	case <-time.After(60 * time.Second):
		office.stuck.Store(true)
		return nil, errors.New("office tidak merespons")
	}
}

// stopOffice closes the Office apps Sherlock started (called on exit).
func stopOffice() {
	if office == nil || office.stuck.Load() {
		return
	}
	reply := make(chan officeResult, 1)
	select {
	case office.jobs <- officeJob{"", reply}:
		select {
		case <-reply:
		case <-time.After(10 * time.Second):
		}
	case <-time.After(2 * time.Second):
	}
}

func (w *officeWorker) loop() {
	runtime.LockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE = already initialised
			for j := range w.jobs {
				j.reply <- officeResult{err: err}
			}
		}
	}
	idle := time.NewTimer(time.Hour)
	for {
		select {
		case j := <-w.jobs:
			if j.path == "" { // stop request
				w.quitAll()
				j.reply <- officeResult{}
				continue
			}
			img, err := w.render(j.path)
			j.reply <- officeResult{img, err}
			idle.Reset(90 * time.Second)
		case <-idle.C:
			w.quitAll() // free Office's memory when nobody is browsing previews
		}
	}
}

func (w *officeWorker) app(name string) (*ole.IDispatch, error) {
	if a := w.apps[name]; a != nil {
		return a, nil
	}
	if w.missing[name] {
		return nil, fmt.Errorf("%s tidak tersedia", name)
	}
	unk, err := oleutil.CreateObject(name + ".Application")
	if err != nil {
		w.missing[name] = true
		return nil, err
	}
	a, err := unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if err != nil {
		w.missing[name] = true
		return nil, err
	}
	switch name {
	case "Word":
		oleutil.PutProperty(a, "Visible", false)
		oleutil.PutProperty(a, "DisplayAlerts", 0) // wdAlertsNone
	case "Excel":
		oleutil.PutProperty(a, "Visible", false)
		oleutil.PutProperty(a, "DisplayAlerts", false)
		oleutil.PutProperty(a, "ScreenUpdating", false)
	}
	w.apps[name] = a
	return a, nil
}

// quitAll closes Office apps Sherlock started, but never one the user still has documents open in.
func (w *officeWorker) quitAll() {
	for name, a := range w.apps {
		coll := map[string]string{"Word": "Documents", "Excel": "Workbooks", "PowerPoint": "Presentations"}[name]
		busy := true
		if v, err := oleutil.GetProperty(a, coll); err == nil {
			d := v.ToIDispatch()
			if c, err := oleutil.GetProperty(d, "Count"); err == nil && c.Val == 0 {
				busy = false
			}
			d.Release()
		}
		if !busy {
			oleutil.CallMethod(a, "Quit")
		}
		a.Release()
		delete(w.apps, name)
	}
}

func (w *officeWorker) render(path string) (img image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			img, err = nil, fmt.Errorf("office panic: %v", r)
		}
	}()
	name := officeApp[strings.ToLower(filepath.Ext(path))]
	a, err := w.app(name)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(w.tmpDir, "p")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	switch name {
	case "Word":
		return w.word(a, path, filepath.Join(tmp, "p.pdf"))
	case "Excel":
		return w.excel(a, path, filepath.Join(tmp, "p.pdf"))
	default:
		return w.powerpoint(a, path, filepath.Join(tmp, "p.png"))
	}
}

func dispatch(v *ole.VARIANT, err error) (*ole.IDispatch, error) {
	if err != nil {
		return nil, err
	}
	d := v.ToIDispatch()
	if d == nil {
		return nil, errors.New("objek office kosong")
	}
	return d, nil
}

func (w *officeWorker) word(app *ole.IDispatch, path, out string) (image.Image, error) {
	docs, err := dispatch(oleutil.GetProperty(app, "Documents"))
	if err != nil {
		w.forget("Word")
		return nil, err
	}
	defer docs.Release()
	// Open(FileName, ConfirmConversions, ReadOnly, AddToRecentFiles, PasswordDocument):
	// a dummy password makes protected files fail instead of popping up a prompt.
	doc, err := dispatch(oleutil.CallMethod(docs, "Open", path, false, true, false, "sherlock-no-password"))
	if err != nil {
		return nil, err
	}
	defer doc.Release()
	defer oleutil.CallMethod(doc, "Close", 0) // wdDoNotSaveChanges
	// ExportAsFixedFormat(OutputFileName, wdExportFormatPDF, OpenAfterExport, OptimizeFor, Range=wdExportFromTo, From, To)
	if _, err := oleutil.CallMethod(doc, "ExportAsFixedFormat", out, 17, false, 1, 3, 1, 1); err != nil {
		return nil, err
	}
	return renderPDFPage1(out, thumbSide*2)
}

func (w *officeWorker) excel(app *ole.IDispatch, path, out string) (image.Image, error) {
	books, err := dispatch(oleutil.GetProperty(app, "Workbooks"))
	if err != nil {
		w.forget("Excel")
		return nil, err
	}
	defer books.Release()
	// Open(FileName, UpdateLinks=0, ReadOnly=true, Format, Password)
	wb, err := dispatch(oleutil.CallMethod(books, "Open", path, 0, true, nil, "sherlock-no-password"))
	if err != nil {
		return nil, err
	}
	defer wb.Release()
	defer oleutil.CallMethod(wb, "Close", false)
	// ExportAsFixedFormat(xlTypePDF, Filename, Quality, IncludeDocProperties, IgnorePrintAreas, From, To)
	if _, err := oleutil.CallMethod(wb, "ExportAsFixedFormat", 0, out, 0, false, false, 1, 1); err != nil {
		return nil, err
	}
	return renderPDFPage1(out, thumbSide*2)
}

func (w *officeWorker) powerpoint(app *ole.IDispatch, path, out string) (image.Image, error) {
	pres, err := dispatch(oleutil.GetProperty(app, "Presentations"))
	if err != nil {
		w.forget("PowerPoint")
		return nil, err
	}
	defer pres.Release()
	// Open(FileName, ReadOnly=msoTrue, Untitled=msoFalse, WithWindow=msoFalse)
	p, err := dispatch(oleutil.CallMethod(pres, "Open", path+"::sherlock-no-password::", -1, 0, 0))
	if err != nil {
		p, err = dispatch(oleutil.CallMethod(pres, "Open", path, -1, 0, 0))
		if err != nil {
			return nil, err
		}
	}
	defer p.Release()
	defer oleutil.CallMethod(p, "Close")
	slides, err := dispatch(oleutil.GetProperty(p, "Slides"))
	if err != nil {
		return nil, err
	}
	defer slides.Release()
	s, err := dispatch(oleutil.CallMethod(slides, "Item", 1))
	if err != nil {
		return nil, err
	}
	defer s.Release()
	if _, err := oleutil.CallMethod(s, "Export", out, "PNG", 1280, 720); err != nil {
		return nil, err
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// forget drops a cached app object (e.g. the user closed that Office app), so the next job restarts it.
func (w *officeWorker) forget(name string) {
	if a := w.apps[name]; a != nil {
		a.Release()
		delete(w.apps, name)
	}
}
