package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	webui "github.com/localhub/localhub/web"
)

// staticHandler serves the embedded dashboard (web/dist) with an SPA fallback: a request
// for an existing file is served as-is; an unknown path with no file extension is treated
// as a client-side route and served index.html (so future client routes work); an unknown
// path WITH an extension (a missing asset) is a real 404.
//
// It is mounted LAST, on the bare "/" pattern, so Go 1.22 ServeMux precedence lets the
// method+path API routes ("GET /api/...", "GET /api/ws") win. This handler therefore only
// ever sees non-API paths. As defense in depth it still refuses to serve anything under
// /api/ or /ws with the JSON 404 envelope, so a future un-registered API path never leaks
// the dashboard HTML instead of a machine-readable error.
//
// Cache headers: hashed assets under /assets/ are immutable (content hash in the name);
// index.html (and any other non-asset path that falls back to it) must not be hard-cached
// or a phone keeps loading stale asset names after a redeploy (wiring-p15.md §"Cache
// headers").
func newStaticHandler() (http.Handler, error) {
	sub, err := webui.Dist()
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path

		// Defense in depth: API/WS namespaces are never the static handler's to serve.
		// If a request reaches here under those prefixes it means no API route matched,
		// so return the JSON 404 envelope, not the SPA shell.
		if strings.HasPrefix(reqPath, "/api/") || reqPath == "/api" ||
			strings.HasPrefix(reqPath, "/ws/") || reqPath == "/ws" {
			writeAPINotFound(w, r)
			return
		}

		p := strings.TrimPrefix(reqPath, "/")
		if p == "" {
			p = "index.html"
		}

		if _, statErr := fs.Stat(sub, p); statErr != nil {
			// No such file. A path without an extension is a client-side route → serve
			// the SPA shell so the app boots and routes internally.
			if path.Ext(p) == "" {
				serveIndex(w, r, fileServer)
				return
			}
			// A missing asset (has an extension) is a genuine 404.
			http.NotFound(w, r)
			return
		}

		setCacheHeaders(w, reqPath)
		fileServer.ServeHTTP(w, r)
	}), nil
}

// serveIndex serves dist/index.html for an SPA route. It rewrites the path to "/" (a clone,
// so the original request is untouched) and delegates to the file server, which sets the
// correct text/html content type. index.html must not be hard-cached.
func serveIndex(w http.ResponseWriter, r *http.Request, fileServer http.Handler) {
	setCacheHeaders(w, "/index.html")
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/"
	fileServer.ServeHTTP(w, r2)
}

// setCacheHeaders applies immutable caching to hashed assets and no-cache to everything
// else (index.html and SPA fallbacks). Vite emits content-hashed filenames under /assets/,
// so those are safe to cache forever; index.html references those names and must be
// re-fetched to pick up a new build.
func setCacheHeaders(w http.ResponseWriter, reqPath string) {
	if strings.HasPrefix(reqPath, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
