// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"bytes"
	"fmt"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMissThenHit(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	h := c.Handler()
	want := testPNG("/b/4/5/6.png")

	first := get(h, http.MethodGet, "/b/4/5/6.png")
	if first.Code != http.StatusOK || first.Header().Get("X-Tile-Cache") != "MISS" {
		t.Fatalf("first request: status %d, X-Tile-Cache %q; want 200 MISS", first.Code, first.Header().Get("X-Tile-Cache"))
	}
	if !bytes.Equal(first.Body.Bytes(), want) {
		t.Fatal("first request: body differs from upstream tile")
	}
	onDisk, err := os.ReadFile(filepath.Join(c.dir, "4", "5", "6.png"))
	if err != nil {
		t.Fatalf("tile not stored: %v", err)
	}
	if !bytes.Equal(onDisk, want) {
		t.Fatal("stored tile differs from upstream tile")
	}

	second := get(h, http.MethodGet, "/b/4/5/6.png")
	if second.Code != http.StatusOK || second.Header().Get("X-Tile-Cache") != "HIT" {
		t.Fatalf("second request: status %d, X-Tile-Cache %q; want 200 HIT", second.Code, second.Header().Get("X-Tile-Cache"))
	}
	if !bytes.Equal(second.Body.Bytes(), want) {
		t.Fatal("second request: body differs from upstream tile")
	}

	reqs := u.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	if reqs[0].path != "/b/4/5/6.png" {
		t.Errorf("upstream path = %q, want /b/4/5/6.png", reqs[0].path)
	}
	if reqs[0].userAgent != testUA {
		t.Errorf("upstream User-Agent = %q, want %q", reqs[0].userAgent, testUA)
	}
	assertNoTempFiles(t, c.dir)
}

func TestResponseHeaders(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	h := c.Handler()
	const path = "/a/2/1/3.png"

	miss := get(h, http.MethodGet, path)
	for name, want := range map[string]string{
		"Content-Type":   "image/png",
		"Cache-Control":  "public, max-age=86400",
		"X-Tile-Cache":   "MISS",
		"Content-Length": strconv.Itoa(miss.Body.Len()),
	} {
		if got := miss.Header().Get(name); got != want {
			t.Errorf("MISS %s = %q, want %q", name, got, want)
		}
	}
	tag := miss.Header().Get("ETag")
	if len(tag) < 3 || !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Fatalf("ETag %q is not a strong entity tag", tag)
	}

	hit := get(h, http.MethodGet, path)
	if hit.Header().Get("X-Tile-Cache") != "HIT" {
		t.Fatalf("second request X-Tile-Cache = %q, want HIT", hit.Header().Get("X-Tile-Cache"))
	}
	if got := hit.Header().Get("ETag"); got != tag {
		t.Errorf("HIT ETag = %q, want %q (same as MISS)", got, tag)
	}
	if hit.Header().Get("Last-Modified") == "" {
		t.Error("HIT has no Last-Modified")
	}
	if got := hit.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("HIT Cache-Control = %q", got)
	}
	if other := get(h, http.MethodGet, "/a/2/1/2.png").Header().Get("ETag"); other == tag {
		t.Error("different tiles share an ETag")
	}

	for _, inm := range []string{tag, "W/" + tag, `"nope", ` + tag, "*"} {
		rec := get(h, http.MethodGet, path, "If-None-Match", inm)
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %s: status %d, want 304", inm, rec.Code)
			continue
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: 304 has a body", inm)
		}
		if rec.Header().Get("ETag") != tag || rec.Header().Get("X-Tile-Cache") != "HIT" {
			t.Errorf("If-None-Match %s: ETag %q X-Tile-Cache %q", inm, rec.Header().Get("ETag"), rec.Header().Get("X-Tile-Cache"))
		}
	}
	if rec := get(h, http.MethodGet, path, "If-None-Match", `"nope"`); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("non-matching If-None-Match: status %d, %d bytes; want 200 with body", rec.Code, rec.Body.Len())
	}

	head := get(h, http.MethodHead, path)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: status %d, %d body bytes; want 200 without body", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(miss.Body.Len()) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, miss.Body.Len())
	}

	if got := u.count(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (one per distinct tile)", got)
	}
}

// fillUA is a transport that, like many client libraries, fills in its own
// User-Agent when a request has none.
type fillUA struct{ base http.RoundTripper }

func (f *fillUA) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", "SomeLibrary/2.0")
	}
	return f.base.RoundTrip(r)
}

