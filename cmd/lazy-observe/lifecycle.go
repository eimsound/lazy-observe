package main

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

type cleanupEntry struct {
	name string
	fn   func() error
}

// cleanupStack releases initialized server resources in strict LIFO order.
// Calling Close is idempotent and runs every registered cleanup even if an
// earlier cleanup returns an error.
type cleanupStack struct {
	logger  *slog.Logger
	mu      sync.Mutex
	entries []cleanupEntry
	once    sync.Once
	err     error
}

func newCleanupStack(logger *slog.Logger) *cleanupStack {
	if logger == nil {
		logger = slog.Default()
	}
	return &cleanupStack{logger: logger}
}

func (s *cleanupStack) Push(name string, fn func() error) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	s.entries = append(s.entries, cleanupEntry{name: name, fn: fn})
	s.mu.Unlock()
}

func (s *cleanupStack) PushFunc(name string, fn func()) {
	if fn == nil {
		return
	}
	s.Push(name, func() error {
		fn()
		return nil
	})
}

func (s *cleanupStack) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		entries := make([]cleanupEntry, len(s.entries))
		copy(entries, s.entries)
		s.mu.Unlock()

		var errs []error
		for i := len(entries) - 1; i >= 0; i-- {
			entry := entries[i]
			if err := entry.fn(); err != nil {
				s.logger.Error("server resource cleanup failed", "resource", entry.name, "error", err)
				errs = append(errs, fmt.Errorf("%s: %w", entry.name, err))
			}
		}
		s.err = errors.Join(errs...)
	})
	return s.err
}
