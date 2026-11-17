package symbolicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func TestSymbolicateRealTerserMapAndCache(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "app.min.js.map"))
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	if err := store.Put(ctx, "sourcemaps/default/revisions/"+digest+".map", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]string{
		"kind": "content-addressed-v1", "release": "r1", "file:app.min.js": digest,
	})
	engine := New(store, 128)
	line, column := 1, 37
	event := errortrack.Event{
		Platform: "javascript",
		Release:  "r1",
		Exception: errortrack.Exception{
			Stacktrace: errortrack.Stacktrace{
				Frames: []errortrack.Frame{
					{Function: "Error", Filename: "https://cdn.example/app.min.js?x=1", Lineno: &line, Colno: &column},
				},
			},
		},
	}

	got := engine.Symbolicate(ctx, "default", snapshot, event)
	frame := got.Exception.Stacktrace.Frames[0]
	if frame.Filename != "lazy-observe-original.js" || frame.Function != "Error" || frame.Lineno == nil ||
		*frame.Lineno != 3 ||
		frame.Colno == nil ||
		*frame.Colno != 12 ||
		!frame.Symbolicated {
		t.Fatalf("symbolicated frame = %#v", frame)
	}
	_ = engine.Symbolicate(ctx, "default", snapshot, event)
	if size, hits := engine.CacheStats(); size != 1 || hits != 1 {
		t.Fatalf("cache size=%d hits=%d", size, hits)
	}
}

func TestMissingMapFallsBackUnchanged(t *testing.T) {
	store, _ := blob.NewLocal(t.TempDir())
	engine := New(store, 128)
	line, column := 1, 2
	event := errortrack.Event{
		Platform: "javascript",
		Release:  "missing",
		Exception: errortrack.Exception{
			Stacktrace: errortrack.Stacktrace{
				Frames: []errortrack.Frame{{Function: "a", Filename: "app.js", Lineno: &line, Colno: &column}},
			},
		},
	}
	got := engine.Symbolicate(context.Background(), "default", json.RawMessage(`{"kind":"none"}`), event)
	if got.Exception.Stacktrace.Frames[0].Symbolicated || got.Exception.Stacktrace.Frames[0].Filename != "app.js" {
		t.Fatalf("fallback changed event: %#v", got)
	}
}
