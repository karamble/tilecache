// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A tile at zoom 16 whose centre is used as a preload target. A 10 m radius
// around it stays inside one tile at every zoom up to 16, so the tiles a
// region covers are known without the code under test.
const (
	probeX16 = 34567
	probeY16 = 22222
)

func probeTile(z int) tile { return tile{z, probeX16 >> (16 - z), probeY16 >> (16 - z)} }

// tileCenter returns the centre of a Web Mercator tile.
func tileCenter(x, y, z int) (lat, lng float64) {
	n := float64(int(1) << z)
	lng = (float64(x)+0.5)/n*360 - 180
	lat = math.Atan(math.Sinh(math.Pi*(1-2*(float64(y)+0.5)/n))) * 180 / math.Pi
	return lat, lng
}

func probeRegion(minZoom, maxZoom int) Region {
	lat, lng := tileCenter(probeX16, probeY16, 16)
	return Region{Lat: lat, Lng: lng, RadiusKm: 0.01, MinZoom: minZoom, MaxZoom: maxZoom}
}

// putTile stores a tile directly on disk with the given modification time.
func putTile(t *testing.T, c *Cache, k tile, data []byte, mod time.Time) {
	t.Helper()
	path := c.path(k)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func waitDone(t *testing.T, j *Job) {
	t.Helper()
	select {
	case <-j.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the preload job")
	}
}

// blockingUpstream holds every request until the client gives up on it.
func blockingUpstream(t *testing.T) *upstream {
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
}

func TestPlanRegion(t *testing.T) {
	c := newTestCache(t, Options{Upstream: "http://tiles.invalid/{z}/{x}/{y}.png"})

	// Around 0,0 a small radius covers the single zoom-0 tile and the four
	// zoom-1 tiles that meet there.
	ranges, total, err := c.planRegion(Region{Lat: 0, Lng: 0, RadiusKm: 1, MinZoom: 0, MaxZoom: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []tileRange{{z: 0, x0: 0, x1: 0, y0: 0, y1: 0}, {z: 1, x0: 0, x1: 1, y0: 0, y1: 1}}
	if total != 5 || !slices.Equal(ranges, want) {
		t.Errorf("planRegion = %v, %d; want %v, 5", ranges, total, want)
	}

	_, total, err = c.planRegion(probeRegion(0, 16))
	if err != nil || total != 17 {
		t.Errorf("probe region: %d tiles, err %v; want one tile per zoom level (17)", total, err)
	}

	low := newTestCache(t, Options{Upstream: "http://tiles.invalid/{z}/{x}/{y}.png", MaxZoom: 12})
	invalid := []struct {
		name  string
		cache *Cache
		r     Region
	}{
		{"zero radius", c, Region{Lat: 50, Lng: 8, RadiusKm: 0, MinZoom: 1, MaxZoom: 2}},
		{"negative radius", c, Region{Lat: 50, Lng: 8, RadiusKm: -1, MinZoom: 1, MaxZoom: 2}},
		{"radius above 50 km", c, Region{Lat: 50, Lng: 8, RadiusKm: 50.001, MinZoom: 1, MaxZoom: 2}},
		{"NaN radius", c, Region{Lat: 50, Lng: 8, RadiusKm: math.NaN(), MinZoom: 1, MaxZoom: 2}},
		{"negative min zoom", c, Region{Lat: 50, Lng: 8, RadiusKm: 1, MinZoom: -1, MaxZoom: 2}},
		{"max zoom above 16", c, Region{Lat: 50, Lng: 8, RadiusKm: 1, MinZoom: 1, MaxZoom: 17}},
		{"min above max", c, Region{Lat: 50, Lng: 8, RadiusKm: 1, MinZoom: 5, MaxZoom: 4}},
		{"max zoom above cache MaxZoom", low, Region{Lat: 50, Lng: 8, RadiusKm: 1, MinZoom: 1, MaxZoom: 13}},
		{"latitude beyond Web Mercator", c, Region{Lat: 86, Lng: 8, RadiusKm: 1, MinZoom: 1, MaxZoom: 2}},
		{"NaN latitude", c, Region{Lat: math.NaN(), Lng: 8, RadiusKm: 1, MinZoom: 1, MaxZoom: 2}},
		{"longitude out of range", c, Region{Lat: 50, Lng: -181, RadiusKm: 1, MinZoom: 1, MaxZoom: 2}},
		{"more than 10000 tiles", c, Region{Lat: 50, Lng: 8, RadiusKm: 50, MinZoom: 0, MaxZoom: 16}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := tt.cache.planRegion(tt.r); !errors.Is(err, ErrInvalidRegion) {
				t.Errorf("planRegion(%+v) error = %v, want ErrInvalidRegion", tt.r, err)
			}
			if _, err := tt.cache.Preload(t.Context(), tt.r); !errors.Is(err, ErrInvalidRegion) {
				t.Errorf("Preload(%+v) error = %v, want ErrInvalidRegion", tt.r, err)
			}
		})
	}
}

func TestPreloadCounts(t *testing.T) {
	now := time.Now()
	clk := newFakeClock(now)
	failing := probeTile(14)
	failPath := fmt.Sprintf("/%d/%d/%d.png", failing.z, failing.x, failing.y)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, failPath) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write(testPNG(r.URL.Path))
	})
	c := newTestCache(t, Options{Upstream: u.template(), Clock: clk.now, PreloadDelay: time.Millisecond})

	putTile(t, c, probeTile(10), testPNG("cached 10"), now)
	putTile(t, c, probeTile(12), testPNG("cached 12"), now.Add(-time.Hour))
	putTile(t, c, probeTile(13), testPNG("stale 13"), now.Add(-40*24*time.Hour))

	job, err := c.Preload(t.Context(), probeRegion(10, 14))
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, job)

	st := job.Status()
	if st.Total != 5 || st.Done != 5 || st.Cached != 2 || st.Fetched != 2 || st.Failed != 1 || st.Running || st.Err != "" {
		t.Errorf("status = %+v; want Total 5, Done 5, Cached 2, Fetched 2, Failed 1, not running, no error", st)
	}
	if !st.StartedAt.Equal(clk.now()) || !st.FinishedAt.Equal(clk.now()) {
		t.Errorf("timestamps %v / %v, want the injected clock %v", st.StartedAt, st.FinishedAt, clk.now())
	}

	// Tiles 11, 13 (stale) and 14 went upstream, rotating subdomains.
	var paths []string
	for _, r := range u.requests() {
		paths = append(paths, r.path)
		if r.userAgent != testUA {
			t.Errorf("%s: User-Agent %q, want %q", r.path, r.userAgent, testUA)
		}
	}
	wantPaths := []string{
		fmt.Sprintf("/a/11/%d/%d.png", probeTile(11).x, probeTile(11).y),
		fmt.Sprintf("/b/13/%d/%d.png", probeTile(13).x, probeTile(13).y),
		"/c" + failPath,
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("upstream paths = %v, want %v", paths, wantPaths)
	}

	for z := 10; z <= 13; z++ {
		if _, err := os.Stat(c.path(probeTile(z))); err != nil {
			t.Errorf("zoom %d tile missing after preload: %v", z, err)
		}
	}
	if onDisk, _ := os.ReadFile(c.path(probeTile(13))); bytes.Equal(onDisk, testPNG("stale 13")) {
		t.Error("stale tile was not refreshed by the preload")
	}
	if _, err := os.Stat(c.path(failing)); !os.IsNotExist(err) {
		t.Errorf("failed tile exists on disk (err %v)", err)
	}
	assertNoTempFiles(t, c.dir)
}

