package symbolicate

import (
	"container/list"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	gosourcemap "github.com/go-sourcemap/sourcemap"
)

const (
	// DefaultMaxCacheEntries bounds the number of cached parsed source maps.
	DefaultMaxCacheEntries = 128
	// DefaultMaxCacheBytes bounds the estimated heap footprint of cached parsed
	// source maps so large bundles cannot OOM the gateway process.
	DefaultMaxCacheBytes int64 = 64 << 20

	estimatedMappingBytes = 24
	sha256DigestBytes     = 32
)

type cacheEntry struct {
	key         string
	sizeBytes   int64
	mapConsumer *gosourcemap.Consumer
}

type Engine struct {
	blobs        blob.Store
	max          int
	maxBytes     int64
	mu           sync.Mutex
	lru          *list.List
	items        map[string]*list.Element
	currentBytes int64
	hits         uint64
}

func New(blobs blob.Store, max int) *Engine {
	return NewWithLimits(blobs, max, DefaultMaxCacheBytes)
}

func NewWithLimits(blobs blob.Store, maxEntries int, maxBytes int64) *Engine {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxCacheEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCacheBytes
	}
	return &Engine{
		blobs:    blobs,
		max:      maxEntries,
		maxBytes: maxBytes,
		lru:      list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (e *Engine) Symbolicate(
	ctx context.Context,
	projectSlug string,
	snapshotJSON json.RawMessage,
	event errortrack.Event,
) (errortrack.Event, error) {
	if e == nil || e.blobs == nil || !strings.EqualFold(event.Platform, "javascript") || event.Release == "" {
		return event, nil
	}
	var snapshot map[string]string
	if json.Unmarshal(snapshotJSON, &snapshot) != nil || snapshot["kind"] != "content-addressed-v1" ||
		snapshot["release"] != event.Release {
		return event, nil
	}
	event.Exception.Stacktrace.Frames = append([]errortrack.Frame(nil), event.Exception.Stacktrace.Frames...)
	for i := range event.Exception.Stacktrace.Frames {
		frame := &event.Exception.Stacktrace.Frames[i]
		if frame.Lineno == nil || frame.Colno == nil {
			continue
		}
		digest := matchSnapshotDigest(snapshot, frame.Filename)
		if !validDigest(digest) {
			continue
		}
		consumer, err := e.consumer(ctx, projectSlug, digest)
		if err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				continue
			}
			if errors.Is(err, ErrInvalidSourceMap) {
				slog.Warn(
					"skip invalid source map during symbolication",
					"project", projectSlug,
					"release", event.Release,
					"digest", digest,
					"error", err,
				)
				continue
			}
			// Transient storage errors (e.g. S3 timeout or 503) must fail the
			// attempt so the Inbox retries instead of fingerprinting unsymbolicated
			// frames and splitting Issues.
			return event, fmt.Errorf("load source map %s: %w", digest, err)
		}
		if consumer == nil {
			continue
		}
		source, name, line, column, ok, lookupErr := safeConsumerSource(consumer, *frame.Lineno, *frame.Colno)
		if lookupErr != nil {
			slog.Warn(
				"recovered panic in source map lookup",
				"project", projectSlug,
				"release", event.Release,
				"digest", digest,
				"error", lookupErr,
			)
			continue
		}
		if !ok {
			continue
		}
		frame.Filename, frame.Lineno, frame.Colno, frame.Symbolicated = source, intPtr(line), intPtr(column), true
		if name != "" {
			frame.Function = name
		}
	}
	return event, nil
}

