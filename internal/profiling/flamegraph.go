package profiling

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	pprofprofile "github.com/google/pprof/profile"
)

// FlameNode is one node in an aggregated flamegraph tree.
type FlameNode struct {
	Name      string       `json:"name"`
	Value     int64        `json:"value"`
	Unit      string       `json:"unit,omitempty"`
	Children  []*FlameNode `json:"children,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	byName    map[string]*FlameNode
}

// FlamegraphBuilder incrementally folds pprof profiles into a bounded flame
// tree in a single pass. Each profile is folded and immediately released so
// peak heap usage stays constant regardless of how many profiles are aggregated.
type FlamegraphBuilder struct {
	profileType  meta.ProfileType
	maxDepth     int
	maxNodes     int
	root         *FlameNode
	nodes        int
	profileCount int
	unit         string
	snapshot     bool
}

// NewFlamegraphBuilder constructs a streaming flamegraph aggregator bounded by
// maxDepth and maxNodes.
func NewFlamegraphBuilder(profileType meta.ProfileType, maxDepth, maxNodes int) *FlamegraphBuilder {
	return &FlamegraphBuilder{
		profileType: profileType,
		maxDepth:    maxDepth,
		maxNodes:    maxNodes,
		root:        &FlameNode{Name: "root", byName: make(map[string]*FlameNode)},
		nodes:       1,
	}
}

// Count returns the number of profiles folded into this builder so far.
func (b *FlamegraphBuilder) Count() int {
	return b.profileCount
}

// AddGzipped parses one gzipped pprof payload, folds its samples into the
// flame tree, and immediately drops the parsed pprof object.
func (b *FlamegraphBuilder) AddGzipped(data []byte, decodedLimit int) error {
	parsed, err := ParseGzipped(data, decodedLimit)
	if err != nil {
		return err
	}
	b.AddProfile(parsed)
	return nil
}

// AddGzippedReader reads a bounded compressed pprof stream from r, parses and
// folds it into the flame tree, and immediately releases both the raw buffer
// and parsed profile.
func (b *FlamegraphBuilder) AddGzippedReader(r io.Reader, compressedLimit, decodedLimit int) error {
	if compressedLimit <= 0 {
		compressedLimit = DefaultMaxProfileBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, int64(compressedLimit)+1))
	if err != nil {
		return err
	}
	if len(raw) > compressedLimit {
		return ErrCompressedTooLarge
	}
	return b.AddGzipped(raw, decodedLimit)
}

// AddProfile folds a single parsed pprof profile into the flame tree without
// retaining any reference to p.
func (b *FlamegraphBuilder) AddProfile(p *pprofprofile.Profile) {
	if p == nil {
		return
	}
	valueIndex, unit, snapshot := profileValueMeta(p, b.profileType)
	if b.profileCount == 0 {
		b.unit = unit
		b.snapshot = snapshot
	} else {
		if b.unit == "" && unit != "" {
			b.unit = unit
		}
		if !snapshot {
			b.snapshot = false
		}
	}
	b.profileCount++
	if b.maxNodes <= 0 {
		if len(p.Sample) > 0 {
			b.root.Truncated = true
		}
		return
	}
	for _, sample := range p.Sample {
		if valueIndex < 0 || valueIndex >= len(sample.Value) {
			continue
		}
		value := convertUnitValue(sample.Value[valueIndex], unit, b.unit)
		if value <= 0 {
			continue
		}
		b.root.Value = addInt64Clamped(b.root.Value, value)
		frames := collapseDirectRecursion(sampleFrames(sample))
		current := b.root
		for depth, name := range frames {
			if depth >= b.maxDepth {
				current.Truncated = true
				break
			}
			child := current.byName[name]
			if child == nil {
				if b.nodes >= b.maxNodes {
					current.Truncated = true
					break
				}
				child = &FlameNode{Name: name, byName: make(map[string]*FlameNode)}
				current.byName[name] = child
				current.Children = append(current.Children, child)
				b.nodes++
			}
			child.Value = addInt64Clamped(child.Value, value)
			current = child
		}
	}
}

// Finish finalizes snapshot averaging, sets the root unit, sorts children
// deterministically, and releases internal lookup maps.
func (b *FlamegraphBuilder) Finish() *FlameNode {
	if b.snapshot && b.profileCount > 1 {
		scaleFlameTreeAverage(b.root, int64(b.profileCount))
	}
	b.root.Unit = b.unit
	sortFlameTree(b.root)
	return b.root
}

// BuildFlamegraph folds a single parsed profile into a finalized FlameNode tree.
func BuildFlamegraph(p *pprofprofile.Profile, profileType meta.ProfileType, maxDepth, maxNodes int) *FlameNode {
	builder := NewFlamegraphBuilder(profileType, maxDepth, maxNodes)
	builder.AddProfile(p)
	return builder.Finish()
}

func addInt64Clamped(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func scaleFlameTreeAverage(node *FlameNode, count int64) {
	if node == nil || count <= 1 {
		return
	}
	if node.Value > 0 {
		q, r := node.Value/count, node.Value%count
		if r*2 >= count && q < math.MaxInt64 {
			q++
		}
		if q == 0 {
			q = 1
		}
		node.Value = q
	}
	for _, child := range node.Children {
		scaleFlameTreeAverage(child, count)
	}
}

func collapseDirectRecursion(frames []string) []string {
	if len(frames) <= 1 {
		return frames
	}
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		if len(out) > 0 && out[len(out)-1] == frame {
			continue
		}
		out = append(out, frame)
	}
	return out
}

func profileValueMeta(p *pprofprofile.Profile, profileType meta.ProfileType) (index int, unit string, snapshot bool) {
	index = profileValueIndex(p, profileType)
	if index >= 0 && index < len(p.SampleType) && p.SampleType[index] != nil {
		st := p.SampleType[index]
		unit = st.Unit
		switch st.Type {
		case "inuse_space", "inuse_objects", "goroutine":
			return index, unit, true
		case "cpu", "wall", "alloc_space", "alloc_objects":
			return index, unit, false
		case "samples":
			return index, unit, profileType == meta.ProfileTypeGoroutine
		}
	}
	return index, unit, profileType == meta.ProfileTypeHeap || profileType == meta.ProfileTypeGoroutine
}

func profileValueIndex(p *pprofprofile.Profile, profileType meta.ProfileType) int {
	var wanted []string
	switch profileType {
	case meta.ProfileTypeCPU:
		wanted = []string{"cpu", "wall", "samples"}
	case meta.ProfileTypeHeap:
		wanted = []string{"inuse_space", "alloc_space", "inuse_objects", "alloc_objects"}
	case meta.ProfileTypeGoroutine:
		wanted = []string{"goroutine", "samples", "count"}
	}
	for _, name := range wanted {
		for i, sampleType := range p.SampleType {
			if sampleType != nil && sampleType.Type == name {
				return i
			}
		}
	}
	if len(p.SampleType) == 0 {
		return 0
	}
	return len(p.SampleType) - 1
}

func convertUnitValue(value int64, fromUnit, toUnit string) int64 {
	if value <= 0 || fromUnit == "" || toUnit == "" || fromUnit == toUnit {
		return value
	}
	fromScale, fromFamily := unitScale(fromUnit)
	toScale, toFamily := unitScale(toUnit)
	if fromFamily == "" || fromFamily != toFamily {
		return value
	}
	if fromScale == toScale {
		return value
	}
	if fromScale > toScale {
		mul := fromScale / toScale
		if value > math.MaxInt64/mul {
			return math.MaxInt64
		}
		return value * mul
	}
	div := toScale / fromScale
	q := value / div
	if q == 0 {
		return 1
	}
	return q
}

func unitScale(unit string) (scale int64, family string) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "nanosecond", "nanoseconds", "ns":
		return 1, "time"
	case "microsecond", "microseconds", "us", "µs":
		return 1_000, "time"
	case "millisecond", "milliseconds", "ms":
		return 1_000_000, "time"
	case "second", "seconds", "s":
		return 1_000_000_000, "time"
	case "byte", "bytes", "b":
		return 1, "bytes"
	case "kilobyte", "kilobytes", "kb":
		return 1024, "bytes"
	case "megabyte", "megabytes", "mb":
		return 1024 * 1024, "bytes"
	default:
		return 0, ""
	}
}

func sampleFrames(sample *pprofprofile.Sample) []string {
	leafFirst := make([]string, 0, len(sample.Location))
	for _, location := range sample.Location {
		if location == nil {
			continue
		}
		if len(location.Line) == 0 {
			leafFirst = append(leafFirst, fmt.Sprintf("0x%x", location.Address))
			continue
		}
		for _, line := range location.Line {
			name := "unknown"
			if line.Function != nil && line.Function.Name != "" {
				name = line.Function.Name
			}
			leafFirst = append(leafFirst, name)
		}
	}
	frames := make([]string, len(leafFirst))
	for i := range leafFirst {
		frames[len(leafFirst)-1-i] = leafFirst[i]
	}
	return frames
}

func sortFlameTree(node *FlameNode) {
	sort.Slice(node.Children, func(i, j int) bool {
		if node.Children[i].Value == node.Children[j].Value {
			return node.Children[i].Name < node.Children[j].Name
		}
		return node.Children[i].Value > node.Children[j].Value
	})
	for _, child := range node.Children {
		sortFlameTree(child)
	}
	node.byName = nil
}
