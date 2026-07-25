// Package web embeds the built dashboard (web/dist) so the daemon serves the UI from
// the same origin as the API — no separate static server, no CDN. The React/Vite build
// lands in web/dist (see wiring-p15.md §1); this file makes that directory part of the
// Go binary.
//
// BUILD REQUIREMENT: web/dist must exist at `go build` time or //go:embed fails the
// build. Run `npm run build` in web/ before building the daemon (scripts/dev.ps1's
// build/check/run steps do this via the build-web step). The committed dist keeps a
// plain `go build ./...` working for dev.
package web

import (
	"embed"
	"io/fs"
)

// distFS embeds the entire built dashboard tree, including index.html and hashed assets
// under dist/assets/. The `all:` prefix ensures dotfiles are embedded too (there are
// none today, but it makes the contract explicit and future-proof).
//
//go:embed all:dist
var distFS embed.FS

// Dist returns the built dashboard rooted at dist/ so "/" maps to dist/index.html and
// "/assets/x" maps to dist/assets/x. It errors only if the embed tree is malformed,
// which would be a build-time impossibility once web/dist exists.
func Dist() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