func TestUserAgentForced(t *testing.T) {
	t.Run("default client", func(t *testing.T) {
		u := newUpstream(t, nil)
		c := newTestCache(t, Options{Upstream: u.template()})
		if rec := get(c.Handler(), http.MethodGet, "/a/1/0/0.png"); rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
			t.Fatalf("status %d, X-Tile-Cache %q", rec.Code, rec.Header().Get("X-Tile-Cache"))
		}
		assertUserAgents(t, u, 1)
	})

	t.Run("client transport with its own default", func(t *testing.T) {
		u := newUpstream(t, nil)
		transport := &fillUA{base: u.Client().Transport}
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		c := newTestCache(t, Options{Upstream: u.template(), HTTPClient: client})
		if rec := get(c.Handler(), http.MethodGet, "/a/1/0/1.png"); rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
			t.Fatalf("status %d, X-Tile-Cache %q", rec.Code, rec.Header().Get("X-Tile-Cache"))
		}
		assertUserAgents(t, u, 1)
		if client.Transport != transport {
			t.Error("New modified the caller's http.Client")
		}
		if c.client.Timeout != 3*time.Second {
			t.Errorf("client timeout = %v, want the caller's 3s", c.client.Timeout)
		}
	})

	t.Run("redirect follow-up", func(t *testing.T) {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/moved/") {
				http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound)
				return
			}
			w.Write(testPNG(r.URL.Path))
		})
		c := newTestCache(t, Options{Upstream: u.template()})
		if rec := get(c.Handler(), http.MethodGet, "/a/1/1/1.png"); rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
			t.Fatalf("status %d, X-Tile-Cache %q", rec.Code, rec.Header().Get("X-Tile-Cache"))
		}
		assertUserAgents(t, u, 2)
	})
}

func assertUserAgents(t *testing.T, u *upstream, n int) {
	t.Helper()
	reqs := u.requests()
	if len(reqs) != n {
		t.Fatalf("upstream requests = %d, want %d", len(reqs), n)
	}
	for _, r := range reqs {
		if r.userAgent != testUA {
			t.Errorf("request %s User-Agent = %q, want %q", r.path, r.userAgent, testUA)
		}
	}
}

func TestSingleFlight(t *testing.T) {
	gate := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(testPNG(r.URL.Path))
	})
	c := newTestCache(t, Options{Upstream: u.template()})
	h := c.Handler()
	want := testPNG("/c/3/2/1.png")

	const n = 20
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { recs[i] = get(h, http.MethodGet, "/c/3/2/1.png") })
	}
	waitFor(t, "the upstream request", func() bool { return u.count() == 1 })
	waitFor(t, "all requests to join the flight", func() bool { return c.flights.waiters(tile{3, 2, 1}) == n })
	close(gate)
	wg.Wait()

	if got := u.count(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
	for i, rec := range recs {
		if rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("request %d: status %d, X-Tile-Cache %q, %d bytes; want 200 MISS with the tile",
				i, rec.Code, rec.Header().Get("X-Tile-Cache"), rec.Body.Len())
		}
	}
}

func TestFailedFetchIsNotCached(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"not a PNG", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>rate limited</html>"))
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"non-200 with PNG body", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(testPNG(r.URL.Path))
		}},
		{"truncated body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Write(testPNG(r.URL.Path)[:20])
		}},
		{"oversized body", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(pngSignature))
			w.Write(make([]byte, maxTileBytes))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.handler)
			c := newTestCache(t, Options{Upstream: u.template()})
			h := c.Handler()
			for i := range 2 {
				rec := get(h, http.MethodGet, "/a/2/2/2.png")
				assertPlaceholder(t, c, rec)
				if got := u.count(); got != i+1 {
					t.Fatalf("after request %d: upstream requests = %d, want %d", i+1, got, i+1)
				}
			}
			if left := files(t, c.dir); len(left) != 0 {
				t.Errorf("files left in cache: %v", left)
			}
		})
	}
}

func assertPlaceholder(t *testing.T, c *Cache, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	for name, want := range map[string]string{
		"X-Tile-Cache":  "PLACEHOLDER",
		"Cache-Control": "no-cache",
		"Content-Type":  "image/png",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("placeholder has an ETag")
	}
	if !bytes.Equal(rec.Body.Bytes(), c.placeholder) {
		t.Error("body is not the placeholder")
	}
}

