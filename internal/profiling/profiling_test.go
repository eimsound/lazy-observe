package profiling

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"math"
	"runtime"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	pprofprofile "github.com/google/pprof/profile"
)

func gzipProfile(t *testing.T, p *pprofprofile.Profile) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeProfile(
	sampleTypes []*pprofprofile.ValueType,
	values []int64,
	leaf, root string,
) *pprofprofile.Profile {
	rootFn := &pprofprofile.Function{ID: 1, Name: root, SystemName: root, Filename: "main.go"}
	leafFn := &pprofprofile.Function{ID: 2, Name: leaf, SystemName: leaf, Filename: "main.go"}
	rootLoc := &pprofprofile.Location{ID: 1, Address: 0x1000, Line: []pprofprofile.Line{{Function: rootFn, Line: 10}}}
	leafLoc := &pprofprofile.Location{ID: 2, Address: 0x2000, Line: []pprofprofile.Line{{Function: leafFn, Line: 20}}}
	return &pprofprofile.Profile{
		SampleType: sampleTypes,
		Sample: []*pprofprofile.Sample{
			{Location: []*pprofprofile.Location{leafLoc, rootLoc}, Value: values},
		},
		Location: []*pprofprofile.Location{rootLoc, leafLoc},
		Function: []*pprofprofile.Function{rootFn, leafFn},
	}
}

func findNode(node *FlameNode, name string) *FlameNode {
	if node == nil {
		return nil
	}
	if node.Name == name {
		return node
	}
	for _, child := range node.Children {
		if found := findNode(child, name); found != nil {
			return found
		}
	}
	return nil
}

func TestParseGzippedValidationAndBounds(t *testing.T) {
	// Non-gzip payload rejected.
	if _, err := ParseGzipped([]byte("plain text"), 1024); !errors.Is(err, ErrNotGzip) {
		t.Fatalf("plain payload err=%v want ErrNotGzip", err)
	}

	// Oversized decoded stream rejected with ErrDecodedTooLarge.
	var huge bytes.Buffer
	zw := gzip.NewWriter(&huge)
	_, _ = zw.Write(bytes.Repeat([]byte("A"), 4096))
	_ = zw.Close()
	if _, err := ParseGzipped(huge.Bytes(), 512); !errors.Is(err, ErrDecodedTooLarge) {
		t.Fatalf("oversized decoded err=%v want ErrDecodedTooLarge", err)
	}

	// Nested gzip rejected with ErrNestedGzip.
	validCPU := gzipProfile(t, makeProfile(
		[]*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		[]int64{1, 100},
		"main.work",
		"main.main",
	))
	var nested bytes.Buffer
	zw2 := gzip.NewWriter(&nested)
	_, _ = zw2.Write(validCPU)
	_ = zw2.Close()
	if _, err := ParseGzipped(nested.Bytes(), DefaultMaxDecodedBytes); !errors.Is(err, ErrNestedGzip) {
		t.Fatalf("nested gzip err=%v want ErrNestedGzip", err)
	}

	// Empty SampleType rejected with ErrInvalidSampleType.
	emptyST := gzipProfile(t, &pprofprofile.Profile{})
	if _, err := ParseGzipped(emptyST, DefaultMaxDecodedBytes); !errors.Is(err, ErrInvalidSampleType) {
		t.Fatalf("empty SampleType err=%v want ErrInvalidSampleType", err)
	}

	// Mismatched SampleType for profile kind rejected with ErrInvalidSampleType.
	heapBytes := gzipProfile(t, makeProfile(
		[]*pprofprofile.ValueType{{Type: "inuse_space", Unit: "bytes"}},
		[]int64{4096},
		"main.alloc",
		"main.main",
	))
	if _, err := ParseGzippedForType(heapBytes, DefaultMaxDecodedBytes, meta.ProfileTypeCPU); !errors.Is(
		err,
		ErrInvalidSampleType,
	) {
		t.Fatalf("heap uploaded as cpu err=%v want ErrInvalidSampleType", err)
	}
	if _, err := ParseGzippedForType(heapBytes, DefaultMaxDecodedBytes, meta.ProfileTypeHeap); err != nil {
		t.Fatalf("valid heap err=%v", err)
	}

	// Valid CPU profile accepted for CPU.
	if _, err := ParseGzippedForType(validCPU, DefaultMaxDecodedBytes, meta.ProfileTypeCPU); err != nil {
		t.Fatalf("valid cpu err=%v", err)
	}
}

