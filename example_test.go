// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache_test

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/karamble/tilecache"
)

// Serve OpenStreetMap tiles under /tiles/, for a map client configured with
// the URL template /tiles/{s}/{z}/{x}/{y}.png.
func Example() {
	cache, err := tilecache.New(tilecache.Options{
		Dir:            "/var/cache/tiles",
		UserAgent:      tilecache.DefaultUserAgent,
		DisablePreload: true, // the OSM tile usage policy forbids bulk downloads
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer cache.Close()

	mux := http.NewServeMux()
	mux.Handle("/tiles/", http.StripPrefix("/tiles", cache.Handler()))

	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
