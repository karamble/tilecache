// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := func(mod func(*Options)) Options {
		o := Options{Dir: filepath.Join(dir, "cache"), UserAgent: testUA}
		mod(&o)
		return o
	}
	tests := []struct {
		name string
		opts Options
	}{
		{"missing Dir", opts(func(o *Options) { o.Dir = "" })},
		{"missing UserAgent", opts(func(o *Options) { o.UserAgent = "" })},
		{"blank UserAgent", opts(func(o *Options) { o.UserAgent = "  " })},
		{"UserAgent with line break", opts(func(o *Options) { o.UserAgent = "Mozilla/5.0\r\nX-Injected: 1" })},
		{"Upstream without {y}", opts(func(o *Options) { o.Upstream = "https://tiles.invalid/{z}/{x}.png" })},
		{"Upstream not http", opts(func(o *Options) { o.Upstream = "ftp://tiles.invalid/{z}/{x}/{y}.png" })},
		{"Upstream relative", opts(func(o *Options) { o.Upstream = "/{z}/{x}/{y}.png" })},
		{"empty subdomain", opts(func(o *Options) { o.Subdomains = []string{"a", ""} })},
		{"subdomain with slash", opts(func(o *Options) { o.Subdomains = []string{"a/b"} })},
		{"negative MaxZoom", opts(func(o *Options) { o.MaxZoom = -1 })},
		{"MaxZoom above 30", opts(func(o *Options) { o.MaxZoom = 31 })},
		{"negative Concurrency", opts(func(o *Options) { o.Concurrency = -1 })},
		{"negative RateLimit", opts(func(o *Options) { o.RateLimit = -time.Second })},
		{"negative PreloadDelay", opts(func(o *Options) { o.PreloadDelay = -time.Second })},
		{"Placeholder not a PNG", opts(func(o *Options) { o.Placeholder = []byte("nope") })},
		{"Dir below a file", opts(func(o *Options) { o.Dir = filepath.Join(file, "cache") })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.opts)
			if err == nil {
				c.Close()
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "tiles")
	c, err := New(Options{Dir: dir, UserAgent: testUA})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("cache directory not created: %v", err)
	}
	if c.upstream != DefaultUpstream || !c.hasSubdomain {
		t.Errorf("upstream = %q", c.upstream)
	}
	if !slices.Equal(c.subdomains, []string{"a", "b", "c"}) {
		t.Errorf("subdomains = %v", c.subdomains)
	}
	if c.maxZoom != 19 || c.maxAge != 30*24*time.Hour || cap(c.sem) != 2 || c.limit.interval != 0 || c.preloadDelay != 250*time.Millisecond {
		t.Errorf("defaults: MaxZoom %d, MaxAge %v, Concurrency %d, RateLimit %v, PreloadDelay %v",
			c.maxZoom, c.maxAge, cap(c.sem), c.limit.interval, c.preloadDelay)
	}
	if c.client.Timeout != 15*time.Second {
		t.Errorf("client timeout = %v, want 15s", c.client.Timeout)
	}
	if ua, ok := c.client.Transport.(*uaTransport); !ok || ua.userAgent != testUA {
		t.Errorf("client transport = %#v, want the User-Agent enforcing wrapper", c.client.Transport)
	}
	if !isPNG(c.placeholder) {
		t.Error("default placeholder is not a PNG")
	}
}

func TestStats(t *testing.T) {
	u := newUpstream(t, nil)
	c := newTestCache(t, Options{Upstream: u.template()})
	if n, size, err := c.Stats(); err != nil || n != 0 || size != 0 {
		t.Fatalf("empty cache: Stats = %d, %d, %v", n, size, err)
	}

	var want int64
	for _, p := range []string{"/a/1/0/0.png", "/a/1/1/0.png", "/a/2/3/3.png"} {
		want += int64(get(c.Handler(), http.MethodGet, p).Body.Len())
	}
	// Neither a leftover temporary file nor an unrelated file is a tile.
	if err := os.WriteFile(filepath.Join(c.dir, "1", "0", ".tile-1.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, size, err := c.Stats()
	if err != nil || n != 3 || size != want {
		t.Errorf("Stats = %d, %d, %v; want 3, %d, nil", n, size, err, want)
	}
}

func TestCloseWaitsForBackgroundWork(t *testing.T) {
	var entered, active, finished atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered.Add(1)
		active.Add(1)
		defer active.Add(-1)
		<-r.Context().Done()
		// Widen the window in which a Close that does not wait would
		// return before this request is over.
		time.Sleep(20 * time.Millisecond)
		finished.Add(1)
		return nil, r.Context().Err()
	})
	now := time.Now()
	c := newTestCache(t, Options{
		Upstream:     "http://tiles.invalid/{s}/{z}/{x}/{y}.png",
		HTTPClient:   &http.Client{Transport: transport},
		Concurrency:  4,
		PreloadDelay: time.Millisecond,
	})
	h := c.Handler()

	// A request-driven fetch, a background refresh and a preload job, all
	// stuck at the upstream.
	missDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { missDone <- get(h, http.MethodGet, "/a/3/0/0.png") }()

	stale := tile{3, 1, 1}
	putTile(t, c, stale, testPNG("stale"), now.Add(-40*24*time.Hour))
	if rec := get(h, http.MethodGet, "/a/3/1/1.png"); rec.Header().Get("X-Tile-Cache") != "HIT" {
		t.Fatalf("stale tile: X-Tile-Cache %q, want HIT", rec.Header().Get("X-Tile-Cache"))
	}

	job, err := c.Preload(t.Context(), probeRegion(10, 10))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three upstream requests", func() bool { return entered.Load() == 3 })

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if active.Load() != 0 || finished.Load() != 3 {
		t.Errorf("Close returned with %d upstream requests still running", active.Load())
	}
	select {
	case <-job.Done():
	default:
		t.Error("Close returned before the preload job stopped")
	}
	if job.Status().Running {
		t.Error("preload job still marked running")
	}
	if n := c.flights.inFlight(); n != 0 {
		t.Errorf("%d flights left after Close", n)
	}
	select {
	case rec := <-missDone:
		assertPlaceholder(t, c, rec)
	case <-time.After(10 * time.Second):
		t.Fatal("request waiting on a fetch did not return after Close")
	}

	// A closed cache still serves what it has, and nothing else.
	if _, err := c.Preload(t.Context(), probeRegion(10, 10)); !errors.Is(err, ErrClosed) {
		t.Errorf("Preload after Close: %v, want ErrClosed", err)
	}
	if rec := get(h, http.MethodGet, "/a/3/1/1.png"); rec.Header().Get("X-Tile-Cache") != "HIT" {
		t.Errorf("cached tile after Close: X-Tile-Cache %q, want HIT", rec.Header().Get("X-Tile-Cache"))
	}
	assertPlaceholder(t, c, get(h, http.MethodGet, "/a/3/2/2.png"))
	if got := entered.Load(); got != 3 {
		t.Errorf("upstream requests after Close: %d, want none", got-3)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
