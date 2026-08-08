package web

import (
	"embed"
	"io/fs"
)

// dist holds the console, the landing page and the tutorial. Every file in it is
// checked-in source, not a build product: there is no bundler, no Node toolchain and
// no build tag, so `go build` alone produces the whole shipped program. That is the
// same bargain the rest of the binary makes with CGO_ENABLED=0.
//
//go:embed all:dist
var bundled embed.FS

func assetFS() fs.FS {
	root, err := fs.Sub(bundled, "dist")
	if err != nil {
		// The path is a compile-time constant against a compile-time filesystem, so
		// this cannot fail in a binary that built.
		panic("web: embedded dist missing: " + err.Error())
	}
	return root
}
