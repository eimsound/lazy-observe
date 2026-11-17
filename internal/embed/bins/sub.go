package bins

import "io/fs"

func mustSub(root fs.FS, dir string) fs.FS {
	result, err := fs.Sub(root, dir)
	if err != nil {
		panic(err)
	}
	return result
}
