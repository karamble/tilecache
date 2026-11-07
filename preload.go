// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

const (
	maxPreloadZoom     = 16
	maxPreloadRadiusKm = 50
	maxPreloadTiles    = 10000
	maxMercatorLat     = 85.0511287798066
	kmPerDegreeLat     = 111.32
)

// Region is the area a preload job fetches: every tile intersecting the
// bounding box of a circle of RadiusKm around Lat/Lng, at each zoom level
// from MinZoom to MaxZoom.
//
// RadiusKm must be in (0, 50], the zoom levels in 0..16 (and not above the
// cache's MaxZoom) with MinZoom <= MaxZoom, and the region may cover at most
// 10000 tiles.
type Region struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	RadiusKm float64 `json:"radius_km"`
	MinZoom  int     `json:"min_zoom"`
	MaxZoom  int     `json:"max_zoom"`
}

// JobStatus is a snapshot of a preload job's progress. Done counts the tiles
// processed so far: Cached (already on disk and fresh), Fetched and Failed.
// Err is set when the job stopped early, for example because it was
// cancelled. The timestamps come from Options.Clock.
type JobStatus struct {
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	Cached     int       `json:"cached"`
	Fetched    int       `json:"fetched"`
	Failed     int       `json:"failed"`
	Running    bool      `json:"running"`
	Err        string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Job is a running or finished preload job.
type Job struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	status JobStatus
}

// Status returns a snapshot of the job's progress.
func (j *Job) Status() JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// Cancel stops the job. It returns immediately; wait on Done to know when
// the job has stopped.
func (j *Job) Cancel() { j.cancel() }

// Done returns a channel that is closed when the job has stopped.
func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) update(f func(*JobStatus)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f(&j.status)
}

func (j *Job) running() bool { return j.Status().Running }

