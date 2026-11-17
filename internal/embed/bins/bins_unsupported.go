//go:build !(linux && (amd64 || arm64))

package bins

import "io/fs"

var platformAssets fs.FS