func TestFlamegraphBuilderHeterogeneousGoNodePythonAndUnits(t *testing.T) {
	// Simulate Go CPU ([samples/count, cpu/nanoseconds]),
	// Python CPU ([samples/count, cpu/nanoseconds]),
	// Node/custom CPU ([cpu/microseconds]), and single-column ([cpu/nanoseconds]).
	goCPU := makeProfile(
		[]*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		[]int64{10, 100_000},
		"go.handler",
		"main.main",
	)
	pyCPU := makeProfile(
		[]*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		[]int64{5, 50_000},
		"py.parse",
		"main.main",
	)
	microCPU := makeProfile(
		[]*pprofprofile.ValueType{{Type: "cpu", Unit: "microseconds"}},
		[]int64{25}, // 25 us == 25_000 ns
		"node.render",
		"main.main",
	)

	builder := NewFlamegraphBuilder(meta.ProfileTypeCPU, DefaultMaxFlameDepth, DefaultMaxFlameNodes)
	for _, raw := range [][]byte{gzipProfile(t, goCPU), gzipProfile(t, pyCPU), gzipProfile(t, microCPU)} {
		if err := builder.AddGzipped(raw, DefaultMaxDecodedBytes); err != nil {
			t.Fatalf("AddGzipped: %v", err)
		}
	}
	if builder.Count() != 3 {
		t.Fatalf("Count=%d want=3", builder.Count())
	}
	tree := builder.Finish()
	if tree.Unit != "nanoseconds" {
		t.Fatalf("unit=%q want=nanoseconds", tree.Unit)
	}
	if tree.Value != 175_000 {
		t.Fatalf("root value=%d want=175000", tree.Value)
	}
	if n := findNode(tree, "node.render"); n == nil || n.Value != 25_000 {
		t.Fatalf("node.render=%+v want=25000", n)
	}
}

func TestFlamegraphBuilderHeapAndGoroutineSnapshotAveraging(t *testing.T) {
	// Go heap has 4 sample types; Node heap has 2 sample types. Both pick inuse_space and average.
	goHeap := makeProfile(
		[]*pprofprofile.ValueType{
			{Type: "alloc_objects", Unit: "count"},
			{Type: "alloc_space", Unit: "bytes"},
			{Type: "inuse_objects", Unit: "count"},
			{Type: "inuse_space", Unit: "bytes"},
		},
		[]int64{10, 1000, 2, 100},
		"main.alloc",
		"main.main",
	)
	nodeHeap := makeProfile(
		[]*pprofprofile.ValueType{
			{Type: "inuse_objects", Unit: "count"},
			{Type: "inuse_space", Unit: "bytes"},
		},
		[]int64{4, 300},
		"main.alloc",
		"main.main",
	)

	heapBuilder := NewFlamegraphBuilder(meta.ProfileTypeHeap, DefaultMaxFlameDepth, DefaultMaxFlameNodes)
	heapBuilder.AddProfile(goHeap)
	heapBuilder.AddProfile(nodeHeap)
	heapTree := heapBuilder.Finish()
	if heapTree.Unit != "bytes" || heapTree.Value != 200 {
		t.Fatalf("heap snapshot tree=%+v want value=200 unit=bytes", heapTree)
	}

	// Goroutine profiles average snapshots as well.
	g1 := makeProfile([]*pprofprofile.ValueType{{Type: "goroutine", Unit: "count"}}, []int64{10}, "worker", "root")
	g2 := makeProfile([]*pprofprofile.ValueType{{Type: "samples", Unit: "count"}}, []int64{20}, "worker", "root")
	gBuilder := NewFlamegraphBuilder(meta.ProfileTypeGoroutine, DefaultMaxFlameDepth, DefaultMaxFlameNodes)
	gBuilder.AddProfile(g1)
	gBuilder.AddProfile(g2)
	gTree := gBuilder.Finish()
	if gTree.Unit != "count" || gTree.Value != 15 {
		t.Fatalf("goroutine snapshot tree=%+v want value=15 unit=count", gTree)
	}
}

