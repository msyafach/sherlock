package main

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Path    string
	Size    int64
	ModTime int64
	Kind    string
	Mode    string // "konten" atau "nama"
	Vec     []float32
}

// indexVersion 3: PDFs are embedded from an image of page 1 only (re-embed older PDF entries once).
const indexVersion = 3

type Index struct {
	Model   string
	Dim     int
	Version int
	Entries map[string]*Entry
}

type Status struct {
	Running bool   `json:"running"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Current string `json:"current"`
	Message string `json:"message"`
}

type Store struct {
	mu       sync.RWMutex
	path     string
	idx      *Index
	status   Status
	cancel   context.CancelFunc
	excluded []string
}

func LoadStore(path string) *Store {
	s := &Store{path: path, idx: &Index{Version: indexVersion, Entries: map[string]*Entry{}}}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		var idx Index
		if gob.NewDecoder(f).Decode(&idx) == nil && idx.Entries != nil {
			if idx.Version < indexVersion {
				for p, e := range idx.Entries {
					if e.Kind == "pdf" {
						delete(idx.Entries, p)
					}
				}
			}
			idx.Version = indexVersion
			s.idx = &idx
		}
	}
	return s
}

func (s *Store) saveLocked() error {
	tmp := s.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(s.idx); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, s.path)
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.idx.Entries)
}

func (s *Store) Has(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.idx.Entries[path]
	return ok
}

func (s *Store) GetStatus() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *Store) setStatus(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

func (s *Store) Cancel() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

var skipDirs = map[string]bool{"node_modules": true, "__pycache__": true, "venv": true, ".venv": true, "$recycle.bin": true}

type job struct {
	path string
	size int64
	mod  int64
	req  EmbedReq
	mode string
}

// SetExcluded replaces the excluded-folder list and drops already-indexed files inside them.
func (s *Store) SetExcluded(dirs []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.excluded = s.excluded[:0]
	for _, d := range dirs {
		if d = strings.TrimSpace(d); d != "" {
			s.excluded = append(s.excluded, strings.ToLower(filepath.Clean(d)))
		}
	}
	removed := 0
	for p := range s.idx.Entries {
		if s.isExcludedLocked(p) {
			delete(s.idx.Entries, p)
			removed++
		}
	}
	if removed > 0 {
		s.saveLocked()
	}
	return removed
}

func (s *Store) isExcludedLocked(p string) bool {
	lp := strings.ToLower(p)
	for _, ex := range s.excluded {
		if lp == ex || strings.HasPrefix(lp, ex+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// StartIndex runs indexing of root in the background.
func (s *Store) StartIndex(root string, c *Client) error {
	s.mu.Lock()
	if s.status.Running {
		s.mu.Unlock()
		return errors.New("indexing sedang berjalan")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.status = Status{Running: true, Message: "Memindai folder..."}
	if s.idx.Model != c.Model || s.idx.Dim != c.Dim {
		s.idx = &Index{Model: c.Model, Dim: c.Dim, Version: indexVersion, Entries: map[string]*Entry{}}
	}
	s.mu.Unlock()
	c.OnWait = func(d time.Duration) {
		s.setStatus(func(st *Status) {
			st.Current = fmt.Sprintf("menunggu batas per menit Gemini (%d dtk)", int(d.Seconds()))
		})
	}
	go func() {
		msg := s.runIndex(ctx, root, c)
		s.mu.Lock()
		s.saveLocked()
		s.status.Running = false
		s.status.Current = ""
		s.status.Message = msg
		s.cancel = nil
		s.mu.Unlock()
		cancel()
	}()
	return nil
}

func (s *Store) runIndex(ctx context.Context, root string, c *Client) string {
	root = filepath.Clean(root)
	type fileInfo struct {
		path      string
		size, mod int64
	}
	var files []fileInfo
	seen := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || skipDirs[strings.ToLower(name)]) {
				return filepath.SkipDir
			}
			s.mu.RLock()
			ex := s.isExcludedLocked(p)
			s.mu.RUnlock()
			if ex {
				return filepath.SkipDir
			}
			return nil
		}
		ln := strings.ToLower(name)
		if !indexable(name) || ln == "desktop.ini" || ln == "thumbs.db" || strings.HasPrefix(name, "~") ||
			strings.HasSuffix(ln, ".tmp") || strings.HasSuffix(ln, ".crdownload") || strings.HasSuffix(ln, ".part") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		seen[p] = true
		files = append(files, fileInfo{p, info.Size(), info.ModTime().Unix()})
		return nil
	})

	// Drop entries under root whose files are gone; find what needs (re)embedding.
	s.mu.Lock()
	prefix := strings.ToLower(root + string(os.PathSeparator))
	for p := range s.idx.Entries {
		if strings.HasPrefix(strings.ToLower(p), prefix) && !seen[p] {
			delete(s.idx.Entries, p)
		}
	}
	var todo []fileInfo
	for _, f := range files {
		if e, ok := s.idx.Entries[f.path]; ok && e.Size == f.size && e.ModTime == f.mod {
			continue
		}
		todo = append(todo, f)
	}
	// Shallow folders first: the user's own files usually sit near the top, deep trees are datasets/source.
	sort.SliceStable(todo, func(i, j int) bool {
		return strings.Count(todo[i].path, string(os.PathSeparator)) < strings.Count(todo[j].path, string(os.PathSeparator))
	})
	s.status.Total = len(todo)
	s.status.Skipped = len(files) - len(todo)
	s.status.Message = fmt.Sprintf("%d file baru/berubah, %d sudah ter-index", len(todo), len(files)-len(todo))
	s.mu.Unlock()
	if len(todo) == 0 {
		return fmt.Sprintf("Index sudah terbaru (%d file).", len(files))
	}

	const maxBatch, maxBytes = 40, 9 << 20
	var batch []job
	bytesInBatch := 0
	var fatal error
	lastSave := time.Now()

	flush := func() {
		if len(batch) == 0 || fatal != nil {
			return
		}
		results := s.embedJobs(ctx, c, batch, &fatal)
		s.mu.Lock()
		for i, j := range batch {
			if results[i] == nil {
				if fatal == nil {
					s.status.Failed++
				}
				continue
			}
			s.idx.Entries[j.path] = &Entry{Path: j.path, Size: j.size, ModTime: j.mod, Kind: kindOf(j.path), Mode: j.mode, Vec: results[i]}
			s.status.Done++
		}
		if time.Since(lastSave) > 10*time.Second {
			s.saveLocked()
			lastSave = time.Now()
		}
		s.mu.Unlock()
		batch, bytesInBatch = nil, 0
	}

	for _, f := range todo {
		if ctx.Err() != nil || fatal != nil {
			break
		}
		s.setStatus(func(st *Status) { st.Current = filepath.Base(f.path) })
		req, mode := buildReq(f.path, root, f.size)
		n := req.approxBytes()
		if len(batch) > 0 && (len(batch) >= maxBatch || bytesInBatch+n > maxBytes) {
			flush()
		}
		batch = append(batch, job{f.path, f.size, f.mod, req, mode})
		bytesInBatch += n
	}
	flush()

	st := s.GetStatus()
	summary := fmt.Sprintf("%d ter-index, %d gagal, %d tidak berubah.", st.Done, st.Failed, st.Skipped)
	switch {
	case fatal != nil:
		msg := "Berhenti: " + friendlyErr(fatal)
		if r := c.KeyReport(); r != "" {
			msg += " (" + r + ")"
		}
		return msg + " Progres tersimpan. " + summary
	case ctx.Err() != nil:
		return "Dibatalkan. Progres tersimpan. " + summary
	}
	return "Selesai. " + summary
}

// embedJobs embeds a batch; on a bad request it retries item by item, then falls back to name-only.
func (s *Store) embedJobs(ctx context.Context, c *Client, jobs []job, fatal *error) [][]float32 {
	out := make([][]float32, len(jobs))
	reqs := make([]EmbedReq, len(jobs))
	for i, j := range jobs {
		reqs[i] = j.req
	}
	vecs, err := c.Batch(ctx, reqs)
	if err == nil {
		return vecs
	}
	if isFatal(err) || ctx.Err() != nil {
		*fatal = err
		return out
	}
	for i := range jobs {
		v, err := c.Batch(ctx, []EmbedReq{jobs[i].req})
		if err != nil && !isFatal(err) && ctx.Err() == nil && jobs[i].mode != "nama" {
			jobs[i].mode = "nama"
			v, err = c.Batch(ctx, []EmbedReq{nameOnlyReq(jobs[i].path, filepath.Dir(jobs[i].path))})
		}
		if err != nil {
			if isFatal(err) || ctx.Err() != nil {
				*fatal = err
				return out
			}
			continue
		}
		out[i] = v[0]
	}
	return out
}

func isFatal(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == 401 || he.Status == 403 || he.Status == 429 || he.Status >= 500 ||
			(he.Status == 400 && strings.Contains(he.Body, "API key"))
	}
	return !errors.Is(err, context.Canceled) // network errors after retries
}

func friendlyErr(err error) string {
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == 400 && strings.Contains(he.Body, "API key"), he.Status == 401, he.Status == 403:
			return "API key tidak valid atau tidak punya akses ke model ini."
		case he.Status == 429:
			return "Semua API key kena limit atau kuota hariannya habis. Tambah key lain atau lanjutkan nanti."
		case he.Status == 404:
			return "Model tidak ditemukan, cek nama model di Pengaturan."
		}
	}
	if errors.Is(err, context.Canceled) {
		return "Dibatalkan."
	}
	if errors.Is(err, errAllDead) {
		return "Semua API key tidak bisa dipakai (tidak valid atau kuota harian habis)."
	}
	return err.Error()
}

type Result struct {
	Path  string  `json:"path"`
	Name  string  `json:"name"`
	Dir   string  `json:"dir"`
	Kind  string  `json:"kind"`
	Mode  string  `json:"mode"`
	Size  int64   `json:"size"`
	MTime int64   `json:"mtime"`
	Score float32 `json:"score"`
}

func (s *Store) Search(qv []float32, query, kind string, limit int) []Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var toks []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if len(t) >= 3 {
			toks = append(toks, t)
		}
	}
	res := make([]Result, 0, len(s.idx.Entries))
	for _, e := range s.idx.Entries {
		if kind != "" && kind != "semua" && e.Kind != kind {
			continue
		}
		if len(e.Vec) != len(qv) {
			continue
		}
		var dot float32
		for i, x := range e.Vec {
			dot += x * qv[i]
		}
		ln := strings.ToLower(filepath.Base(e.Path))
		for _, t := range toks {
			if strings.Contains(ln, t) {
				dot += 0.04
			}
		}
		res = append(res, Result{Path: e.Path, Name: filepath.Base(e.Path), Dir: filepath.Dir(e.Path), Kind: e.Kind, Mode: e.Mode, Size: e.Size, MTime: e.ModTime, Score: dot})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Score > res[j].Score })
	if len(res) > limit {
		res = res[:limit]
	}
	return res
}
