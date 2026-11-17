//go:build linux && amd64

package bins

import (
	"embed"
	"io/fs"
)

//go:embed all:assets/linux-amd64
var embeddedPlatform embed.FS

var platformAssets fs.FS = mustSub(embeddedPlatform, "assets/linux-amd64")