func TestFlamegraphBuilderRecursionTruncationAndOverflow(t *testing.T) {
	fnA := &pprofprofile.Function{ID: 1, Name: "rec"}
	fnB := &pprofprofile.Function{ID: 2, Name: "base"}
	locA := &pprofprofile.Location{ID: 1, Line: []pprofprofile.Line{{Function: fnA}}}
	locB := &pprofprofile.Location{ID: 2, Line: []pprofprofile.Line{{Function: fnB}}}
	p := &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*pprofprofile.Sample{
			{Location: []*pprofprofile.Location{locA, locA, locA, locB}, Value: []int64{math.MaxInt64 - 1}},
			{Location: []*pprofprofile.Location{locA, locB}, Value: []int64{50}},
		},
		Location: []*pprofprofile.Location{locA, locB},
		Function: []*pprofprofile.Function{fnA, fnB},
	}
	tree := BuildFlamegraph(p, meta.ProfileTypeCPU, DefaultMaxFlameDepth, DefaultMaxFlameNodes)
	if tree.Value != math.MaxInt64 {
		t.Fatalf("clamped value=%d want MaxInt64", tree.Value)
	}
	base := findNode(tree, "base")
	if base == nil || len(base.Children) != 1 || base.Children[0].Name != "rec" || len(base.Children[0].Children) != 0 {
		t.Fatalf("direct recursion not collapsed: %+v", base)
	}
}

// TestFlamegraphBuilderStreamingMemoryBounded verifies that folding many large
// profiles sequentially via AddGzippedReader does not retain parsed *Profile
// objects across iterations.
func TestFlamegraphBuilderStreamingMemoryBounded(t *testing.T) {
	// Build a profile template with 500 unique locations and 500 samples.
	buildLarge := func(iter int) []byte {
		p := &pprofprofile.Profile{
			SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		}
		sharedRootFn := &pprofprofile.Function{ID: 1, Name: "main.serve"}
		sharedRootLoc := &pprofprofile.Location{ID: 1, Line: []pprofprofile.Line{{Function: sharedRootFn}}}
		p.Function = append(p.Function, sharedRootFn)
		p.Location = append(p.Location, sharedRootLoc)
		for i := 2; i <= 400; i++ {
			fn := &pprofprofile.Function{
				ID:       uint64(i),
				Name:     fmt.Sprintf("pkg.worker_%d", i%32),
				Filename: fmt.Sprintf("/src/service/iter_%d/file_%d.go", iter, i),
			}
			loc := &pprofprofile.Location{
				ID:      uint64(i),
				Address: uint64(i * 16),
				Line:    []pprofprofile.Line{{Function: fn}},
			}
			p.Function = append(p.Function, fn)
			p.Location = append(p.Location, loc)
			p.Sample = append(p.Sample, &pprofprofile.Sample{
				Location: []*pprofprofile.Location{loc, sharedRootLoc},
				Value:    []int64{10},
			})
		}
		return gzipProfile(t, p)
	}

	const iterations = 64
	payloads := make([][]byte, iterations)
	for i := range iterations {
		payloads[i] = buildLarge(i)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	builder := NewFlamegraphBuilder(meta.ProfileTypeCPU, DefaultMaxFlameDepth, DefaultMaxFlameNodes)
	for _, raw := range payloads {
		if err := builder.AddGzippedReader(
			bytes.NewReader(raw),
			DefaultMaxProfileBytes,
			DefaultMaxDecodedBytes,
		); err != nil {
			t.Fatalf("AddGzippedReader: %v", err)
		}
	}
	tree := builder.Finish()
	if builder.Count() != iterations {
		t.Fatalf("Count=%d want=%d", builder.Count(), iterations)
	}
	wantTotal := int64(iterations * 399 * 10)
	if tree.Value != wantTotal {
		t.Fatalf("total=%d want=%d", tree.Value, wantTotal)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Retained heap growth (tree with ~34 nodes) must stay tiny compared to
	// retaining all 64 parsed pprof.Profile graphs (which would be tens of MB).
	var retainedDelta uint64
	if after.HeapAlloc > before.HeapAlloc {
		retainedDelta = after.HeapAlloc - before.HeapAlloc
	}
	if retainedDelta > 4<<20 {
		t.Fatalf("retained heap grew by %d bytes after streaming %d profiles; builder may retain Profile objects",
			retainedDelta, iterations)
	}
}