func TestPreloadSpacing(t *testing.T) {
	tests := []struct {
		name         string
		preloadDelay time.Duration
		rateLimit    time.Duration
		minGap       time.Duration
	}{
		{"PreloadDelay", 20 * time.Millisecond, 0, 20 * time.Millisecond},
		{"RateLimit when larger", time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, nil)
			c := newTestCache(t, Options{Upstream: u.template(), PreloadDelay: tt.preloadDelay, RateLimit: tt.rateLimit})
			start := time.Now()
			job, err := c.Preload(t.Context(), probeRegion(10, 13))
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, job)
			reqs := u.requests()
			if len(reqs) != 4 {
				t.Fatalf("upstream requests = %d, want 4", len(reqs))
			}
			for i := 1; i < len(reqs); i++ {
				if gap := reqs[i].at.Sub(reqs[i-1].at); gap < tt.minGap-time.Millisecond {
					t.Errorf("gap between preload fetches %d and %d = %v, want at least %v", i-1, i, gap, tt.minGap)
				}
			}
			if span := reqs[3].at.Sub(start); span < 3*tt.minGap {
				t.Errorf("four preload fetches took %v, want at least %v", span, 3*tt.minGap)
			}
		})
	}
}

func TestPreloadCancel(t *testing.T) {
	region := Region{Lat: 50, Lng: 8, RadiusKm: 2, MinZoom: 14, MaxZoom: 15}

	t.Run("Job.Cancel", func(t *testing.T) {
		u := blockingUpstream(t)
		c := newTestCache(t, Options{Upstream: u.template(), PreloadDelay: time.Millisecond})
		job, err := c.Preload(t.Context(), region)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the first preload fetch", func() bool { return u.count() == 1 })
		job.Cancel()
		waitDone(t, job)
		assertCancelled(t, job)
		waitFor(t, "the upstream fetch to be abandoned", func() bool { return c.flights.inFlight() == 0 })
		if got := u.count(); got != 1 {
			t.Errorf("upstream requests = %d, want 1", got)
		}
	})

	t.Run("context", func(t *testing.T) {
		u := blockingUpstream(t)
		c := newTestCache(t, Options{Upstream: u.template(), PreloadDelay: time.Millisecond})
		ctx, cancel := context.WithCancel(t.Context())
		job, err := c.Preload(ctx, region)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the first preload fetch", func() bool { return u.count() == 1 })
		cancel()
		waitDone(t, job)
		assertCancelled(t, job)
	})
}

