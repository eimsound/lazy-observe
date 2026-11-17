// Package webui exposes the Vite production build embedded in lazy-observe.
package webui

import (
	"embed"
	"io/fs"
)

// dist is populated by scripts/prepare-web-assets.sh before production builds.
// The checked-in .gitkeep lets normal Go tooling work before the frontend has
// been built; FS reports unavailable until dist/index.html exists.
//
//go:embed all:dist
var dist embed.FS

// FS returns the root of the embedded Vite build.
func FS() (fs.FS, error) {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(root, "index.html"); err != nil {
		return nil, err
	}
	return root, nil
}
