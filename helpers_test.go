// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"bytes"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testUA is deliberately different from DefaultUserAgent, so tests prove the
// configured value is sent rather than a default.
const testUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// testPNG returns a small valid PNG whose colour is derived from seed, so
// different seeds give different bytes.
func testPNG(seed string) []byte {
	h := fnv.New32a()
	h.Write([]byte(seed))
	v := h.Sum32()
	img := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.RGBA{R: uint8(v), G: uint8(v >> 8), B: uint8(v >> 16), A: 0xff}})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

type recordedRequest struct {
	path      string
	userAgent string
	at        time.Time
}

// upstream is a fake tile server that records every request.
type upstream struct {
	*httptest.Server

	mu     sync.Mutex
	reqs   []recordedRequest
	active int
	peak   int // highest number of requests handled at once
}

// newUpstream starts a fake tile server. A nil handler serves testPNG of the
// request path.
func newUpstream(t *testing.T, handler http.HandlerFunc) *upstream {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(testPNG(r.URL.Path))
		}
	}
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, recordedRequest{path: r.URL.Path, userAgent: r.Header.Get("User-Agent"), at: time.Now()})
		u.active++
		u.peak = max(u.peak, u.active)
		u.mu.Unlock()
		defer func() {
			u.mu.Lock()
			u.active--
			u.mu.Unlock()
		}()
		handler(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

// template is an Upstream URL template pointing at the fake server; the
// subdomain becomes the first path segment.
func (u *upstream) template() string { return u.URL + "/{s}/{z}/{x}/{y}.png" }

func (u *upstream) requests() []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedRequest(nil), u.reqs...)
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) peakConcurrency() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.peak
}

// newTestCache creates a Cache in a temporary directory with testUA and a
// logger writing to the test output, and closes it when the test ends.
func newTestCache(t *testing.T, opts Options) *Cache {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.UserAgent == "" {
		opts.UserAgent = testUA
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// get serves one request through h. Extra arguments are header name/value
// pairs.
func get(h http.Handler, method, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// waitFor polls cond until it holds, failing the test after a generous
// deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// files lists the regular files below dir, relative to it.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, f := range files(t, dir) {
		if strings.HasPrefix(filepath.Base(f), ".tile-") {
			t.Errorf("temporary file left behind: %s", f)
		}
	}
}

// waiters reports how many callers wait on the flight for k.
func (g *flightGroup) waiters(k tile) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f := g.m[k]; f != nil {
		return f.refs
	}
	return 0
}

// inFlight reports the number of flights in progress.
func (g *flightGroup) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// fakeClock is an injectable Clock that only moves when told to.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(now time.Time) *fakeClock {
	c := &fakeClock{}
	c.set(now)
	return c
}

func (c *fakeClock) now() time.Time  { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) set(t time.Time) { c.ns.Store(t.UnixNano()) }

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
