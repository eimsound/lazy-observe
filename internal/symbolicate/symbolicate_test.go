package symbolicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	got, err := engine.Symbolicate(ctx, "default", snapshot, event)
	if err != nil {
		t.Fatalf("Symbolicate error: %v", err)
	}
	frame := got.Exception.Stacktrace.Frames[0]
	if frame.Filename != "lazy-observe-original.js" || frame.Function != "Error" || frame.Lineno == nil ||
		*frame.Lineno != 3 ||
		frame.Colno == nil ||
		*frame.Colno != 12 ||
		!frame.Symbolicated {
		t.Fatalf("symbolicated frame = %#v", frame)
	}
	if _, err := engine.Symbolicate(ctx, "default", snapshot, event); err != nil {
		t.Fatalf("cached Symbolicate error: %v", err)
	}
	if size, hits := engine.CacheStats(); size != 1 || hits != 1 {
		t.Fatalf("cache size=%d hits=%d", size, hits)
	}
	if engine.CacheBytes() <= 0 {
		t.Fatalf("expected positive cache bytes, got %d", engine.CacheBytes())
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
	got, err := engine.Symbolicate(context.Background(), "default", json.RawMessage(`{"kind":"none"}`), event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Exception.Stacktrace.Frames[0].Symbolicated || got.Exception.Stacktrace.Frames[0].Filename != "app.js" {
		t.Fatalf("fallback changed event: %#v", got)
	}
}

func TestValidateAndNormalizeRejectsMaliciousMapsAndStripsSourcesContent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nil section map panic trigger", `{"version":3,"sections":[{}]}`},
		{"out of bounds sourcesInd panic trigger", `{"version":3,"sources":["a.js"],"names":[],"mappings":"ACAA"}`},
		{"out of bounds namesInd", `{"version":3,"sources":["a.js"],"names":[],"mappings":"AAAAA"}`},
		{"wrong version zero", `{"version":0,"sources":["a.js"],"mappings":"AAAA"}`},
		{"wrong version two", `{"version":2,"sources":["a.js"],"mappings":"AAAA"}`},
		{"invalid VLQ byte", `{"version":3,"sources":["a.js"],"mappings":"!!!!"}`},
		{"trailing garbage", `{"version":3,"sources":[],"mappings":""} {}`},
		{
			"nested sections",
			`{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sections":[]}}]}`,
		},
		{
			"vlq separator bomb",
			`{"version":3,"sources":["a.js"],"mappings":"` + strings.Repeat(",", MaxMappingSegments+1) + `"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ValidateAndNormalize([]byte(tc.body)); err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}

	// Verify sourcesContent is stripped from valid maps without breaking lookup.
	raw, err := os.ReadFile(filepath.Join("testdata", "app.min.js.map"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("sourcesContent")) {
		t.Fatal("fixture expected to include sourcesContent")
	}
	normalized, segments, err := ValidateAndNormalize(raw)
	if err != nil {
		t.Fatalf("valid map rejected: %v", err)
	}
	if segments == 0 {
		t.Fatal("expected non-zero mapping segments")
	}
	if bytes.Contains(normalized, []byte("sourcesContent")) {
		t.Fatalf("sourcesContent was not stripped: %s", normalized)
	}
}

func TestSymbolicateDoesNotPanicOnPreexistingCorruptBlobs(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	corruptPayloads := []string{
		`{"version":3,"sections":[{}]}`,
		`{"version":3,"sources":["a.js"],"names":[],"mappings":"ACAA"}`,
	}
	engine := New(store, 128)
	line, col := 1, 0
	for i, payload := range corruptPayloads {
		sum := sha256.Sum256([]byte(payload))
		digest := hex.EncodeToString(sum[:])
		if err := store.Put(ctx, RevisionKey("default", digest), strings.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := json.Marshal(map[string]string{
			"kind": "content-addressed-v1", "release": "r1", "file:bad.js": digest,
		})
		event := errortrack.Event{
			Platform: "javascript",
			Release:  "r1",
			Exception: errortrack.Exception{
				Stacktrace: errortrack.Stacktrace{
					Frames: []errortrack.Frame{
						{Function: "x", Filename: "https://cdn.example/bad.js", Lineno: &line, Colno: &col},
					},
				},
			},
		}
		got, err := engine.Symbolicate(ctx, "default", snapshot, event)
		if err != nil {
			t.Fatalf("case %d: corrupt map should degrade instead of retry-looping: %v", i, err)
		}
		if got.Exception.Stacktrace.Frames[0].Symbolicated {
			t.Fatalf("case %d: corrupt map unexpectedly symbolicated frame", i)
		}
	}
}

type transientErrorBlobStore struct {
	err error
}

func (s transientErrorBlobStore) Put(context.Context, string, io.Reader) error { return s.err }
func (s transientErrorBlobStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, s.err
}
func (s transientErrorBlobStore) List(context.Context, string) ([]blob.Object, error) {
	return nil, s.err
}
func (s transientErrorBlobStore) Delete(context.Context, string) error       { return s.err }
func (s transientErrorBlobStore) DeletePrefix(context.Context, string) error { return s.err }

func TestSymbolicatePropagatesTransientBlobErrors(t *testing.T) {
	transientErr := errors.New("s3 503 slow down")
	engine := New(transientErrorBlobStore{err: transientErr}, 128)
	digest := strings.Repeat("a", 64)
	snapshot, _ := json.Marshal(map[string]string{
		"kind": "content-addressed-v1", "release": "r1", "file:app.js": digest,
	})
	line, col := 1, 1
	event := errortrack.Event{
		Platform: "javascript",
		Release:  "r1",
		Exception: errortrack.Exception{
			Stacktrace: errortrack.Stacktrace{
				Frames: []errortrack.Frame{
					{Function: "a", Filename: "https://cdn.example/app.js", Lineno: &line, Colno: &col},
				},
			},
		},
	}
	if _, err := engine.Symbolicate(context.Background(), "default", snapshot, event); !errors.Is(err, transientErr) {
		t.Fatalf("expected transient error to propagate, got %v", err)
	}
}

func TestSymbolicateLRUEvictsByByteBudget(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mapA := []byte(`{"version":3,"sources":["src/first-file.js"],"names":["fnA"],"mappings":"AAAA"}`)
	mapB := []byte(`{"version":3,"sources":["src/second-file.js"],"names":["fnB"],"mappings":"AAAA"}`)
	sumA := sha256.Sum256(mapA)
	sumB := sha256.Sum256(mapB)
	digestA := hex.EncodeToString(sumA[:])
	digestB := hex.EncodeToString(sumB[:])
	if err := store.Put(ctx, RevisionKey("default", digestA), bytes.NewReader(mapA)); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, RevisionKey("default", digestB), bytes.NewReader(mapB)); err != nil {
		t.Fatal(err)
	}
	// Budget allows one parsed map (~120 bytes) but not two.
	engine := NewWithLimits(store, 128, 180)
	line, col := 1, 0
	for _, item := range []struct {
		file, digest string
	}{{"a.js", digestA}, {"b.js", digestB}} {
		snapshot, _ := json.Marshal(map[string]string{
			"kind": "content-addressed-v1", "release": "r1", "file:" + item.file: item.digest,
		})
		event := errortrack.Event{
			Platform: "javascript",
			Release:  "r1",
			Exception: errortrack.Exception{
				Stacktrace: errortrack.Stacktrace{
					Frames: []errortrack.Frame{
						{Function: "x", Filename: "https://cdn.example/" + item.file, Lineno: &line, Colno: &col},
					},
				},
			},
		}
		if _, err := engine.Symbolicate(ctx, "default", snapshot, event); err != nil {
			t.Fatalf("Symbolicate %s: %v", item.file, err)
		}
	}
	if size, _ := engine.CacheStats(); size != 1 {
		t.Fatalf("expected byte budget to evict older entry leaving 1, got size=%d bytes=%d", size, engine.CacheBytes())
	}
	if engine.CacheBytes() > 180 {
		t.Fatalf("cache bytes=%d exceeded budget 180", engine.CacheBytes())
	}
}

func TestSymbolicateMatchesPathSuffixWithoutBasenameCollision(t *testing.T) {
	ctx := context.Background()
	store, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mapAdmin := []byte(`{"version":3,"sources":["src/admin.ts"],"names":["adminBoom"],"mappings":"AAAAA"}`)
	mapShop := []byte(`{"version":3,"sources":["src/shop.ts"],"names":["shopBoom"],"mappings":"AAAAA"}`)
	sumAdmin := sha256.Sum256(mapAdmin)
	sumShop := sha256.Sum256(mapShop)
	digestAdmin := hex.EncodeToString(sumAdmin[:])
	digestShop := hex.EncodeToString(sumShop[:])
	if err := store.Put(ctx, RevisionKey("default", digestAdmin), bytes.NewReader(mapAdmin)); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, RevisionKey("default", digestShop), bytes.NewReader(mapShop)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]string{
		"kind":                "content-addressed-v1",
		"release":             "r1",
		"file:admin/index.js": digestAdmin,
		"file:shop/index.js":  digestShop,
	})
	engine := New(store, 128)
	line, col := 1, 0
	event := errortrack.Event{
		Platform: "javascript",
		Release:  "r1",
		Exception: errortrack.Exception{
			Stacktrace: errortrack.Stacktrace{
				Frames: []errortrack.Frame{
					{
						Function: "a",
						Filename: "https://cdn.example/static/admin/index.js?v=1",
						Lineno:   &line,
						Colno:    &col,
					},
					{
						Function: "b",
						Filename: "https://cdn.example/static/shop/index.js?v=2",
						Lineno:   &line,
						Colno:    &col,
					},
					{Function: "c", Filename: "https://cdn.example/static/other/index.js", Lineno: &line, Colno: &col},
				},
			},
		},
	}
	got, err := engine.Symbolicate(ctx, "default", snapshot, event)
	if err != nil {
		t.Fatal(err)
	}
	frames := got.Exception.Stacktrace.Frames
	if !frames[0].Symbolicated || frames[0].Filename != "src/admin.ts" {
		t.Fatalf("admin frame=%#v", frames[0])
	}
	if !frames[1].Symbolicated || frames[1].Filename != "src/shop.ts" {
		t.Fatalf("shop frame=%#v", frames[1])
	}
	if frames[2].Symbolicated || frames[2].Filename != "https://cdn.example/static/other/index.js" {
		t.Fatalf("unrelated index.js collided with another directory: %#v", frames[2])
	}
}
