package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// webFS carries the dashboard's templates and static assets into the binary, so
// a release stays a single file and the assets cannot be missing at runtime.
// Templates are parsed from it at startup; web/static is served over HTTP.
//
//go:embed web/templates web/static
var webFS embed.FS

// staticSub is the web/static subtree rooted at its own directory, so an HTTP
// path of "app.js" maps to the file "web/static/app.js".
func staticSub() fs.FS {
	sub, err := fs.Sub(webFS, "web/static")
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
