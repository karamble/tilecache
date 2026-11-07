// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"context"
	"sync"
)

// flightFunc produces a tile. Its context is cancelled when nobody waits for
// the result any more or when the cache closes.
type flightFunc func(ctx context.Context) ([]byte, error)

// flight is one in-progress fetch shared by every caller asking for the same
// tile. data and err are set before done is closed.
type flight struct {
	key  tile
	done chan struct{}
	data []byte
	err  error

	// Guarded by flightGroup.mu.
	refs       int  // callers currently waiting
	background bool // started by start; not cancelled when refs drops to zero
	cancel     context.CancelFunc
}

// flightGroup deduplicates concurrent fetches of the same tile. Unlike a
// classic singleflight, the work runs in its own goroutine so that a caller
// can stop waiting without cancelling the fetch for the others; the fetch is
// cancelled only when the last waiter leaves.
type flightGroup struct {
	ctx   context.Context   // parent of every flight's context
	spawn func(func()) bool // starts a tracked goroutine; false once closed

	mu sync.Mutex
	m  map[tile]*flight
}

// join registers the caller as a waiter on the flight for k, starting fn if
// no flight is in progress. The caller must call leave exactly once when it
// stops waiting.
func (g *flightGroup) join(k tile, fn flightFunc) *flight {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.m[k]
	if f == nil {
		f = g.startLocked(k, fn, false)
	}
	f.refs++
	return f
}

// start begins a background flight for k unless one is already in progress.
// It reports whether a flight was started.
func (g *flightGroup) start(k tile, fn flightFunc) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.m[k]; ok {
		return false
	}
	f := g.startLocked(k, fn, true)
	return f.err == nil
}

func (g *flightGroup) startLocked(k tile, fn flightFunc, background bool) *flight {
	ctx, cancel := context.WithCancel(g.ctx)
	f := &flight{key: k, done: make(chan struct{}), background: background, cancel: cancel}
	if !g.spawn(func() { g.run(ctx, f, fn) }) {
		cancel()
		f.err = ErrClosed
		close(f.done)
		return f
	}
	g.m[k] = f
	return f
}

func (g *flightGroup) run(ctx context.Context, f *flight, fn flightFunc) {
	data, err := fn(ctx)
	f.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m[f.key] == f {
		delete(g.m, f.key)
	}
	f.data, f.err = data, err
	close(f.done)
}

// leave unregisters a waiter. When the last waiter of a request-driven
// flight leaves before it finished, the flight is cancelled and forgotten so
// that a later caller starts a fresh one.
func (g *flightGroup) leave(f *flight) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f.refs--
	if f.refs > 0 || f.background {
		return
	}
	select {
	case <-f.done:
		return
	default:
	}
	f.cancel()
	if g.m[f.key] == f {
		delete(g.m, f.key)
	}
}