func (e *Engine) consumer(ctx context.Context, project, digest string) (*gosourcemap.Consumer, error) {
	cacheKey := project + "\x00" + digest
	e.mu.Lock()
	if elem := e.items[cacheKey]; elem != nil {
		e.hits++
		e.lru.MoveToFront(elem)
		consumer := elem.Value.(*cacheEntry).mapConsumer
		e.mu.Unlock()
		return consumer, nil
	}
	e.mu.Unlock()
	key := RevisionKey(project, digest)
	r, err := e.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(r, MaxSourceMapBytes+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > MaxSourceMapBytes {
		return nil, fmt.Errorf("%w: blob exceeds %d bytes", ErrInvalidSourceMap, MaxSourceMapBytes)
	}
	normalized, segments, err := ValidateAndNormalize(data)
	if err != nil {
		return nil, err
	}
	if segments == 0 {
		// Valid v3 source map with empty mappings: nothing to resolve.
		return nil, nil
	}
	consumer, err := safeParseSourceMap(key, normalized)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSourceMap, err)
	}
	entrySize := int64(len(normalized)) + int64(segments)*estimatedMappingBytes
	if entrySize <= 0 {
		entrySize = 1
	}
	if entrySize > e.maxBytes {
		// Still usable for this lookup, but skip caching to respect memory budget.
		return consumer, nil
	}
	e.mu.Lock()
	if elem := e.items[cacheKey]; elem != nil {
		consumer = elem.Value.(*cacheEntry).mapConsumer
		e.lru.MoveToFront(elem)
	} else {
		e.items[cacheKey] = e.lru.PushFront(&cacheEntry{
			key:         cacheKey,
			sizeBytes:   entrySize,
			mapConsumer: consumer,
		})
		e.currentBytes += entrySize
		for e.lru.Len() > e.max || e.currentBytes > e.maxBytes {
			last := e.lru.Back()
			if last == nil {
				break
			}
			evicted := last.Value.(*cacheEntry)
			delete(e.items, evicted.key)
			e.lru.Remove(last)
			e.currentBytes -= evicted.sizeBytes
		}
	}
	e.mu.Unlock()
	return consumer, nil
}

func safeParseSourceMap(key string, data []byte) (consumer *gosourcemap.Consumer, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			consumer = nil
			err = fmt.Errorf("panic while parsing source map: %v", recovered)
		}
	}()
	return gosourcemap.Parse(key, data)
}

func safeConsumerSource(
	consumer *gosourcemap.Consumer,
	line, col int,
) (source, name string, origLine, origCol int, ok bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			source, name, origLine, origCol, ok = "", "", 0, 0, false
			err = fmt.Errorf("panic while resolving source map position: %v", recovered)
		}
	}()
	source, name, origLine, origCol, ok = consumer.Source(line, col)
	return source, name, origLine, origCol, ok, nil
}

func (e *Engine) CacheStats() (size int, hits uint64) {
	if e == nil {
		return 0, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lru.Len(), e.hits
}

func (e *Engine) CacheBytes() int64 {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentBytes
}

// matchSnapshotDigest resolves a frame's filename against the pinned source map
// snapshot by trying longest matching path suffixes first (e.g.
// "static/chunks/index.js" -> "chunks/index.js" -> "index.js"). This avoids
// cross-file collisions when multiple directories share a basename while
// remaining compatible with basename-only uploads.
func matchSnapshotDigest(snapshot map[string]string, frameFilename string) string {
	candidates := framePathCandidates(frameFilename)
	for _, candidate := range candidates {
		if digest := snapshot["file:"+candidate]; digest != "" {
			return digest
		}
	}
	return ""
}

func framePathCandidates(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	value = strings.SplitN(value, "?", 2)[0]
	value = strings.SplitN(value, "#", 2)[0]
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.Contains(value, "://") {
		if u, err := url.Parse(value); err == nil && u.Path != "" {
			value = u.Path
		}
	}
	cleaned := path.Clean("/" + value)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return nil
	}
	parts := strings.Split(cleaned, "/")
	candidates := make([]string, 0, len(parts))
	for i := range parts {
		candidate := strings.Join(parts[i:], "/")
		if candidate != "" && candidate != "." {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256DigestBytes && strings.ToLower(value) == value
}

func intPtr(v int) *int { return &v }

// RevisionKey is the blob key of one content-addressed source map revision.
// Uploads, symbolication, and retention must all agree on it.
func RevisionKey(project, digest string) string {
	return "sourcemaps/" + project + "/revisions/" + digest + ".map"
}
