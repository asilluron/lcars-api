// Package adminui embeds the admin page: the React app in admin/, built into
// dist/ by `make admin-ui` (the Docker image builds it too).
//
// The build output is not committed. A binary built without it still works;
// it serves a page saying how to build the admin page instead.
package adminui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built admin page, rooted at its index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is always embedded
	}
	return sub
}

// Built reports whether the admin page was built into this binary.
func Built() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}
