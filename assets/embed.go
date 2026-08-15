// Package assets embeds the small static files that ship inside LocalHub binaries.
//
// The icon lives here rather than next to cmd/localhub-tray because go:embed patterns cannot
// escape their own package directory (`..` is rejected), and the same file is also referenced
// from outside Go — scripts/install-start-menu.ps1 points the Start Menu shortcut's IconLocation
// at assets/localhub.ico so the shortcut and the tray show the same mark.
package assets

import _ "embed"

// Icon is a multi-resolution Windows .ico (16..128 as 32-bit DIBs, 256 as PNG).
//
// The DIB entries matter: fyne.io/systray loads the tray icon with LoadImageW(..., LR_LOADFROMFILE|
// LR_DEFAULTSIZE), which is only reliable for uncompressed DIB entries. The shell (Vista+) reads
// the PNG entry for large Explorer/Start Menu renderings.
//
//go:embed localhub.ico
var Icon []byte
