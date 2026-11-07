// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultUserAgent is a regular desktop Chrome User-Agent string. Callers
// that have no reason to send anything else can pass it as
// Options.UserAgent.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// DefaultUpstream is the upstream URL template used when Options.Upstream is
// empty: the OpenStreetMap standard tile layer.
const DefaultUpstream = "https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"

const (
	defaultMaxZoom      = 19
	maxSupportedZoom    = 30
	defaultMaxAge       = 30 * 24 * time.Hour
	defaultConcurrency  = 2
	defaultPreloadDelay = 250 * time.Millisecond
	defaultHTTPTimeout  = 15 * time.Second

	// refreshBackoff is how long background refreshes of stale tiles are
	// suspended after an upstream fetch failed, so an unreachable or
	// throttling upstream is not hit again for every stale tile served.
	refreshBackoff = time.Minute

	// maxTileBytes bounds what is read from upstream and from disk.
	maxTileBytes = 4 << 20

	pngSignature = "\x89PNG\r\n\x1a\n"
)

var (
	// ErrClosed is returned by operations on a closed Cache.
	ErrClosed = errors.New("tilecache: cache closed")

	// ErrPreloadRunning is returned by Preload while another job runs.
	ErrPreloadRunning = errors.New("tilecache: a preload job is already running")

	// ErrPreloadDisabled is returned by Preload when Options.DisablePreload
	// is set.
	ErrPreloadDisabled = errors.New("tilecache: preload is disabled")

	// ErrInvalidRegion is wrapped by the errors Preload returns for a
	// Region it rejects.
	ErrInvalidRegion = errors.New("tilecache: invalid preload region")
)

// Options configures a Cache. Dir and UserAgent are required; every other
// field has a default that applies when it is left at its zero value.
type Options struct {
	// Dir is the cache root. Tiles are stored as Dir/{z}/{x}/{y}.png. It
	// is created if it does not exist.
	Dir string

	// Upstream is the tile URL template. It must contain {z}, {x} and {y}
	// and may contain {s} for a subdomain. Default: DefaultUpstream.
	Upstream string

	// Subdomains lists the accepted values of the {s} path segment, which
	// is passed through to Upstream. When Upstream has no {s}, any value
	// is accepted and ignored. Default: a, b, c.
	Subdomains []string

	// UserAgent is sent on every upstream request, replacing whatever the
	// HTTP client would send by default. Required.
	UserAgent string

	// MaxZoom is the highest zoom level served; requests above it get 404.
	// Default 19, at most 30.
	MaxZoom int

	// MaxAge is the age after which a cached tile is refreshed in the
	// background when it is requested. The stale tile is still served.
	// Default 30 days; a negative value disables refreshing.
	MaxAge time.Duration

	// Concurrency is the maximum number of parallel upstream fetches.
	// Default 2.
	Concurrency int

	// RateLimit is the minimum spacing between the starts of two upstream
	// requests. Default 0 (no spacing beyond Concurrency).
	RateLimit time.Duration

	// PreloadDelay is the pause between two upstream fetches of a preload
	// job. The effective pause is max(PreloadDelay, RateLimit). Default
	// 250ms.
	PreloadDelay time.Duration

	// Placeholder is the PNG served when a tile is not cached and cannot be
	// fetched. Default: a 256x256 dark grey (#11151a) tile.
	Placeholder []byte

	// Logger receives diagnostics. Default: discard.
	Logger *slog.Logger

	// HTTPClient performs upstream requests. It is copied, not modified;
	// UserAgent is enforced on the copy. Default: a client with a 15s
	// timeout.
	HTTPClient *http.Client

	// Clock returns the current time. It is used for tile staleness, the
	// modification time of stored tiles and preload job timestamps.
	// Default: time.Now.
	Clock func() time.Time

	// DisablePreload makes Preload return ErrPreloadDisabled.
	DisablePreload bool

	// CacheControl is the Cache-Control header of a served tile. Default:
	// DefaultCacheControl. Behind a login, use a private value ("private,
	// max-age=86400"): the tiles someone looks at show where they go, and a
	// shared cache must not keep them.
	CacheControl string
}

// DefaultCacheControl is the Cache-Control of a served tile unless
// Options.CacheControl says otherwise.
const DefaultCacheControl = "public, max-age=86400"

// Cache is an on-request map tile cache. Create it with New, serve tiles
// with Handler and release it with Close. A Cache is safe for concurrent
// use.
type Cache struct {
	dir            string
	upstream       string
	hasSubdomain   bool
	subdomains     []string
	userAgent      string
	maxZoom        int
	maxAge         time.Duration
	preloadDelay   time.Duration
	placeholder    []byte
	cacheControl   string
	log            *slog.Logger
	client         *http.Client
	clock          func() time.Time
	disablePreload bool

	sem     chan struct{} // upstream fetch slots
	limit   limiter
	flights flightGroup

	// refreshPausedUntil holds the Clock time, in Unix nanoseconds, before
	// which no background refresh is started.
	refreshPausedUntil atomic.Int64

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc

	mu     sync.Mutex // guards closed, job and the start of tracked goroutines
	closed bool
	job    *Job
	wg     sync.WaitGroup
}

