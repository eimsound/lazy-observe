package symbolicate

import (
	"container/list"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	gosourcemap "github.com/go-sourcemap/sourcemap"
)

type cacheEntry struct {
	key         string
	mapConsumer *gosourcemap.Consumer
}

type Engine struct {
	blobs blob.Store
	max   int
	mu    sync.Mutex
	lru   *list.List
	items map[string]*list.Element
	hits  uint64
}

func New(blobs blob.Store, max int) *Engine {
	if max <= 0 {
		max = 128
	}
	return &Engine{blobs: blobs, max: max, lru: list.New(), items: make(map[string]*list.Element)}
}

func (e *Engine) Symbolicate(
	ctx context.Context,
	projectSlug string,
	snapshotJSON json.RawMessage,
	event errortrack.Event,
) errortrack.Event {
	if e == nil || e.blobs == nil || !strings.EqualFold(event.Platform, "javascript") || event.Release == "" {
		return event
	}
	var snapshot map[string]string
	if json.Unmarshal(snapshotJSON, &snapshot) != nil || snapshot["kind"] != "content-addressed-v1" ||
		snapshot["release"] != event.Release {
		return event
	}
	event.Exception.Stacktrace.Frames = append([]errortrack.Frame(nil), event.Exception.Stacktrace.Frames...)
	for i := range event.Exception.Stacktrace.Frames {
		frame := &event.Exception.Stacktrace.Frames[i]
		if frame.Lineno == nil || frame.Colno == nil {
			continue
		}
		filename := frameBasename(frame.Filename)
		if filename == "" {
			continue
		}
		digest := snapshot["file:"+filename]
		if !validDigest(digest) {
			continue
		}
		consumer, err := e.consumer(ctx, projectSlug, digest)
		if err != nil {
			continue
		}
		source, name, line, column, ok := consumer.Source(*frame.Lineno, *frame.Colno)
		if !ok {
			continue
		}
		frame.Filename, frame.Lineno, frame.Colno, frame.Symbolicated = source, intPtr(line), intPtr(column), true
		if name != "" {
			frame.Function = name
		}
	}
	return event
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
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	consumer, err := gosourcemap.Parse(key, data)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if elem := e.items[cacheKey]; elem != nil {
		consumer = elem.Value.(*cacheEntry).mapConsumer
		e.lru.MoveToFront(elem)
	} else {
		e.items[cacheKey] = e.lru.PushFront(&cacheEntry{key: cacheKey, mapConsumer: consumer})
		if e.lru.Len() > e.max {
			last := e.lru.Back()
			delete(e.items, last.Value.(*cacheEntry).key)
			e.lru.Remove(last)
		}
	}
	e.mu.Unlock()
	return consumer, nil
}

func (e *Engine) CacheStats() (size int, hits uint64) {
	if e == nil {
		return 0, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lru.Len(), e.hits
}

func frameBasename(value string) string {
	value = strings.SplitN(value, "?", 2)[0]
	value = strings.SplitN(value, "#", 2)[0]
	return path.Base(strings.ReplaceAll(value, "\\", "/"))
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256DigestBytes && strings.ToLower(value) == value
}

const sha256DigestBytes = 32

func intPtr(v int) *int { return &v }

// RevisionKey is the blob key of one content-addressed source map revision.
// Uploads, symbolication, and retention must all agree on it.
func RevisionKey(project, digest string) string {
	return "sourcemaps/" + project + "/revisions/" + digest + ".map"
}
