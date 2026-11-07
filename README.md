# tilecache

An on-request map tile cache for Go, using only the standard library.

`tilecache` is an `http.Handler` that serves raster tiles at
`/{s}/{z}/{x}/{y}.png`. A tile already on disk is served from there. Otherwise
it is fetched from an upstream tile server, checked to be a PNG, written to
disk atomically and then served. A map client points at your server instead of
the tile server, and every tile it has seen keeps working offline.

- Tiles are stored as `Dir/{z}/{x}/{y}.png`. Each write goes to a temporary
  file, is fsynced and is then renamed into place.
- Concurrent requests for the same tile share one upstream fetch. Upstream
  traffic is limited by `Concurrency` and `RateLimit`.
- Tiles older than `MaxAge` are still served, and refreshed in the background
  (one refresh per tile at a time).
- When a tile is neither cached nor fetchable, a placeholder tile is served
  with `Cache-Control: no-cache`, so the client tries again later.
- Responses carry `X-Tile-Cache: HIT | MISS | PLACEHOLDER`, a strong ETag
  derived from the content (`If-None-Match` gets a 304), and
  `Cache-Control: public, max-age=86400`.
- Every upstream request sends the `UserAgent` you configure.

## Usage

```go
cache, err := tilecache.New(tilecache.Options{
	Dir:            "/var/cache/tiles",
	UserAgent:      tilecache.DefaultUserAgent,
	DisablePreload: true,
})
if err != nil {
	return err
}
defer cache.Close()

mux := http.NewServeMux()
mux.Handle("/tiles/", http.StripPrefix("/tiles", cache.Handler()))
```

Then point the map client at the cache, for example in Leaflet:

```js
L.tileLayer('/tiles/{s}/{z}/{x}/{y}.png', { subdomains: 'abc', maxZoom: 19 })
```

## Options

| Field            | Default                                              | Meaning |
|------------------|------------------------------------------------------|---------|
| `Dir`            | required                                             | Cache root; created if missing. |
| `UserAgent`      | required                                             | Sent on every upstream request, replacing the client's default. `DefaultUserAgent` is a regular desktop Chrome string. |
| `Upstream`       | `https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png` | URL template with `{z}`, `{x}`, `{y}` and optionally `{s}`. |
| `Subdomains`     | `a`, `b`, `c`                                        | Accepted values of the `{s}` path segment. If `Upstream` has no `{s}`, any value is accepted. |
| `MaxZoom`        | 19                                                   | Requests above this zoom get 404 (at most 30). |
| `MaxAge`         | 30 days                                              | Age at which a served tile is refreshed in the background. A negative value turns refreshing off. |
| `Concurrency`    | 2                                                    | Maximum parallel upstream fetches. |
| `RateLimit`      | 0                                                    | Minimum spacing between upstream request starts. |
| `PreloadDelay`   | 250ms                                                | Pause between preload fetches. The pause actually used is the larger of `PreloadDelay` and `RateLimit`. |
| `Placeholder`    | 256×256 `#11151a` PNG                                | Served when upstream fails. |
| `Logger`         | discard                                              | `*slog.Logger` for diagnostics. |
| `HTTPClient`     | 15s timeout                                          | The client is copied rather than modified, and `UserAgent` is enforced on the copy. |
| `Clock`          | `time.Now`                                           | Used for staleness, stored-tile mtimes and preload timestamps. |
| `DisablePreload` | false                                                | When set, `Preload` returns `ErrPreloadDisabled`. |

Malformed or out-of-range coordinates get 400 and unknown paths get 404. Only
GET and HEAD are allowed. `Stats` reports the number of cached tiles and their
total size. `Close` stops background work and waits for it to finish.

## Preloading

`Preload` fetches every tile around a point in a background `Job`, one tile at
a time and pausing between fetches. The radius can be at most 50 km, zoom
levels must be between 0 and 16, and a job covers at most 10000 tiles. Only one
job runs at a time.

```go
job, err := cache.Preload(ctx, tilecache.Region{Lat: 47.37, Lng: 8.54, RadiusKm: 5, MinZoom: 10, MaxZoom: 15})
<-job.Done()
fmt.Printf("%+v\n", job.Status())
```

`PreloadHandlers` returns ready-made JSON endpoints for starting a job,
checking its status and cancelling it.

## OpenStreetMap tile usage policy

The default upstream is `tile.openstreetmap.org`. Its
[tile usage policy](https://operations.osmfoundation.org/policies/tiles/)
allows tiles to be fetched on demand for display, expects them to be cached for
a long time, and **forbids bulk downloading**. With the OSM servers, use this
package as an on-demand cache only. Keep the long `MaxAge`, leave the default
`Concurrency`, and do not preload: set `DisablePreload`, or point `Upstream` at
a tile server that allows bulk downloads before calling `Preload`.

## License

ISC, see [LICENSE](LICENSE).
