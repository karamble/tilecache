// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fetch returns tile t from upstream, joining the fetch already in flight for
// it if there is one. It gives up when ctx is done; the upstream request is
// only cancelled once every caller waiting for it has given up.
func (c *Cache) fetch(ctx context.Context, t tile, s string) ([]byte, error) {
	f := c.flights.join(t, c.fetchFunc(t, s))
	defer c.flights.leave(f)
	select {
	case <-f.done:
		return f.data, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// refresh starts a background re-fetch of a stale tile unless one is already
// in flight or refreshes are paused after an upstream failure.
func (c *Cache) refresh(t tile, s string) {
	if c.clock().UnixNano() < c.refreshPausedUntil.Load() {
		return
	}
	if c.flights.start(t, c.fetchFunc(t, s)) {
		c.log.Debug("refreshing stale tile", "tile", t.String())
	}
}

// fetchFunc returns the work of one flight: download t, validate it and store
// it on disk before handing it to the waiters, so a tile has been stored by
// the time any response for it is written.
func (c *Cache) fetchFunc(t tile, s string) flightFunc {
	return func(ctx context.Context) ([]byte, error) {
		// A flight that just finished may have stored the tile after the
		// caller found it missing.
		if data, mod, ok := c.readTile(t); ok && !c.isStale(mod) {
			return data, nil
		}
		data, err := c.download(ctx, t, s)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("fetching tile", "tile", t.String(), "err", err)
				c.refreshPausedUntil.Store(c.clock().Add(refreshBackoff).UnixNano())
			}
			return nil, err
		}
		if err := c.store(t, data); err != nil {
			c.log.Warn("storing tile", "tile", t.String(), "err", err)
		}
		return data, nil
	}
}

// download performs one upstream request within the Concurrency and
// RateLimit bounds and returns the body if it is a PNG.
func (c *Cache) download(ctx context.Context, t tile, s string) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	if err := c.limit.wait(ctx); err != nil {
		return nil, err
	}

	u := c.tileURL(t, s)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "image/png,image/*;q=0.8")
	c.log.Debug("fetching tile from upstream", "tile", t.String(), "url", u)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Drain a little so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading body: %w", u, err)
	}
	if len(data) > maxTileBytes {
		return nil, fmt.Errorf("GET %s: tile exceeds %d bytes", u, maxTileBytes)
	}
	if !isPNG(data) {
		return nil, fmt.Errorf("GET %s: response is not a PNG", u)
	}
	return data, nil
}

func (c *Cache) tileURL(t tile, s string) string {
	return strings.NewReplacer(
		"{s}", s,
		"{z}", strconv.Itoa(t.z),
		"{x}", strconv.Itoa(t.x),
		"{y}", strconv.Itoa(t.y),
	).Replace(c.upstream)
}

// store writes a tile atomically: into a temporary file in the target
// directory, synced to disk, then renamed over the final path. Readers see
// either the previous complete file or the new one, and a failed write
// leaves no temporary file behind. The file's modification time is set from
// the Clock, which is what staleness is measured against.
func (c *Cache) store(t tile, data []byte) (err error) {
	path := c.path(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tile-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	now := c.clock()
	if err = os.Chtimes(tmp.Name(), now, now); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// limiter spaces events at least interval apart. Each caller reserves the
// next free slot and sleeps until it.
type limiter struct {
	interval time.Duration

	mu   sync.Mutex
	next time.Time
}

func (l *limiter) wait(ctx context.Context) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(l.interval)
	l.mu.Unlock()
	return sleep(ctx, at.Sub(now))
}

// sleep pauses for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// uaTransport sets the configured User-Agent on every request that passes
// through the client, including redirect follow-ups, before handing it to
// the client's own transport.
type uaTransport struct {
	base      http.RoundTripper
	userAgent string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != t.userAgent {
		req = req.Clone(req.Context()) // a RoundTripper must not modify its request
		req.Header.Set("User-Agent", t.userAgent)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
