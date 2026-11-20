// Package state contains shared-state abstractions used by gateway replicas.
package state

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("state entry not found")

// Session is the authentication state stored behind an opaque browser token.
type Session struct {
	Token     string    `json:"token"`
	UserID    int       `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type SessionStore interface {
	Create(context.Context, Session) error
	Get(context.Context, string) (Session, error)
	Delete(context.Context, string) error
	Touch(context.Context, string, time.Time) error
	ListByUser(context.Context, int, string) ([]SessionRef, error)
	DeleteByRef(context.Context, int, string) error
	DeleteOtherByUser(context.Context, int, string) error
}

// SessionRef is a user-facing session listing entry. The ID is opaque and
// never exposes the raw session token.
type SessionRef struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

// LoginGate tracks consecutive authentication failures and enforces a
// temporary account lockout after the threshold is crossed.
type LoginGate interface {
	// ConsumeFailure records one failed attempt and reports whether the
	// account became locked because of it.
	ConsumeFailure(ctx context.Context, key string, threshold int, window, lockFor time.Duration) (bool, error)
	// Locked reports whether the account is currently locked out.
	Locked(ctx context.Context, key string) (bool, error)
	// Clear resets the failure counter and any active lockout.
	Clear(ctx context.Context, key string) error
}

// Limiter implements a fixed-window counter. Implementations must increment and
// decide atomically so multiple gateway replicas observe one shared limit.
type Limiter interface {
	Allow(context.Context, string, int, time.Duration) (bool, error)
}

type memorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	now      func() time.Time
}

func NewMemorySessionStore() SessionStore {
	return newMemorySessionStore(time.Now)
}

func newMemorySessionStore(now func() time.Time) SessionStore {
	return &memorySessionStore{sessions: make(map[string]Session), now: now}
}

func (s *memorySessionStore) Create(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = s.now()
	}
	s.sessions[session.Token] = session
	return nil
}

func (s *memorySessionStore) Get(_ context.Context, token string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[token]
	if !ok {
		return Session{}, ErrNotFound
	}
	if !session.ExpiresAt.After(s.now()) {
		delete(s.sessions, token)
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (s *memorySessionStore) Delete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
	return nil
}

func (s *memorySessionStore) Touch(_ context.Context, token string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[token]
	if !ok || !session.ExpiresAt.After(s.now()) {
		delete(s.sessions, token)
		return ErrNotFound
	}
	session.ExpiresAt = expiresAt
	s.sessions[token] = session
	return nil
}

func (s *memorySessionStore) ListByUser(_ context.Context, userID int, currentToken string) ([]SessionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	result := make([]SessionRef, 0, 4)
	for token, session := range s.sessions {
		if session.UserID != userID {
			continue
		}
		if !session.ExpiresAt.After(now) {
			delete(s.sessions, token)
			continue
		}
		result = append(result, SessionRef{
			ID:        sessionRefID(token),
			CreatedAt: session.CreatedAt,
			ExpiresAt: session.ExpiresAt,
			Current:   currentToken != "" && token == currentToken,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (s *memorySessionStore) DeleteByRef(_ context.Context, userID int, refID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.sessions {
		if session.UserID == userID && sessionRefID(token) == refID {
			delete(s.sessions, token)
			return nil
		}
	}
	return ErrNotFound
}

func (s *memorySessionStore) DeleteOtherByUser(_ context.Context, userID int, keepToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.sessions {
		if session.UserID == userID && token != keepToken {
			delete(s.sessions, token)
		}
	}
	return nil
}

const (
	defaultMemoryMaxEntries = 65536
	memorySweepInterval     = time.Minute
)

type loginGateEntry struct {
	key           string
	failStart     time.Time
	failExpiresAt time.Time
	failCount     int
	lockUntil     time.Time
}

func (e *loginGateEntry) isLocked(now time.Time) bool {
	return !e.lockUntil.IsZero() && e.lockUntil.After(now)
}

func (e *loginGateEntry) isExpired(now time.Time) bool {
	failActive := !e.failExpiresAt.IsZero() && now.Before(e.failExpiresAt)
	return !failActive && !e.isLocked(now)
}

type memoryLoginGate struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List
	maxEntries int
	lastSweep  time.Time
	now        func() time.Time
}

// NewMemoryLoginGateWithClock returns a process-local LoginGate sharing the
// caller's clock with bounded capacity and expiry reclamation.
func NewMemoryLoginGateWithClock(now func() time.Time) LoginGate {
	return newMemoryLoginGateWithMax(now, defaultMemoryMaxEntries)
}

func newMemoryLoginGateWithMax(now func() time.Time, maxEntries int) *memoryLoginGate {
	if maxEntries <= 0 {
		maxEntries = defaultMemoryMaxEntries
	}
	return &memoryLoginGate{
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		maxEntries: maxEntries,
		now:        now,
	}
}

func (g *memoryLoginGate) removeElement(elem *list.Element) {
	entry := elem.Value.(*loginGateEntry)
	delete(g.entries, entry.key)
	g.order.Remove(elem)
}

func (g *memoryLoginGate) sweepExpired(now time.Time) {
	for elem := g.order.Back(); elem != nil; {
		prev := elem.Prev()
		entry := elem.Value.(*loginGateEntry)
		if entry.isExpired(now) {
			g.removeElement(elem)
		}
		elem = prev
	}
	g.lastSweep = now
}

func (g *memoryLoginGate) evictOne(now time.Time) {
	// Prefer evicting an unlocked entry so flooding random unknown usernames
	// cannot easily evict an active account lockout.
	for elem := g.order.Back(); elem != nil; elem = elem.Prev() {
		if !elem.Value.(*loginGateEntry).isLocked(now) {
			g.removeElement(elem)
			return
		}
	}
	if back := g.order.Back(); back != nil {
		g.removeElement(back)
	}
}

func (g *memoryLoginGate) ConsumeFailure(
	_ context.Context,
	key string,
	threshold int,
	window, lockFor time.Duration,
) (bool, error) {
	if key == "" || threshold <= 0 || window <= 0 || lockFor <= 0 {
		return false, errors.New("login gate arguments must be valid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.lastSweep.IsZero() || now.Sub(g.lastSweep) >= memorySweepInterval {
		g.sweepExpired(now)
	}
	elem, ok := g.entries[key]
	var entry *loginGateEntry
	if ok {
		entry = elem.Value.(*loginGateEntry)
		g.order.MoveToFront(elem)
	} else {
		if len(g.entries) >= g.maxEntries {
			g.sweepExpired(now)
		}
		if len(g.entries) >= g.maxEntries {
			g.evictOne(now)
		}
		entry = &loginGateEntry{key: key}
		g.entries[key] = g.order.PushFront(entry)
	}
	if entry.failStart.IsZero() || now.Sub(entry.failStart) >= window {
		entry.failStart = now
		entry.failCount = 0
	}
	entry.failExpiresAt = entry.failStart.Add(window)
	entry.failCount++
	if entry.failCount >= threshold {
		entry.lockUntil = now.Add(lockFor)
		return true, nil
	}
	return false, nil
}

func (g *memoryLoginGate) Locked(_ context.Context, key string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	elem, ok := g.entries[key]
	if !ok {
		return false, nil
	}
	now := g.now()
	entry := elem.Value.(*loginGateEntry)
	if entry.isLocked(now) {
		return true, nil
	}
	entry.lockUntil = time.Time{}
	if entry.isExpired(now) {
		g.removeElement(elem)
	}
	return false, nil
}

func (g *memoryLoginGate) Clear(_ context.Context, key string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if elem, ok := g.entries[key]; ok {
		g.removeElement(elem)
	}
	return nil
}

// tokenDigest is the stored form of a session token (SHA-256 hex).
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionRefID is the public, non-secret session ID: the first 48 bits of the
// token digest. digestRefID derives the same ID from a stored digest.
func sessionRefID(token string) string { return digestRefID(tokenDigest(token)) }

func digestRefID(digest string) string { return digest[:12] }

type memoryLimit struct {
	key       string
	start     time.Time
	expiresAt time.Time
	count     int
}

type memoryLimiter struct {
	mu         sync.Mutex
	limits     map[string]*list.Element
	order      *list.List
	maxEntries int
	lastSweep  time.Time
	now        func() time.Time
}

func NewMemoryLimiter() Limiter { return newMemoryLimiter(time.Now) }

// NewMemoryLimiterWithClock is useful when the caller already owns a clock,
// keeping gateway behavior and deterministic tests on the same time source.
func NewMemoryLimiterWithClock(now func() time.Time) Limiter { return newMemoryLimiter(now) }

func newMemoryLimiter(now func() time.Time) Limiter {
	return newMemoryLimiterWithMax(now, defaultMemoryMaxEntries)
}

func newMemoryLimiterWithMax(now func() time.Time, maxEntries int) *memoryLimiter {
	if maxEntries <= 0 {
		maxEntries = defaultMemoryMaxEntries
	}
	return &memoryLimiter{
		limits:     make(map[string]*list.Element),
		order:      list.New(),
		maxEntries: maxEntries,
		now:        now,
	}
}

func (l *memoryLimiter) removeElement(elem *list.Element) {
	entry := elem.Value.(*memoryLimit)
	delete(l.limits, entry.key)
	l.order.Remove(elem)
}

func (l *memoryLimiter) sweepExpired(now time.Time) {
	for elem := l.order.Back(); elem != nil; {
		prev := elem.Prev()
		entry := elem.Value.(*memoryLimit)
		if !now.Before(entry.expiresAt) {
			l.removeElement(elem)
		}
		elem = prev
	}
	l.lastSweep = now
}

func (l *memoryLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (bool, error) {
	if limit <= 0 || window <= 0 {
		return false, errors.New("limit and window must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= memorySweepInterval {
		l.sweepExpired(now)
	}
	elem, ok := l.limits[key]
	var entry *memoryLimit
	if ok {
		entry = elem.Value.(*memoryLimit)
		l.order.MoveToFront(elem)
	} else {
		if len(l.limits) >= l.maxEntries {
			l.sweepExpired(now)
		}
		if len(l.limits) >= l.maxEntries {
			if back := l.order.Back(); back != nil {
				l.removeElement(back)
			}
		}
		entry = &memoryLimit{key: key}
		l.limits[key] = l.order.PushFront(entry)
	}
	if entry.start.IsZero() || now.Sub(entry.start) >= window {
		entry.start = now
		entry.count = 0
	}
	entry.expiresAt = entry.start.Add(window)
	entry.count++
	return entry.count <= limit, nil
}
