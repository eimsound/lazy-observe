package ingestlimit

import (
	"strings"
	"sync"
	"time"
)

// DefaultMaxWindowKeys bounds the number of keys one Window limiter tracks so
// high-cardinality client IPs or session IDs cannot grow process memory
// without limit.
const DefaultMaxWindowKeys = 65536

// WindowConfig configures a per-key fixed-window limiter.
type WindowConfig struct {
	// Limit is the maximum number of admitted events per key per window.
	// Values <= 0 disable the limiter.
	Limit int
	// Window is the fixed window length. Values <= 0 default to one minute.
	Window time.Duration
	// MaxKeys bounds tracked keys. Values <= 0 default to DefaultMaxWindowKeys.
	MaxKeys int
	// Now injects a clock for tests. Nil uses time.Now.
	Now func() time.Time
}

type windowEntry struct {
	key   string
	start time.Time
	count int
	prev  *windowEntry
	next  *windowEntry
}

// Window is a process-local, concurrency-safe fixed-window counter keyed by
// string. It never performs network I/O and runs no background goroutines.
//
// Entries are ordered by window start (newest at the head), so the tail is
// always the entry whose window resets first. Expired entries are swept from
// the tail on insert; when the limiter is full the tail is evicted, which
// sacrifices the counter closest to resetting anyway.
type Window struct {
	limit   int
	window  time.Duration
	maxKeys int
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*windowEntry
	head    *windowEntry
	tail    *windowEntry
}

// NewWindow constructs a fixed-window limiter from cfg.
func NewWindow(cfg WindowConfig) *Window {
	window := cfg.Window
	if window <= 0 {
		window = time.Minute
	}
	maxKeys := cfg.MaxKeys
	if maxKeys <= 0 {
		maxKeys = DefaultMaxWindowKeys
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Window{
		limit:   cfg.Limit,
		window:  window,
		maxKeys: maxKeys,
		now:     now,
		entries: make(map[string]*windowEntry),
	}
}

// Enabled reports whether the limiter enforces a positive limit.
func (w *Window) Enabled() bool {
	return w != nil && w.limit > 0
}

// TrackedKeys returns the current number of tracked keys.
func (w *Window) TrackedKeys() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	n := len(w.entries)
	w.mu.Unlock()
	return n
}

// Allow counts one event for key. Rejected events do not consume quota. When
// rejected, Decision.RetryAfter is the whole-second time until the key's
// window resets, at least one second.
func (w *Window) Allow(key string) Decision {
	if !w.Enabled() {
		return Decision{Allowed: true}
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if w.head != nil && now.Before(w.head.start) {
		// Clamp a clock that stepped backwards so window starts stay
		// monotonically ordered from head to tail.
		now = w.head.start
	}
	entry, ok := w.entries[key]
	if ok && now.Sub(entry.start) >= w.window {
		// Window elapsed: start a new one and move the entry to the head to
		// keep start-time ordering.
		entry.start = now
		entry.count = 0
		w.moveToFrontLocked(entry)
	}
	if !ok {
		w.sweepExpiredLocked(now)
		for len(w.entries) >= w.maxKeys && w.tail != nil {
			w.removeLocked(w.tail)
		}
		// Clone: callers may pass strings backed by reusable request buffers.
		key = strings.Clone(key)
		entry = &windowEntry{key: key, start: now}
		w.entries[key] = entry
		w.pushFrontLocked(entry)
	}
	if entry.count >= w.limit {
		return Decision{Allowed: false, RetryAfter: retryAfterWindow(entry.start.Add(w.window).Sub(now))}
	}
	entry.count++
	return Decision{Allowed: true}
}

func retryAfterWindow(remaining time.Duration) time.Duration {
	if remaining <= time.Second {
		return time.Second
	}
	secs := remaining / time.Second
	if remaining%time.Second != 0 {
		secs++
	}
	return secs * time.Second
}

func (w *Window) sweepExpiredLocked(now time.Time) {
	for w.tail != nil && now.Sub(w.tail.start) >= w.window {
		w.removeLocked(w.tail)
	}
}

func (w *Window) pushFrontLocked(e *windowEntry) {
	e.prev = nil
	e.next = w.head
	if w.head != nil {
		w.head.prev = e
	} else {
		w.tail = e
	}
	w.head = e
}

func (w *Window) moveToFrontLocked(e *windowEntry) {
	if w.head == e {
		return
	}
	w.unlinkLocked(e)
	w.pushFrontLocked(e)
}

func (w *Window) removeLocked(e *windowEntry) {
	delete(w.entries, e.key)
	w.unlinkLocked(e)
}

func (w *Window) unlinkLocked(e *windowEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		w.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		w.tail = e.prev
	}
	e.prev = nil
	e.next = nil
}
