//go:build linux && arm64

package bins

import (
	"embed"
	"io/fs"
)

//go:embed all:assets/linux-arm64
var embeddedPlatform embed.FS

var platformAssets fs.FS = mustSub(embeddedPlatform, "assets/linux-arm64")