// New validates opts, creates the cache directory and returns a Cache.
func New(opts Options) (*Cache, error) {
	if opts.Dir == "" {
		return nil, errors.New("tilecache: Options.Dir is required")
	}
	if err := checkUserAgent(opts.UserAgent); err != nil {
		return nil, err
	}
	upstream := cmp.Or(opts.Upstream, DefaultUpstream)
	if err := checkUpstream(upstream); err != nil {
		return nil, err
	}
	subdomains := []string{"a", "b", "c"}
	if len(opts.Subdomains) > 0 {
		subdomains = slices.Clone(opts.Subdomains)
	}
	for _, s := range subdomains {
		if s == "" || strings.ContainsAny(s, "/?#%") {
			return nil, fmt.Errorf("tilecache: invalid subdomain %q", s)
		}
	}
	maxZoom := cmp.Or(opts.MaxZoom, defaultMaxZoom)
	if maxZoom < 0 || maxZoom > maxSupportedZoom {
		return nil, fmt.Errorf("tilecache: MaxZoom must be between 0 and %d, got %d", maxSupportedZoom, opts.MaxZoom)
	}
	if opts.Concurrency < 0 {
		return nil, fmt.Errorf("tilecache: Concurrency must not be negative, got %d", opts.Concurrency)
	}
	if opts.RateLimit < 0 {
		return nil, fmt.Errorf("tilecache: RateLimit must not be negative, got %v", opts.RateLimit)
	}
	if opts.PreloadDelay < 0 {
		return nil, fmt.Errorf("tilecache: PreloadDelay must not be negative, got %v", opts.PreloadDelay)
	}

	placeholder := opts.Placeholder
	if placeholder == nil {
		var err error
		if placeholder, err = defaultPlaceholder(); err != nil {
			return nil, fmt.Errorf("tilecache: generating placeholder: %w", err)
		}
	} else {
		if !isPNG(placeholder) {
			return nil, errors.New("tilecache: Placeholder is not a PNG")
		}
		placeholder = slices.Clone(placeholder)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}

	client := http.Client{Timeout: defaultHTTPTimeout}
	if opts.HTTPClient != nil {
		client = *opts.HTTPClient
	}
	client.Transport = &uaTransport{base: client.Transport, userAgent: opts.UserAgent}

	dir := filepath.Clean(opts.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tilecache: creating cache directory: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{
		dir:            dir,
		upstream:       upstream,
		hasSubdomain:   strings.Contains(upstream, "{s}"),
		subdomains:     subdomains,
		userAgent:      opts.UserAgent,
		maxZoom:        maxZoom,
		maxAge:         cmp.Or(opts.MaxAge, defaultMaxAge),
		preloadDelay:   cmp.Or(opts.PreloadDelay, defaultPreloadDelay),
		placeholder:    placeholder,
		cacheControl:   cmp.Or(opts.CacheControl, DefaultCacheControl),
		log:            logger,
		client:         &client,
		clock:          clock,
		disablePreload: opts.DisablePreload,
		sem:            make(chan struct{}, cmp.Or(opts.Concurrency, defaultConcurrency)),
		limit:          limiter{interval: opts.RateLimit},
		ctx:            ctx,
		cancel:         cancel,
	}
	c.flights = flightGroup{ctx: ctx, spawn: c.spawn, m: make(map[tile]*flight)}
	return c, nil
}

func checkUserAgent(ua string) error {
	if strings.TrimSpace(ua) == "" {
		return errors.New("tilecache: Options.UserAgent is required")
	}
	for _, r := range ua {
		if r < 0x20 || r == 0x7f {
			return errors.New("tilecache: Options.UserAgent contains control characters")
		}
	}
	return nil
}

func checkUpstream(tmpl string) error {
	for _, p := range []string{"{z}", "{x}", "{y}"} {
		if !strings.Contains(tmpl, p) {
			return fmt.Errorf("tilecache: Upstream %q lacks %s", tmpl, p)
		}
	}
	sample := strings.NewReplacer("{s}", "a", "{z}", "0", "{x}", "0", "{y}", "0").Replace(tmpl)
	u, err := url.Parse(sample)
	if err != nil {
		return fmt.Errorf("tilecache: Upstream: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tilecache: Upstream %q is not an absolute http(s) URL", tmpl)
	}
	return nil
}

// spawn runs f in a goroutine that Close waits for. It reports false, and
// does not run f, once the cache is closed.
func (c *Cache) spawn(f func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.wg.Go(f)
	return true
}

// Close cancels in-flight upstream fetches, background refreshes and any
// preload job, and waits for their goroutines to exit. Cached tiles are
// still served afterwards; misses get the placeholder. Close is idempotent
// and always returns nil.
func (c *Cache) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
	return nil
}

// Stats walks the cache directory and reports the number of cached tiles and
// their total size in bytes.
func (c *Cache) Stats() (tiles int, bytes int64, err error) {
	err = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Tiles may be replaced while we walk; only a failing root
			// is an error worth reporting.
			if path != c.dir && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		name := d.Name()
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".png") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		tiles++
		bytes += info.Size()
		return nil
	})
	return tiles, bytes, err
}

// tile identifies a tile; it is also the single-flight key, so requests for
// the same tile through different subdomains share one fetch.
type tile struct{ z, x, y int }

func (t tile) String() string { return fmt.Sprintf("%d/%d/%d", t.z, t.x, t.y) }

func (c *Cache) path(t tile) string {
	return filepath.Join(c.dir, strconv.Itoa(t.z), strconv.Itoa(t.x), strconv.Itoa(t.y)+".png")
}

func isPNG(data []byte) bool {
	return len(data) >= len(pngSignature) && string(data[:len(pngSignature)]) == pngSignature
}

func (c *Cache) isStale(mod time.Time) bool {
	return c.maxAge > 0 && c.clock().Sub(mod) > c.maxAge
}