func TestStoreFailureLeavesNoTempFile(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	// A directory where the tile belongs makes the final rename fail after
	// the temporary file has been written.
	blocker := c.path(tile{3, 1, 1})
	if err := os.MkdirAll(filepath.Join(blocker, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := get(c.Handler(), http.MethodGet, "/a/3/1/1.png")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
		t.Fatalf("status %d, X-Tile-Cache %q; want the fetched tile despite the store failure", rec.Code, rec.Header().Get("X-Tile-Cache"))
	}
	if !bytes.Equal(rec.Body.Bytes(), testPNG("/a/3/1/1.png")) {
		t.Error("body differs from upstream tile")
	}
	assertNoTempFiles(t, c.dir)
}

func TestInvalidCachedTileIsReplaced(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	path := c.path(tile{2, 3, 1})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := get(c.Handler(), http.MethodGet, "/a/2/3/1.png")
	if rec.Header().Get("X-Tile-Cache") != "MISS" {
		t.Fatalf("X-Tile-Cache = %q, want MISS", rec.Header().Get("X-Tile-Cache"))
	}
	if got := u.count(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, testPNG("/a/2/3/1.png")) {
		t.Errorf("corrupt tile not replaced (err %v)", err)
	}
}

func TestPlaceholderWhenUpstreamDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	tmpl := down.URL + "/{s}/{z}/{x}/{y}.png"
	down.Close()

	t.Run("default placeholder", func(t *testing.T) {
		c := newTestCache(t, Options{Upstream: tmpl})
		rec := get(c.Handler(), http.MethodGet, "/a/1/1/0.png")
		assertPlaceholder(t, c, rec)
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("placeholder is not a PNG: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
			t.Errorf("placeholder is %dx%d, want 256x256", b.Dx(), b.Dy())
		}
		want := color.RGBA{R: 0x11, G: 0x15, B: 0x1a, A: 0xff}
		if got := color.RGBAModel.Convert(img.At(128, 128)); got != want {
			t.Errorf("placeholder colour = %v, want %v", got, want)
		}
		if left := files(t, c.dir); len(left) != 0 {
			t.Errorf("files left in cache: %v", left)
		}
	})

	t.Run("custom placeholder", func(t *testing.T) {
		custom := testPNG("custom placeholder")
		want := bytes.Clone(custom)
		c := newTestCache(t, Options{Upstream: tmpl, Placeholder: custom})
		custom[len(custom)-1] ^= 0xff // New must have copied it
		rec := get(c.Handler(), http.MethodGet, "/a/1/1/0.png")
		if rec.Header().Get("X-Tile-Cache") != "PLACEHOLDER" || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("X-Tile-Cache %q; body equals custom placeholder: %v", rec.Header().Get("X-Tile-Cache"), bytes.Equal(rec.Body.Bytes(), want))
		}
	})
}

func TestStaleTileRefreshedOnce(t *testing.T) {
	now := time.Now()
	clk := newFakeClock(now)
	gate := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(testPNG("fresh " + r.URL.Path))
	})
	c := newTestCache(t, Options{Upstream: u.template(), Clock: clk.now})
	h := c.Handler()

	k := tile{5, 3, 7}
	path := c.path(k)
	stale := testPNG("stale")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	// While the refresh is held at the upstream, every request is served
	// the stale tile and none starts a second refresh.
	for i := range 5 {
		rec := get(h, http.MethodGet, "/a/5/3/7.png")
		if rec.Header().Get("X-Tile-Cache") != "HIT" || !bytes.Equal(rec.Body.Bytes(), stale) {
			t.Fatalf("request %d: X-Tile-Cache %q; want the stale tile as HIT", i, rec.Header().Get("X-Tile-Cache"))
		}
	}
	waitFor(t, "the refresh to reach upstream", func() bool { return u.count() == 1 })
	if got := c.flights.inFlight(); got != 1 {
		t.Fatalf("flights in progress = %d, want 1", got)
	}
	close(gate)
	waitFor(t, "the refresh to finish", func() bool { return c.flights.inFlight() == 0 })

	fresh := testPNG("fresh /a/5/3/7.png")
	rec := get(h, http.MethodGet, "/a/5/3/7.png")
	if rec.Header().Get("X-Tile-Cache") != "HIT" || !bytes.Equal(rec.Body.Bytes(), fresh) {
		t.Fatalf("after refresh: X-Tile-Cache %q; want the refreshed tile as HIT", rec.Header().Get("X-Tile-Cache"))
	}
	if got := u.count(); got != 1 {
		t.Fatalf("upstream requests = %d, want exactly 1 refresh", got)
	}
	if got := c.flights.inFlight(); got != 0 {
		t.Fatalf("a fresh tile started a refresh")
	}

	// Once the injected clock passes MaxAge again, the next access
	// triggers exactly one more refresh.
	clk.set(now.Add(31 * 24 * time.Hour))
	get(h, http.MethodGet, "/a/5/3/7.png")
	waitFor(t, "the second refresh", func() bool { return u.count() == 2 && c.flights.inFlight() == 0 })
	get(h, http.MethodGet, "/a/5/3/7.png")
	if got := c.flights.inFlight(); got != 0 || u.count() != 2 {
		t.Errorf("tile refreshed at the injected clock is stale again: %d flights, %d upstream requests", got, u.count())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The injected clock is a month ahead of the real one, so this proves
	// the mtime was taken from it (with slack for coarse filesystems).
	if d := info.ModTime().Sub(clk.now()).Abs(); d > time.Second {
		t.Errorf("stored tile mtime = %v, want the injected clock %v", info.ModTime(), clk.now())
	}
}