func assertCancelled(t *testing.T, job *Job) {
	t.Helper()
	st := job.Status()
	if st.Running || !strings.Contains(st.Err, "canceled") {
		t.Errorf("status = %+v; want stopped with a cancellation error", st)
	}
	if st.Total < 2 || st.Done != 0 || st.Fetched != 0 || st.Failed != 0 {
		t.Errorf("status = %+v; want an early stop with nothing counted", st)
	}
	if st.FinishedAt.IsZero() {
		t.Error("FinishedAt not set")
	}
}

func TestPreloadOneAtATime(t *testing.T) {
	u := blockingUpstream(t)
	c := newTestCache(t, Options{Upstream: u.template(), PreloadDelay: time.Millisecond})
	region := probeRegion(10, 12)

	first, err := c.Preload(t.Context(), region)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Preload(t.Context(), region); !errors.Is(err, ErrPreloadRunning) {
		t.Fatalf("second Preload error = %v, want ErrPreloadRunning", err)
	}
	first.Cancel()
	waitDone(t, first)

	second, err := c.Preload(t.Context(), region)
	if err != nil {
		t.Fatalf("Preload after the first job stopped: %v", err)
	}
	second.Cancel()
	waitDone(t, second)
}

func TestPreloadDisabled(t *testing.T) {
	c := newTestCache(t, Options{Upstream: "http://tiles.invalid/{z}/{x}/{y}.png", DisablePreload: true})
	if _, err := c.Preload(t.Context(), probeRegion(10, 12)); !errors.Is(err, ErrPreloadDisabled) {
		t.Errorf("Preload error = %v, want ErrPreloadDisabled", err)
	}
	start, _, _ := c.PreloadHandlers()
	if rec := post(start, `{"lat":50,"lng":8,"radius_km":1,"min_zoom":10,"max_zoom":12}`); rec.Code != http.StatusForbidden {
		t.Errorf("start: status %d, want 403", rec.Code)
	}
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestPreloadHandlers(t *testing.T) {
	u := blockingUpstream(t)
	c := newTestCache(t, Options{Upstream: u.template(), PreloadDelay: time.Millisecond})
	start, status, cancel := c.PreloadHandlers()
	const valid = `{"lat":50,"lng":8,"radius_km":2,"min_zoom":14,"max_zoom":15}`

	if rec := get(start, http.MethodGet, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET start: status %d, want 405", rec.Code)
	}
	if rec := post(start, "{"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status %d, want 400", rec.Code)
	}
	rec := post(start, `{"lat":50,"lng":8,"radius_km":0,"min_zoom":14,"max_zoom":15}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid region: status %d, want 400", rec.Code)
	}
	if e := decodeJSON[map[string]string](t, rec); !strings.Contains(e["error"], "radius_km") {
		t.Errorf("invalid region error = %q, want it to name radius_km", e["error"])
	}

	if st := decodeJSON[JobStatus](t, get(status, http.MethodGet, "/")); st.Running || st.Total != 0 {
		t.Errorf("status before any job = %+v, want zero", st)
	}
	if rec := post(cancel, ""); rec.Code != http.StatusConflict {
		t.Errorf("cancel without job: status %d, want 409", rec.Code)
	}

	rec = post(start, valid)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status %d (%s), want 202", rec.Code, rec.Body.String())
	}
	if st := decodeJSON[JobStatus](t, rec); !st.Running || st.Total < 2 || st.StartedAt.IsZero() {
		t.Errorf("start response = %+v, want a running job", st)
	}
	if rec := post(start, valid); rec.Code != http.StatusConflict {
		t.Errorf("second start: status %d, want 409", rec.Code)
	}
	if st := decodeJSON[JobStatus](t, get(status, http.MethodGet, "/")); !st.Running {
		t.Errorf("status while running = %+v", st)
	}

	if rec := post(cancel, ""); rec.Code != http.StatusAccepted {
		t.Errorf("cancel: status %d, want 202", rec.Code)
	}
	waitDone(t, c.currentJob())
	if st := decodeJSON[JobStatus](t, get(status, http.MethodGet, "/")); st.Running || !strings.Contains(st.Err, "canceled") {
		t.Errorf("status after cancel = %+v", st)
	}
	if rec := post(cancel, ""); rec.Code != http.StatusConflict {
		t.Errorf("cancel after the job stopped: status %d, want 409", rec.Code)
	}
}
