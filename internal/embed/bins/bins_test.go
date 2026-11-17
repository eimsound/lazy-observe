package bins

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestExtractVerifiesAndRepairsPayload(t *testing.T) {
	payload := []byte("test storage executable")
	digest := sha256.Sum256(payload)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{}
	var manifest bytes.Buffer
	for _, name := range requiredBinaries {
		fmt.Fprintf(&manifest, "%s\t%d\t%x\n", name, len(payload), digest)
		assets[name+".gz"] = &fstest.MapFile{Data: compressed.Bytes()}
	}
	assets[manifestName] = &fstest.MapFile{Data: manifest.Bytes()}
	dir := t.TempDir()
	path := filepath.Join(dir, "vmagent-prod")
	if err := os.WriteFile(path, []byte("wrong"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotDir, err := extract(assets, dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotDir, _ = filepath.Abs(gotDir); gotDir == "" {
		t.Fatal("extract returned an empty directory")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("extracted mode %v is not executable", info.Mode())
	}
	if _, err := extract(assets, dir); err != nil {
		t.Fatalf("reuse verified payload: %v", err)
	}
}

func TestExtractRejectsBadDigest(t *testing.T) {
	assets := fstest.MapFS{
		manifestName:      &fstest.MapFile{Data: []byte("vmagent-prod 1 " + string(make([]byte, 64)) + "\n")},
		"vmagent-prod.gz": &fstest.MapFile{},
	}
	if _, err := extract(assets, t.TempDir()); err == nil {
		t.Fatal("extract accepted an invalid manifest digest")
	}
}

var _ fs.FS = fstest.MapFS{}
