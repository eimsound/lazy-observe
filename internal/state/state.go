// Package state contains shared-state abstractions used by gateway replicas.
package state

import (
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

type memoryLoginGate struct {
	mu       sync.Mutex
	failures map[string]memoryLimit
	locks    map[string]time.Time
	now      func() time.Time
}

// NewMemoryLoginGateWithClock returns a process-local LoginGate sharing the
// caller's clock.
func NewMemoryLoginGateWithClock(now func() time.Time) LoginGate {
	return &memoryLoginGate{failures: make(map[string]memoryLimit), locks: make(map[string]time.Time), now: now}
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
	entry := g.failures[key]
	if entry.start.IsZero() || now.Sub(entry.start) >= window {
		entry = memoryLimit{start: now}
	}
	entry.count++
	g.failures[key] = entry
	if entry.count >= threshold {
		g.locks[key] = now.Add(lockFor)
		return true, nil
	}
	return false, nil
}

func (g *memoryLoginGate) Locked(_ context.Context, key string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.locks[key]
	if !ok {
		return false, nil
	}
	if !until.After(g.now()) {
		delete(g.locks, key)
		return false, nil
	}
	return true, nil
}

func (g *memoryLoginGate) Clear(_ context.Context, key string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, key)
	delete(g.locks, key)
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
	start time.Time
	count int
}

type memoryLimiter struct {
	mu     sync.Mutex
	limits map[string]memoryLimit
	now    func() time.Time
}

func NewMemoryLimiter() Limiter { return newMemoryLimiter(time.Now) }

// NewMemoryLimiterWithClock is useful when the caller already owns a clock,
// keeping gateway behavior and deterministic tests on the same time source.
func NewMemoryLimiterWithClock(now func() time.Time) Limiter { return newMemoryLimiter(now) }

func newMemoryLimiter(now func() time.Time) Limiter {
	return &memoryLimiter{limits: make(map[string]memoryLimit), now: now}
}

func (l *memoryLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (bool, error) {
	if limit <= 0 || window <= 0 {
		return false, errors.New("limit and window must be positive")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry := l.limits[key]
	if entry.start.IsZero() || now.Sub(entry.start) >= window {
		entry = memoryLimit{start: now}
	}
	entry.count++
	l.limits[key] = entry
	return entry.count <= limit, nil
}
