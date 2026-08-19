package main

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"syscall"
)

// staticFS carries the dashboard's static assets into the binary, so a
// release stays a single file and the assets cannot be missing at runtime.
// They are a copy of the worker dashboard's bundle (worker/web/static):
// after the monorepo split the two binaries no longer share an embed.
//
//go:embed static
var staticFS embed.FS

// staticSub is the static subtree rooted at its own directory, so an HTTP
// path of "app.js" maps to the file "static/app.js".
func staticSub() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Unreachable: the path is a compile-time constant that go:embed verified.
		panic(err)
	}
	return sub
}

// staticHandler serves the embedded static assets under /static/. Only regular
// files resolve: an unknown name and the asset directory itself both 404, so
// net/http's directory index never becomes an endpoint the dashboard didn't ask
// for. Content-Type comes from the file extension.
func staticHandler() http.Handler {
	assets := staticSub()
	files := http.FileServerFS(assets)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := fs.Stat(assets, strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}))
}

// isClientDisconnect reports whether a write failed because the client hung up
// (broken pipe / connection reset) — a mid-response poll cancellation, not a
// server fault, so it is not worth logging.
func isClientDisconnect(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