// Preload starts a background job that fetches every tile of r that is not
// cached yet (or is stale), one at a time, pausing max(PreloadDelay,
// RateLimit) between upstream fetches. Only one job runs at a time. The job
// stops when it is done, when ctx is cancelled, on Job.Cancel or on Close.
//
// Bulk downloading is forbidden by the OpenStreetMap tile usage policy; only
// preload from tile servers that allow it.
func (c *Cache) Preload(ctx context.Context, r Region) (*Job, error) {
	if c.disablePreload {
		return nil, ErrPreloadDisabled
	}
	ranges, total, err := c.planRegion(r)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.job != nil && c.job.running() {
		return nil, ErrPreloadRunning
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	job := &Job{
		cancel: cancel,
		done:   make(chan struct{}),
		status: JobStatus{Total: total, Running: true, StartedAt: c.clock()},
	}
	c.job = job
	c.log.Info("tile preload started", "lat", r.Lat, "lng", r.Lng, "radius_km", r.RadiusKm,
		"min_zoom", r.MinZoom, "max_zoom", r.MaxZoom, "tiles", total)
	c.wg.Go(func() {
		defer stop()
		defer cancel()
		c.runPreload(jobCtx, job, ranges)
	})
	return job, nil
}

// tileRange is the inclusive block of tiles a region covers at zoom z.
type tileRange struct{ z, x0, x1, y0, y1 int }

func (r tileRange) count() int { return (r.x1 - r.x0 + 1) * (r.y1 - r.y0 + 1) }

// planRegion validates r and returns the tile blocks it covers per zoom
// level and their total count.
func (c *Cache) planRegion(r Region) ([]tileRange, int, error) {
	switch {
	case !(r.RadiusKm > 0 && r.RadiusKm <= maxPreloadRadiusKm):
		return nil, 0, fmt.Errorf("%w: radius_km must be in (0, %d], got %v", ErrInvalidRegion, maxPreloadRadiusKm, r.RadiusKm)
	case !(r.Lat >= -maxMercatorLat && r.Lat <= maxMercatorLat):
		return nil, 0, fmt.Errorf("%w: lat must be within ±%.4f, got %v", ErrInvalidRegion, maxMercatorLat, r.Lat)
	case !(r.Lng >= -180 && r.Lng <= 180):
		return nil, 0, fmt.Errorf("%w: lng must be within ±180, got %v", ErrInvalidRegion, r.Lng)
	case r.MinZoom < 0 || r.MaxZoom > maxPreloadZoom || r.MinZoom > r.MaxZoom:
		return nil, 0, fmt.Errorf("%w: zoom levels must satisfy 0 <= min_zoom <= max_zoom <= %d, got %d..%d",
			ErrInvalidRegion, maxPreloadZoom, r.MinZoom, r.MaxZoom)
	case r.MaxZoom > c.maxZoom:
		return nil, 0, fmt.Errorf("%w: max_zoom %d is above the cache's MaxZoom %d", ErrInvalidRegion, r.MaxZoom, c.maxZoom)
	}

	dLat := r.RadiusKm / kmPerDegreeLat
	dLng := r.RadiusKm / (kmPerDegreeLat * math.Cos(r.Lat*math.Pi/180))
	north := min(r.Lat+dLat, maxMercatorLat)
	south := max(r.Lat-dLat, -maxMercatorLat)
	west := max(r.Lng-dLng, -180)
	east := min(r.Lng+dLng, 180)

	var ranges []tileRange
	total := 0
	for z := r.MinZoom; z <= r.MaxZoom; z++ {
		x0, y0 := lngLatToTile(west, north, z)
		x1, y1 := lngLatToTile(east, south, z)
		tr := tileRange{z: z, x0: x0, x1: x1, y0: y0, y1: y1}
		total += tr.count()
		if total > maxPreloadTiles {
			return nil, 0, fmt.Errorf("%w: region covers more than %d tiles; reduce the radius or zoom range",
				ErrInvalidRegion, maxPreloadTiles)
		}
		ranges = append(ranges, tr)
	}
	return ranges, total, nil
}

// lngLatToTile returns the Web Mercator tile containing a point at zoom z.
func lngLatToTile(lng, lat float64, z int) (x, y int) {
	n := float64(int(1) << z)
	latRad := lat * math.Pi / 180
	x = int(math.Floor((lng + 180) / 360 * n))
	y = int(math.Floor((1 - math.Asinh(math.Tan(latRad))/math.Pi) / 2 * n))
	last := int(n) - 1
	return min(max(x, 0), last), min(max(y, 0), last)
}

func (c *Cache) runPreload(ctx context.Context, j *Job, ranges []tileRange) {
	var stopErr error
	defer func() {
		j.update(func(st *JobStatus) {
			st.Running = false
			st.FinishedAt = c.clock()
			if stopErr != nil {
				st.Err = stopErr.Error()
			}
		})
		st := j.Status()
		c.log.Info("tile preload finished", "total", st.Total, "cached", st.Cached,
			"fetched", st.Fetched, "failed", st.Failed, "err", st.Err)
		close(j.done)
	}()

	delay := max(c.preloadDelay, c.limit.interval)
	fetched := 0
	for _, tr := range ranges {
		for x := tr.x0; x <= tr.x1; x++ {
			for y := tr.y0; y <= tr.y1; y++ {
				if stopErr = ctx.Err(); stopErr != nil {
					return
				}
				t := tile{z: tr.z, x: x, y: y}
				if _, mod, ok := c.readTile(t); ok && !c.isStale(mod) {
					j.update(func(st *JobStatus) { st.Done++; st.Cached++ })
					continue
				}
				if fetched > 0 {
					if stopErr = sleep(ctx, delay); stopErr != nil {
						return
					}
				}
				s := ""
				if c.hasSubdomain {
					s = c.subdomains[fetched%len(c.subdomains)]
				}
				fetched++
				_, err := c.fetch(ctx, t, s)
				if err != nil && ctx.Err() != nil {
					stopErr = ctx.Err()
					return
				}
				j.update(func(st *JobStatus) {
					st.Done++
					if err != nil {
						st.Failed++
					} else {
						st.Fetched++
					}
				})
			}
		}
	}
}

func (c *Cache) currentJob() *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.job
}

// PreloadHandlers returns JSON endpoints for preloading, to be mounted by the
// caller, for example:
//
//	start, status, cancel := cache.PreloadHandlers()
//	mux.Handle("POST /tiles/preload", start)
//	mux.Handle("GET /tiles/preload/status", status)
//	mux.Handle("POST /tiles/preload/cancel", cancel)
//
// start takes a POST with a Region as JSON ({"lat":..,"lng":..,
// "radius_km":..,"min_zoom":..,"max_zoom":..}) and answers 202 with the new
// job's JobStatus, 400 for an invalid region, 409 while a job runs and 403
// when preloading is disabled. status answers GET with the JobStatus of the
// current or last job. cancel takes a POST, cancels the running job and
// answers 202 with its status, or 409 when no job runs. Errors are JSON
// objects with an "error" field.
func (c *Cache) PreloadHandlers() (start, status, cancel http.Handler) {
	start = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		var reg Region
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&reg); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		job, err := c.Preload(context.WithoutCancel(r.Context()), reg)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, job.Status())
		case errors.Is(err, ErrInvalidRegion):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrPreloadRunning):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrPreloadDisabled):
			writeJSONError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, ErrClosed):
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
	})
	status = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET, HEAD")
			return
		}
		var st JobStatus
		if job := c.currentJob(); job != nil {
			st = job.Status()
		}
		writeJSON(w, http.StatusOK, st)
	})
	cancel = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		job := c.currentJob()
		if job == nil || !job.running() {
			writeJSONError(w, http.StatusConflict, "no preload in progress")
			return
		}
		job.Cancel()
		writeJSON(w, http.StatusAccepted, job.Status())
	})
	return start, status, cancel
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSONError(w, http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
