package matcher

import (
	"container/list"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

const (
	// OpEqual matches a label value by exact equality.
	OpEqual = "="
	// OpRegex matches a label value against a fully anchored regular expression.
	OpRegex = "=~"

	// MaxRegexLength bounds regex pattern length to reject pathological inputs
	// before compilation or caching.
	MaxRegexLength = 1024
	// MaxMatchers bounds the number of matchers in a single silence or
	// inhibition matcher set.
	MaxMatchers = 64
	// DefaultRegexCacheCapacity bounds the number of compiled regex patterns
	// retained in the process-wide cache.
	DefaultRegexCacheCapacity = 512
)

// Spec is the wire/persistence representation of a label matcher.
type Spec struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Op    string `json:"op"`
}

// Compiled is an immutable, precompiled label matcher safe for concurrent use.
type Compiled struct {
	name  string
	op    string
	value string
	re    *regexp.Regexp
}

// Name returns the matcher label name.
func (c Compiled) Name() string { return c.name }

// Op returns the canonical matcher operator ("=" or "=~").
func (c Compiled) Op() string { return c.op }

// Value returns the raw matcher value or regex pattern.
func (c Compiled) Value() string { return c.value }

// Matches reports whether labels satisfies this matcher.
func (c Compiled) Matches(labels map[string]string) bool {
	value, ok := labels[c.name]
	if !ok {
		return false
	}
	switch c.op {
	case OpEqual:
		return value == c.value
	case OpRegex:
		return c.re != nil && c.re.MatchString(value)
	default:
		return false
	}
}

// Set is a non-empty precompiled conjunction of label matchers.
type Set struct {
	matchers []Compiled
}

// Len returns the number of compiled matchers in the set.
func (s Set) Len() int { return len(s.matchers) }

// Matches reports whether labels satisfies every matcher in the set.
// An empty set never matches.
func (s Set) Matches(labels map[string]string) bool {
	if len(s.matchers) == 0 {
		return false
	}
	for _, m := range s.matchers {
		if !m.Matches(labels) {
			return false
		}
	}
	return true
}

// AlertRef is the minimal alert identity needed for inhibition evaluation.
type AlertRef struct {
	Fingerprint string
	Status      string
	Labels      map[string]string
}

// InhibitionRule is a precompiled Alertmanager-style inhibition rule.
type InhibitionRule struct {
	Source      Set
	Target      Set
	EqualLabels []string
}

// Inhibits reports whether any active firing alert in sources inhibits target.
func (r InhibitionRule) Inhibits(target AlertRef, sources []AlertRef) bool {
	if !r.Target.Matches(target.Labels) {
		return false
	}
	for _, source := range sources {
		if source.Status == "resolved" || source.Fingerprint == target.Fingerprint {
			continue
		}
		if !r.Source.Matches(source.Labels) {
			continue
		}
		if labelsEqual(source.Labels, target.Labels, r.EqualLabels) {
			return true
		}
	}
	return false
}

func labelsEqual(a, b map[string]string, names []string) bool {
	for _, name := range names {
		av, aok := a[name]
		bv, bok := b[name]
		if !aok || !bok || av != bv {
			return false
		}
	}
	return true
}

// CacheStats captures bounded regex cache metrics.
type CacheStats struct {
	Entries  int
	Capacity int
	Hits     uint64
	Misses   uint64
}

type cacheEntry struct {
	pattern string
	re      *regexp.Regexp
}

// RegexCache compiles and caches anchored matcher regexes (`^(?:...)$`) with a
// fixed LRU capacity so hot evaluation paths avoid recompilation without
// unbounded memory growth.
type RegexCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	items    map[string]*list.Element
	hits     uint64
	misses   uint64
}

// NewRegexCache creates a bounded regex cache. Non-positive capacities fall
// back to DefaultRegexCacheCapacity.
func NewRegexCache(capacity int) *RegexCache {
	if capacity <= 0 {
		capacity = DefaultRegexCacheCapacity
	}
	return &RegexCache{
		capacity: capacity,
		order:    list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Compile validates and compiles pattern as a fully anchored regex `^(?:...)$`,
// returning a cached instance on subsequent calls.
func (c *RegexCache) Compile(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > MaxRegexLength {
		return nil, fmt.Errorf("matcher regex exceeds maximum length of %d bytes", MaxRegexLength)
	}
	c.mu.Lock()
	if elem, ok := c.items[pattern]; ok {
		c.order.MoveToFront(elem)
		c.hits++
		re := elem.Value.(*cacheEntry).re
		c.mu.Unlock()
		return re, nil
	}
	c.mu.Unlock()

	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[pattern]; ok {
		c.order.MoveToFront(elem)
		c.hits++
		return elem.Value.(*cacheEntry).re, nil
	}
	c.misses++
	elem := c.order.PushFront(&cacheEntry{pattern: pattern, re: re})
	c.items[pattern] = elem
	for len(c.items) > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).pattern)
	}
	return re, nil
}

// Stats returns a point-in-time snapshot of cache usage and hit/miss counts.
func (c *RegexCache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Entries:  len(c.items),
		Capacity: c.capacity,
		Hits:     c.hits,
		Misses:   c.misses,
	}
}

var defaultRegexCache = NewRegexCache(DefaultRegexCacheCapacity)

// CompileRegex compiles an anchored matcher regex `^(?:...)$` using the
// process-wide bounded cache.
func CompileRegex(pattern string) (*regexp.Regexp, error) {
	return defaultRegexCache.Compile(pattern)
}

