//go:build !(linux && (amd64 || arm64))

package bins

func releaseEmbeddedReadOnlyPages() error {
	return nil
}
