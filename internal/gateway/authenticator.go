package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	tokenCacheTTL            = 30 * time.Second
	defaultAuthLookupTimeout = 5 * time.Second
)

type cachedProject struct {
	project   meta.ProjectInfo
	expiresAt time.Time
}

var errAuthenticationRevisionChanged = errors.New("authentication store revision changed")

type authenticationCall struct {
	done     chan struct{}
	revision uint64
	project  meta.ProjectInfo
	err      error
	panicked bool
	panicVal any
}

// Authenticator resolves a bearer credential to a project through one lookup. Two
// instances exist: ingest (project API tokens) and source map upload (ADR 0018);
// each only ever consults its own token table.
//
// Cache hits take only a shared RLock; misses, expiry evictions, revision
// changes, and explicit invalidations take the write lock.
type Authenticator struct {
	revisionFn     func() uint64
	lookup         func(context.Context, string) (meta.ProjectInfo, error)
	invalid        error
	configured     bool
	lookupTimeout  time.Duration
	now            func() time.Time
	remoteRevision atomic.Uint64
	mu             sync.RWMutex
	cache          map[string]cachedProject
	inflight       map[string]*authenticationCall
	revision       uint64
}

func NewAuthenticator(store TokenStore) *Authenticator {
	a := newAuthenticator()
	if store != nil {
		a.revisionFn = store.Revision
		a.lookup = store.AuthenticateProjectAPIToken
		a.invalid = meta.ErrInvalidProjectAPIToken
		a.configured = true
	}
	return a
}

func newUploadAuthenticator(store UploadTokenStore) *Authenticator {
	a := newAuthenticator()
	if store != nil {
		a.revisionFn = store.Revision
		a.lookup = store.AuthenticateSourceMapUploadToken
		a.invalid = meta.ErrInvalidSourceMapUploadToken
		a.configured = true
	}
	return a
}

func newAuthenticator() *Authenticator {
	return &Authenticator{
		lookupTimeout: defaultAuthLookupTimeout,
		now:           time.Now,
		cache:         make(map[string]cachedProject),
		inflight:      make(map[string]*authenticationCall),
	}
}

func (a *Authenticator) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *Authenticator) currentRevision() uint64 {
	var local uint64
	if a.revisionFn != nil {
		local = a.revisionFn()
	}
	return local + a.remoteRevision.Load()
}

// Invalidate advances the remote invalidation epoch and clears cached entries
// immediately. Any in-flight lookup started before this call detects the
// revision change and retries instead of caching stale state.
func (a *Authenticator) Invalidate() {
	a.remoteRevision.Add(1)
	a.mu.Lock()
	clear(a.cache)
	a.revision = a.currentRevision()
	a.mu.Unlock()
}

// Middleware authenticates ingest routes with a project API token. A source map
// upload token is refused explicitly: it is a different credential kind.
func (a *Authenticator) Middleware(c fiber.Ctx) error {
	cred := classifyBearer(c)
	if cred.kind == bearerUploadToken {
		return uploadOnlyToken(c)
	}
	return a.authenticateRequest(c, cred.token)
}

// UploadMiddleware authenticates the source map upload route. Only a source map
// upload token is accepted; project API tokens and user tokens get 403 with
// source_map_upload_token_required, anything else 401.
func (a *Authenticator) UploadMiddleware(c fiber.Ctx) error {
	cred := classifyBearer(c)
	if cred.kind == bearerProjectToken || cred.kind == bearerUserToken {
		return sourceMapUploadTokenRequired(c)
	}
	return a.authenticateRequest(c, cred.token)
}

func (a *Authenticator) authenticateRequest(c fiber.Ctx, token string) error {
	if token == "" || !a.configured {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	p, err := a.authenticate(c.Context(), token)
	if err != nil {
		if !errors.Is(err, a.invalid) && !meta.IsNotFound(err) {
			return c.Status(fiber.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "authentication temporarily unavailable"})
		}
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	c.Locals(projectLocal, p)
	return c.Next()
}

func (a *Authenticator) authenticate(ctx context.Context, token string) (meta.ProjectInfo, error) {
	for {
		now := a.clock()
		// Hot path: shared RLock for live cache hits at the current revision.
		a.mu.RLock()
		revision := a.currentRevision()
		if revision == a.revision {
			if cached, ok := a.cache[token]; ok && now.Before(cached.expiresAt) {
				project := cached.project
				a.mu.RUnlock()
				return project, nil
			}
		}
		a.mu.RUnlock()

		// Slow path: write lock for revision rollover, expiry eviction, or miss.
		a.mu.Lock()
		revision = a.currentRevision()
		if revision != a.revision {
			clear(a.cache)
			a.revision = revision
		}
		if cached, ok := a.cache[token]; ok && now.Before(cached.expiresAt) {
			project := cached.project
			a.mu.Unlock()
			return project, nil
		}
		delete(a.cache, token)
		if call, ok := a.inflight[token]; ok {
			done := call.done
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return meta.ProjectInfo{}, ctx.Err()
			case <-done:
			}
			if errors.Is(call.err, errAuthenticationRevisionChanged) {
				continue
			}
			return call.project, call.err
		}

		// The caller's token may borrow Fiber's reusable request buffer, which
		// the next request on the same context overwrites. A borrowed map key
		// would then silently become another request's token: a different
		// project's token, or one that was never issued, would match this
		// entry and authenticate as this project. Store only an owned copy.
		// Lookups above may use the borrowed value; only stored keys must own
		// their bytes.
		key := strings.Clone(token)
		call := &authenticationCall{done: make(chan struct{}), revision: revision}
		a.inflight[key] = call
		a.mu.Unlock()

		// Decouple upstream lookup from the leader's cancellation so a single
		// client disconnect cannot fail concurrent waiters sharing this call,
		// while bounding the lookup with an independent timeout.
		timeout := a.lookupTimeout
		if timeout <= 0 {
			timeout = defaultAuthLookupTimeout
		}
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		go a.runLookup(lookupCtx, cancel, key, call)

		select {
		case <-ctx.Done():
			return meta.ProjectInfo{}, ctx.Err()
		case <-call.done:
		}
		if call.panicked {
			panic(call.panicVal)
		}
		if errors.Is(call.err, errAuthenticationRevisionChanged) {
			continue
		}
		return call.project, call.err
	}
}

func (a *Authenticator) runLookup(
	ctx context.Context,
	cancel context.CancelFunc,
	key string,
	call *authenticationCall,
) {
	defer cancel()
	completed := false
	defer func() {
		r := recover()
		if !completed {
			a.mu.Lock()
			if r != nil {
				call.panicked = true
				call.panicVal = r
				call.err = fmt.Errorf("authentication lookup panic: %v", r)
			} else {
				call.err = errors.New("authentication lookup aborted")
			}
			if a.inflight[key] == call {
				delete(a.inflight, key)
			}
			close(call.done)
			a.mu.Unlock()
		}
	}()

	project, authErr := a.lookup(ctx, key)
	a.mu.Lock()
	latestRevision := a.currentRevision()
	if latestRevision != a.revision {
		clear(a.cache)
		a.revision = latestRevision
	}
	if latestRevision != call.revision {
		call.err = errAuthenticationRevisionChanged
	} else {
		call.project, call.err = project, authErr
		if authErr == nil {
			a.cache[key] = cachedProject{
				project:   project,
				expiresAt: a.clock().Add(tokenCacheTTL),
			}
		}
	}
	if a.inflight[key] == call {
		delete(a.inflight, key)
	}
	completed = true
	close(call.done)
	a.mu.Unlock()
}