// DefaultCacheStats returns hit/miss and occupancy counters for the default cache.
func DefaultCacheStats() CacheStats {
	return defaultRegexCache.Stats()
}

// Compile validates and compiles a single Spec.
func Compile(spec Spec) (Compiled, error) {
	return CompileWithCache(spec, defaultRegexCache)
}

// CompileWithCache validates and compiles a single Spec using cache.
func CompileWithCache(spec Spec, cache *RegexCache) (Compiled, error) {
	if cache == nil {
		cache = defaultRegexCache
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return Compiled{}, errors.New("matcher name is required")
	}
	op := strings.TrimSpace(spec.Op)
	switch op {
	case OpEqual:
		return Compiled{name: name, op: OpEqual, value: spec.Value}, nil
	case OpRegex:
		re, err := cache.Compile(spec.Value)
		if err != nil {
			return Compiled{}, fmt.Errorf("regex %q is invalid: %w", spec.Value, err)
		}
		return Compiled{name: name, op: OpRegex, value: spec.Value, re: re}, nil
	default:
		return Compiled{}, fmt.Errorf("operator %q is unsupported", spec.Op)
	}
}

// CompileSet validates and compiles a non-empty slice of Specs.
func CompileSet(specs []Spec) (Set, error) {
	return CompileSetWithCache(specs, defaultRegexCache)
}

// CompileSetWithCache validates and compiles a non-empty slice of Specs using cache.
func CompileSetWithCache(specs []Spec, cache *RegexCache) (Set, error) {
	if len(specs) == 0 {
		return Set{}, errors.New("at least one matcher is required")
	}
	if len(specs) > MaxMatchers {
		return Set{}, fmt.Errorf("too many matchers: %d exceeds limit %d", len(specs), MaxMatchers)
	}
	compiled := make([]Compiled, 0, len(specs))
	for i, spec := range specs {
		m, err := CompileWithCache(spec, cache)
		if err != nil {
			return Set{}, fmt.Errorf("matcher %d: %w", i, err)
		}
		compiled = append(compiled, m)
	}
	return Set{matchers: compiled}, nil
}

// ValidateSet validates every matcher in specs without exposing compiled internals.
func ValidateSet(specs []Spec) error {
	_, err := CompileSet(specs)
	return err
}

// SpecsFromMaps converts persisted map-based matchers into strongly typed Specs.
func SpecsFromMaps(raw []map[string]string) []Spec {
	out := make([]Spec, len(raw))
	for i, item := range raw {
		out[i] = Spec{Name: item["name"], Value: item["value"], Op: item["op"]}
	}
	return out
}

// SpecsToMaps converts Specs into persistence-layer maps.
func SpecsToMaps(specs []Spec) []map[string]string {
	out := make([]map[string]string, len(specs))
	for i, item := range specs {
		out[i] = map[string]string{
			"name":  strings.TrimSpace(item.Name),
			"value": item.Value,
			"op":    strings.TrimSpace(item.Op),
		}
	}
	return out
}

// CompileSetsLenient compiles multiple matcher sets, skipping any invalid
// persisted set with a warning instead of aborting alert evaluation.
func CompileSetsLenient(raw [][]Spec) []Set {
	out := make([]Set, 0, len(raw))
	for _, specs := range raw {
		set, err := CompileSet(specs)
		if err != nil {
			slog.Warn("skip invalid persisted alert matcher set", "error", err)
			continue
		}
		out = append(out, set)
	}
	return out
}

// CompileSetsFromMapsLenient converts and compiles persisted silence matcher
// slices, skipping invalid entries safely.
func CompileSetsFromMapsLenient(raw [][]map[string]string) []Set {
	out := make([]Set, 0, len(raw))
	for _, item := range raw {
		set, err := CompileSet(SpecsFromMaps(item))
		if err != nil {
			slog.Warn("skip invalid persisted silence matchers", "error", err)
			continue
		}
		out = append(out, set)
	}
	return out
}

// CompileInhibitionRule validates and compiles a single inhibition rule.
func CompileInhibitionRule(source, target []Spec, equalLabels []string) (InhibitionRule, error) {
	sourceSet, err := CompileSet(source)
	if err != nil {
		return InhibitionRule{}, fmt.Errorf("source_matchers: %w", err)
	}
	targetSet, err := CompileSet(target)
	if err != nil {
		return InhibitionRule{}, fmt.Errorf("target_matchers: %w", err)
	}
	cleanedEqual := make([]string, 0, len(equalLabels))
	for i, label := range equalLabels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			return InhibitionRule{}, fmt.Errorf("equal_labels[%d] must not be empty", i)
		}
		cleanedEqual = append(cleanedEqual, trimmed)
	}
	return InhibitionRule{
		Source:      sourceSet,
		Target:      targetSet,
		EqualLabels: cleanedEqual,
	}, nil
}

// MatchesAny reports whether labels matches at least one compiled Set.
func MatchesAny(labels map[string]string, sets []Set) bool {
	for _, set := range sets {
		if set.Matches(labels) {
			return true
		}
	}
	return false
}

// IsInhibited reports whether any compiled inhibition rule suppresses target
// given the active firing sources.
func IsInhibited(target AlertRef, sources []AlertRef, rules []InhibitionRule) bool {
	for _, rule := range rules {
		if rule.Inhibits(target, sources) {
			return true
		}
	}
	return false
}
