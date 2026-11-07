// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Handler returns the tile handler. It serves GET and HEAD requests for
// paths of the form /{s}/{z}/{x}/{y}.png (the leading slash is optional), so
// it is usually mounted with http.StripPrefix:
//
//	mux.Handle("/tiles/", http.StripPrefix("/tiles", cache.Handler()))
//
// Responses carry X-Tile-Cache: HIT (served from disk), MISS (fetched from
// upstream) or PLACEHOLDER (upstream failed; not cached by the client).
func (c *Cache) Handler() http.Handler {
	return http.HandlerFunc(c.serveTile)
}

func (c *Cache) serveTile(w http.ResponseWriter, r *http.Request) {
	t, s, status := c.parsePath(r.URL.Path)
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	if data, mod, ok := c.readTile(t); ok {
		if c.isStale(mod) {
			c.refresh(t, s)
		}
		c.respond(w, r, data, mod, "HIT")
		return
	}

	data, err := c.fetch(r.Context(), t, s)
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away
		}
		c.servePlaceholder(w)
		return
	}
	c.respond(w, r, data, time.Time{}, "MISS")
}

// parsePath parses "{s}/{z}/{x}/{y}.png". The status is 404 for paths of
// another shape or a zoom above MaxZoom, 400 for malformed or out-of-range
// values, and 200 otherwise.
func (c *Cache) parsePath(p string) (t tile, s string, status int) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) != 4 {
		return t, "", http.StatusNotFound
	}
	s = parts[0]
	ys, ok := strings.CutSuffix(parts[3], ".png")
	if !ok || s == "" || parts[1] == "" || parts[2] == "" || ys == "" {
		return t, "", http.StatusNotFound
	}
	z, okZ := parseCoord(parts[1])
	x, okX := parseCoord(parts[2])
	y, okY := parseCoord(ys)
	if !okZ || !okX || !okY {
		return t, "", http.StatusBadRequest
	}
	if z > c.maxZoom {
		return t, "", http.StatusNotFound
	}
	if n := 1 << z; x >= n || y >= n {
		return t, "", http.StatusBadRequest
	}
	if c.hasSubdomain && !slices.Contains(c.subdomains, s) {
		return t, "", http.StatusBadRequest
	}
	return tile{z: z, x: x, y: y}, s, http.StatusOK
}

// parseCoord accepts a non-negative decimal integer of at most 10 digits
// (enough for x and y at zoom 30) without sign or other characters.
func parseCoord(s string) (int, bool) {
	if len(s) == 0 || len(s) > 10 {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// readTile returns the cached tile and its modification time. A cached file
// that is not a PNG is removed.
func (c *Cache) readTile(t tile) ([]byte, time.Time, bool) {
	path := c.path(t)
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("reading cached tile", "tile", t.String(), "err", err)
		}
		return nil, time.Time{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, time.Time{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTileBytes+1))
	if err != nil {
		c.log.Warn("reading cached tile", "tile", t.String(), "err", err)
		return nil, time.Time{}, false
	}
	if len(data) > maxTileBytes || !isPNG(data) {
		c.log.Warn("removing invalid cached tile", "tile", t.String(), "size", len(data))
		// Only remove the file we read, not one a concurrent store has
		// just renamed into place.
		if cur, err := os.Stat(path); err == nil && os.SameFile(cur, info) {
			os.Remove(path)
		}
		return nil, time.Time{}, false
	}
	return data, info.ModTime(), true
}

// respond serves a tile. http.ServeContent answers If-None-Match (weak
// comparison, as RFC 9110 requires) and If-Modified-Since with 304, and
// handles HEAD and Range requests.
func (c *Cache) respond(w http.ResponseWriter, r *http.Request, data []byte, mod time.Time, source string) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", c.cacheControl)
	h.Set("ETag", etag(data))
	h.Set("X-Tile-Cache", source)
	http.ServeContent(w, r, "", mod, bytes.NewReader(data))
}

func (c *Cache) servePlaceholder(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(c.placeholder)))
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Tile-Cache", "PLACEHOLDER")
	w.WriteHeader(http.StatusOK)
	w.Write(c.placeholder)
}

// etag returns a strong entity tag derived from the tile's content, so it
// stays valid across a refresh that returns identical bytes.
func etag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