func TestRefreshPausedAfterUpstreamFailure(t *testing.T) {
	now := time.Now()
	clk := newFakeClock(now)
	var failing atomic.Bool
	failing.Store(true)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write(testPNG("new " + r.URL.Path))
	})
	c := newTestCache(t, Options{Upstream: u.template(), Clock: clk.now})
	h := c.Handler()

	old := now.Add(-40 * 24 * time.Hour)
	for _, k := range []tile{{4, 1, 1}, {4, 1, 2}} {
		path := c.path(k)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, testPNG("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	get(h, http.MethodGet, "/a/4/1/1.png")
	waitFor(t, "the failed refresh", func() bool { return u.count() == 1 && c.flights.inFlight() == 0 })

	for _, p := range []string{"/a/4/1/1.png", "/a/4/1/2.png"} {
		if rec := get(h, http.MethodGet, p); rec.Header().Get("X-Tile-Cache") != "HIT" {
			t.Fatalf("%s: X-Tile-Cache %q, want the stale tile as HIT", p, rec.Header().Get("X-Tile-Cache"))
		}
		if c.flights.inFlight() != 0 {
			t.Fatalf("%s: refresh started while paused", p)
		}
	}

	failing.Store(false)
	clk.set(now.Add(refreshBackoff + time.Second))
	get(h, http.MethodGet, "/a/4/1/2.png")
	waitFor(t, "the refresh after the pause", func() bool { return u.count() == 2 && c.flights.inFlight() == 0 })
	onDisk, err := os.ReadFile(c.path(tile{4, 1, 2}))
	if err != nil || !bytes.Equal(onDisk, testPNG("new /a/4/1/2.png")) {
		t.Errorf("tile not refreshed after the pause (err %v)", err)
	}
}

func TestRequestValidation(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	low := newTestCache(t, Options{Upstream: u.template(), MaxZoom: 5})

	tests := []struct {
		name  string
		cache *Cache
		path  string
		want  int
	}{
		{"x out of range", c, "/a/1/2/0.png", http.StatusBadRequest},
		{"y out of range", c, "/a/1/0/2.png", http.StatusBadRequest},
		{"zoom 0 has one tile", c, "/a/0/1/0.png", http.StatusBadRequest},
		{"non-numeric zoom", c, "/a/z/0/0.png", http.StatusBadRequest},
		{"negative x", c, "/a/1/-1/0.png", http.StatusBadRequest},
		{"signed x", c, "/a/1/+1/0.png", http.StatusBadRequest},
		{"hex y", c, "/a/1/0/0x1.png", http.StatusBadRequest},
		{"too many digits", c, "/a/1/0/12345678901.png", http.StatusBadRequest},
		{"unknown subdomain", c, "/d/1/0/0.png", http.StatusBadRequest},
		{"zoom above default MaxZoom", c, "/a/20/0/0.png", http.StatusNotFound},
		{"zoom above custom MaxZoom", low, "/a/6/0/0.png", http.StatusNotFound},
		{"wrong extension", c, "/a/1/0/0.jpg", http.StatusNotFound},
		{"no extension", c, "/a/1/0/0", http.StatusNotFound},
		{"empty y", c, "/a/1/0/.png", http.StatusNotFound},
		{"trailing segment", c, "/a/1/0/0.png/x", http.StatusNotFound},
		{"missing segment", c, "/a/1/0.png", http.StatusNotFound},
		{"empty subdomain", c, "//1/0/0.png", http.StatusNotFound},
		{"root", c, "/", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := get(tt.cache.Handler(), http.MethodGet, tt.path); rec.Code != tt.want {
				t.Errorf("GET %s: status %d, want %d", tt.path, rec.Code, tt.want)
			}
		})
	}

	t.Run("method", func(t *testing.T) {
		rec := get(c.Handler(), http.MethodPost, "/a/1/0/0.png")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("POST: status %d, Allow %q; want 405 with Allow: GET, HEAD", rec.Code, rec.Header().Get("Allow"))
		}
	})

	if got := u.count(); got != 0 {
		t.Errorf("invalid requests reached upstream %d times", got)
	}
}

