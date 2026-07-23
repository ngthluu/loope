package main

import (
	"embed"
	"io/fs"
	"net/http"
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

// staticHandler serves the embedded static assets under /static/. An unknown
// path 404s, and Content-Type comes from the file extension.
func staticHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServerFS(staticSub()))
}
