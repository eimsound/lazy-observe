package main

import "sync"

type validationKey struct {
	kind   signalKind
	digest [32]byte
}

type validationCall struct {
	done chan struct{}
	err  error
}

// validationCache is a bounded FIFO with per-key request coalescing. Cache hits
// take only a shared read lock, keeping identical benchmark requests concurrent.
// Only fully validated payloads enter the cache; signalKind is part of the key
// so a body accepted as metrics can never bypass logs validation (or vice versa).
type validationCache struct {
	mu         sync.RWMutex
	capacity   int
	entries    map[validationKey]struct{}
	order      []validationKey
	nextEvict  int
	inProgress map[validationKey]*validationCall
}

func newValidationCache(capacity int) *validationCache {
	if capacity <= 0 {
		panic("validation cache capacity must be positive")
	}
	return &validationCache{
		capacity:   capacity,
		entries:    make(map[validationKey]struct{}, capacity),
		order:      make([]validationKey, 0, capacity),
		inProgress: make(map[validationKey]*validationCall),
	}
}

func (c *validationCache) validate(key validationKey, validate func() error) error {
	c.mu.RLock()
	_, cached := c.entries[key]
	c.mu.RUnlock()
	if cached {
		return nil
	}

	c.mu.Lock()
	if _, cached = c.entries[key]; cached {
		c.mu.Unlock()
		return nil
	}
	if call := c.inProgress[key]; call != nil {
		c.mu.Unlock()
		<-call.done
		return call.err
	}
	call := &validationCall{done: make(chan struct{})}
	c.inProgress[key] = call
	c.mu.Unlock()

	err := validate()

	c.mu.Lock()
	delete(c.inProgress, key)
	if err == nil {
		c.entries[key] = struct{}{}
		if len(c.order) < c.capacity {
			c.order = append(c.order, key)
		} else {
			delete(c.entries, c.order[c.nextEvict])
			c.order[c.nextEvict] = key
			c.nextEvict = (c.nextEvict + 1) % c.capacity
		}
	}
	call.err = err
	close(call.done)
	c.mu.Unlock()
	return err
}

func (c *validationCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
