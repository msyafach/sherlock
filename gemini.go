package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type Part struct {
	Text       string `json:"text,omitempty"`
	InlineData *Blob  `json:"inlineData,omitempty"`
}

type Content struct {
	Parts []Part `json:"parts"`
}

type EmbedReq struct {
	Model                string  `json:"model"`
	Content              Content `json:"content"`
	OutputDimensionality int     `json:"outputDimensionality,omitempty"`
}

func (r EmbedReq) approxBytes() int {
	n := 200
	for _, p := range r.Content.Parts {
		n += len(p.Text)
		if p.InlineData != nil {
			n += len(p.InlineData.Data)
		}
	}
	return n
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	b := e.Body
	if len(b) > 400 {
		b = b[:400]
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, b)
}

// keyState tracks one API key in the rotation.
type keyState struct {
	key     string
	until   time.Time // cooling down (per-minute limit) until this time
	dead    bool      // daily quota used up or key invalid
	deadWhy string
}

type Client struct {
	Model  string
	Dim    int
	Fake   bool
	OnWait func(time.Duration)
	http   *http.Client

	mu   sync.Mutex
	keys []*keyState
	next int
}

func NewClient(keys []string, model string, dim int, fake bool) *Client {
	c := &Client{Model: model, Dim: dim, Fake: fake, http: &http.Client{Timeout: 180 * time.Second}}
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			c.keys = append(c.keys, &keyState{key: k})
		}
	}
	return c
}

var errAllDead = fmt.Errorf("semua API key tidak bisa dipakai")

// pick returns the next usable key (round-robin), or how long to wait until one frees up.
func (c *Client) pick() (*keyState, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var soonest time.Duration = -1
	for i := 0; i < len(c.keys); i++ {
		k := c.keys[(c.next+i)%len(c.keys)]
		if k.dead {
			continue
		}
		if wait := k.until.Sub(now); wait > 0 {
			if soonest < 0 || wait < soonest {
				soonest = wait
			}
			continue
		}
		c.next = (c.next + i + 1) % len(c.keys)
		return k, 0, nil
	}
	if soonest < 0 {
		return nil, 0, errAllDead
	}
	return nil, soonest, nil
}

func (c *Client) mark(k *keyState, f func(*keyState)) {
	c.mu.Lock()
	f(k)
	c.mu.Unlock()
}

// Batch embeds many requests in one call, retrying on rate limits / server errors.
func (c *Client) Batch(ctx context.Context, reqs []EmbedReq) ([][]float32, error) {
	if c.Fake {
		out := make([][]float32, len(reqs))
		for i, r := range reqs {
			out[i] = fakeEmbed(r, c.Dim)
		}
		return out, nil
	}
	for i := range reqs {
		reqs[i].Model = "models/" + c.Model
		reqs[i].OutputDimensionality = c.Dim
	}
	body, err := json.Marshal(map[string]any{"requests": reqs})
	if err != nil {
		return nil, err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + c.Model + ":batchEmbedContents"
	waits := []time.Duration{5, 10, 20, 40, 60, 60}
	errTries, waitsForLimit := 0, 0
	var lastErr error
	sleep := func(d time.Duration) error {
		if c.OnWait != nil {
			c.OnWait(d)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	for {
		k, wait, err := c.pick()
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if wait > 0 { // every key is cooling down on its per-minute limit
			if waitsForLimit++; waitsForLimit > 120 {
				return nil, lastErr
			}
			if err := sleep(wait + time.Second); err != nil {
				return nil, err
			}
			continue
		}
		vecs, err := c.post(ctx, url, body, k.key)
		if err == nil {
			if len(vecs) != len(reqs) {
				return nil, fmt.Errorf("jumlah embedding tidak cocok: %d vs %d", len(vecs), len(reqs))
			}
			return vecs, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		he, ok := err.(*HTTPError)
		switch {
		case ok && he.Status == 429 && strings.Contains(he.Body, "PerDay"):
			c.mark(k, func(s *keyState) { s.dead, s.deadWhy = true, "kuota harian habis" })
		case ok && he.Status == 429:
			d := retryDelay(he.Body)
			c.mark(k, func(s *keyState) { s.until = time.Now().Add(d) })
		case ok && (he.Status == 401 || he.Status == 403 || (he.Status == 400 && strings.Contains(he.Body, "API key"))):
			c.mark(k, func(s *keyState) { s.dead, s.deadWhy = true, "key tidak valid" })
		case !ok || he.Status >= 500:
			if errTries >= len(waits) {
				return nil, err
			}
			if err := sleep(waits[errTries] * time.Second); err != nil {
				return nil, err
			}
			errTries++
		default:
			return nil, err
		}
	}
}

// KeyReport summarizes which keys ended up unusable.
func (c *Client) KeyReport() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	for i, k := range c.keys {
		if k.dead {
			parts = append(parts, fmt.Sprintf("key #%d: %s", i+1, k.deadWhy))
		}
	}
	return strings.Join(parts, ", ")
}

var retryRe = regexp.MustCompile(`"retryDelay":\s*"(\d+(?:\.\d+)?)s"`)

func retryDelay(body string) time.Duration {
	if m := retryRe.FindStringSubmatch(body); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			return time.Duration(f*float64(time.Second)) + 2*time.Second
		}
	}
	return 30 * time.Second
}

func (c *Client) post(ctx context.Context, url string, body []byte, key string) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	var out struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(out.Embeddings))
	for i, e := range out.Embeddings {
		vecs[i] = normalize(e.Values)
	}
	return vecs, nil
}

func (c *Client) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	r := EmbedReq{Content: Content{Parts: []Part{{Text: "task: search result | query: " + q}}}}
	v, err := c.Batch(ctx, []EmbedReq{r})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// fakeEmbed: hashed bag-of-words, used only for offline testing (SHERLOCK_FAKE=1).
func fakeEmbed(r EmbedReq, dim int) []float32 {
	v := make([]float32, dim)
	var sb strings.Builder
	for _, p := range r.Content.Parts {
		sb.WriteString(p.Text)
		sb.WriteByte(' ')
	}
	text := strings.ToLower(sb.String())
	for _, pfx := range []string{"task: search result | query:", "title:", "text:"} {
		text = strings.ReplaceAll(text, pfx, " ")
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, w := range words {
		if len(w) < 2 {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(w))
		v[int(h.Sum32())%dim] += 1
	}
	return normalize(v)
}