func TestUpstreamWithoutSubdomain(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.URL + "/{z}/{x}/{y}.png"})
	rec := get(c.Handler(), http.MethodGet, "/anything/1/1/1.png")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
		t.Fatalf("status %d, X-Tile-Cache %q; want 200 MISS", rec.Code, rec.Header().Get("X-Tile-Cache"))
	}
	if reqs := u.requests(); len(reqs) != 1 || reqs[0].path != "/1/1/1.png" {
		t.Errorf("upstream requests = %+v, want one for /1/1/1.png", reqs)
	}
}

func TestMountWithStripPrefix(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	for _, prefix := range []string{"/tiles", "/tiles/"} {
		mux := http.NewServeMux()
		mux.Handle("/tiles/", http.StripPrefix(prefix, c.Handler()))
		if rec := get(mux, http.MethodGet, "/tiles/a/1/0/1.png"); rec.Code != http.StatusOK {
			t.Errorf("StripPrefix(%q): status %d, want 200", prefix, rec.Code)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	gate := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(testPNG(r.URL.Path))
	})
	c := newTestCache(t, Options{Upstream: u.template(), Concurrency: 2})
	h := c.Handler()

	const n = 6
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { recs[i] = get(h, http.MethodGet, fmt.Sprintf("/a/3/%d/0.png", i)) })
	}
	waitFor(t, "all fetches to start", func() bool { return c.flights.inFlight() == n })
	waitFor(t, "two upstream requests", func() bool { return u.count() == 2 })
	if got := len(c.sem); got != 2 {
		t.Errorf("fetch slots in use = %d, want 2", got)
	}
	close(gate)
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK || rec.Header().Get("X-Tile-Cache") != "MISS" {
			t.Errorf("request %d: status %d, X-Tile-Cache %q", i, rec.Code, rec.Header().Get("X-Tile-Cache"))
		}
	}
	if got := u.count(); got != n {
		t.Errorf("upstream requests = %d, want %d", got, n)
	}
	if got := u.peakConcurrency(); got > 2 {
		t.Errorf("peak concurrent upstream requests = %d, want <= 2", got)
	}
}

func TestRateLimit(t *testing.T) {
	const interval = 25 * time.Millisecond
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template(), Concurrency: 4, RateLimit: interval})
	h := c.Handler()

	start := time.Now()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() { get(h, http.MethodGet, fmt.Sprintf("/a/2/%d/0.png", i)) })
	}
	wg.Wait()

	reqs := u.requests()
	if len(reqs) != 4 {
		t.Fatalf("upstream requests = %d, want 4", len(reqs))
	}
	last := slices.MaxFunc(reqs, func(a, b recordedRequest) int { return a.at.Compare(b.at) })
	// Four requests spaced at least interval apart span 3 intervals.
	if span := last.at.Sub(start); span < 3*interval {
		t.Errorf("four rate-limited requests took %v, want at least %v", span, 3*interval)
	}
}

// TestCacheControlOption: a cache behind a login serves its tiles as private.
func TestCacheControlOption(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template(), CacheControl: "private, max-age=86400"})
	h := c.Handler()
	for _, want := range []string{"MISS", "HIT"} {
		res := get(h, http.MethodGet, "/a/3/2/1.png")
		if res.Code != http.StatusOK || res.Header().Get("X-Tile-Cache") != want || res.Header().Get("Cache-Control") != "private, max-age=86400" {
			t.Fatalf("%s: status %d, Cache-Control %q", want, res.Code, res.Header().Get("Cache-Control"))
		}
	}
}
