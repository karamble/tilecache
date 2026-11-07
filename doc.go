// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package tilecache is an on-request cache for raster map tiles.
//
// A Cache is an http.Handler for paths of the form /{s}/{z}/{x}/{y}.png. A
// tile found on disk (Dir/{z}/{x}/{y}.png) is served from there; otherwise it
// is fetched from an upstream tile server, validated as a PNG, stored
// atomically and served. Concurrent requests for the same tile share one
// upstream fetch, and upstream traffic is bounded by Options.Concurrency and
// Options.RateLimit. Every upstream request carries Options.UserAgent.
//
// Cached tiles older than Options.MaxAge are still served, and refreshed in
// the background. When a tile is neither cached nor fetchable, a placeholder
// tile is served with Cache-Control: no-cache so clients retry later.
//
// Responses carry an X-Tile-Cache header of HIT, MISS or PLACEHOLDER, a
// strong content-derived ETag and Cache-Control: public, max-age=86400 (Options.CacheControl
// changes it; use a private value behind a login).
//
// Preload fetches every tile of a Region in a background Job, for tile
// servers that permit bulk downloads.
//
// # OpenStreetMap tile usage policy
//
// The default upstream is tile.openstreetmap.org, whose usage policy
// (https://operations.osmfoundation.org/policies/tiles/) allows tiles to be
// fetched on demand for display and requires them to be cached, but forbids
// bulk downloading. Do not use Preload against tile.openstreetmap.org; point
// Options.Upstream at a tile server that allows it, or set
// Options.DisablePreload.
package tilecache
