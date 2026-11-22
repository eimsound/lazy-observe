package meta

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime                  = uint32(1)
	argonMemory                = uint32(64 * 1024)
	argonThreads               = uint8(4)
	argonKeyLen                = uint32(32)
	maxArgonMemory             = uint32(512 * 1024)
	defaultArgonConcurrency    = 4
	defaultArgonAcquireTimeout = 2 * time.Second
	argonReclaimMinMemoryKB    = uint32(8 * 1024)
	argonReclaimMinInterval    = 250 * time.Millisecond
)

// ErrPasswordVerificationBusy reports that all bounded Argon2id verification
// slots are occupied. Callers must map it to 503 (never 401 or lockout) so
// password hashing concurrency stays capped below container memory limits.
var ErrPasswordVerificationBusy = errors.New("password verification busy")

// IsPasswordVerificationBusy reports whether an error indicates Argon2id
// concurrency saturation.
func IsPasswordVerificationBusy(err error) bool {
	return errors.Is(err, ErrPasswordVerificationBusy)
}

type argonLimiter struct {
	sem     chan struct{}
	timeout time.Duration
}

var (
	argonLimiterMu      sync.RWMutex
	defaultArgonLimiter = &argonLimiter{
		sem:     make(chan struct{}, defaultArgonConcurrency),
		timeout: defaultArgonAcquireTimeout,
	}
)

func currentArgonLimiter() *argonLimiter {
	argonLimiterMu.RLock()
	limiter := defaultArgonLimiter
	argonLimiterMu.RUnlock()
	return limiter
}

func acquireArgonSlot(ctx context.Context) (func(), error) {
	limiter := currentArgonLimiter()
	select {
	case limiter.sem <- struct{}{}:
		return func() { <-limiter.sem }, nil
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(limiter.timeout)
	defer timer.Stop()
	select {
	case limiter.sem <- struct{}{}:
		return func() { <-limiter.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrPasswordVerificationBusy
	}
}

func acquireArgonSlotBlocking() func() {
	limiter := currentArgonLimiter()
	limiter.sem <- struct{}{}
	return func() { <-limiter.sem }
}

// HashPassword returns a self-describing Argon2id password hash.
func HashPassword(password string) (string, error) {
	return hashPasswordWithRelease(password, acquireArgonSlotBlocking())
}

// HashPasswordContext returns a self-describing Argon2id password hash while
// respecting the bounded Argon2id concurrency semaphore and ctx cancellation.
func HashPasswordContext(ctx context.Context, password string) (string, error) {
	release, err := acquireArgonSlot(ctx)
	if err != nil {
		return "", err
	}
	return hashPasswordWithRelease(password, release)
}

func hashPasswordWithRelease(password string, release func()) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		release()
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := runArgon2IDKey(release, []byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory,
		argonTime,
		argonThreads,
		b64.EncodeToString(salt),
		b64.EncodeToString(key),
	), nil
}

// VerifyPassword checks a password against a HashPassword encoding.
func VerifyPassword(encoded, password string) bool {
	memory, iterations, threads, salt, want, ok := parseArgon2Hash(encoded)
	if !ok {
		return false
	}
	release := acquireArgonSlotBlocking()
	got := runArgon2IDKey(release, []byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// VerifyPasswordContext checks a password against a HashPassword encoding while
// respecting the bounded Argon2id concurrency semaphore and ctx cancellation.
func VerifyPasswordContext(ctx context.Context, encoded, password string) (bool, error) {
	memory, iterations, threads, salt, want, ok := parseArgon2Hash(encoded)
	if !ok {
		return false, nil
	}
	release, err := acquireArgonSlot(ctx)
	if err != nil {
		return false, err
	}
	got := runArgon2IDKey(release, []byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func parseArgon2Hash(encoded string) (memory, iterations uint32, threads uint8, salt, want []byte, ok bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return 0, 0, 0, nil, nil, false
	}
	if _, err := fmt.Sscanf(
		parts[3],
		"m=%d,t=%d,p=%d",
		&memory,
		&iterations,
		&threads,
	); err != nil || memory == 0 || memory > maxArgonMemory || iterations == 0 ||
		threads == 0 {
		return 0, 0, 0, nil, nil, false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return 0, 0, 0, nil, nil, false
	}
	want, err = b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return 0, 0, 0, nil, nil, false
	}
	return memory, iterations, threads, salt, want, true
}

var errInvalidCredentials = errors.New("invalid credentials")

// IsInvalidCredentials reports whether authentication failed because the
// supplied username/password pair was invalid. Callers must still return one
// generic response for missing users and incorrect passwords.
func IsInvalidCredentials(err error) bool {
	return errors.Is(err, errInvalidCredentials)
}

func argon2DummyContext(ctx context.Context, password string) ([]byte, error) {
	release, err := acquireArgonSlot(ctx)
	if err != nil {
		return nil, err
	}
	return runArgon2IDKey(
		release,
		[]byte(password),
		[]byte("lazy-observe-dummy"),
		argonTime,
		argonMemory,
		argonThreads,
		argonKeyLen,
	), nil
}

// runArgon2IDKey executes argon2.IDKey while coordinating OS heap reclamation
// for large transient Argon2id memory blocks. Defer order ensures the
// concurrency slot is released before any synchronous FreeOSMemory run.
func runArgon2IDKey(
	release func(),
	password, salt []byte,
	iterations, memory uint32,
	threads uint8,
	keyLen uint32,
) []byte {
	if defaultArgonReclaimer.enter(memory) {
		defer defaultArgonReclaimer.leave()
	}
	if release != nil {
		defer release()
	}
	return argon2.IDKey(password, salt, iterations, memory, threads, keyLen)
}

type argonMemoryReclaimer struct {
	mu          sync.Mutex
	inFlight    int
	running     bool
	pending     bool
	lastReclaim time.Time
	timer       *time.Timer
	minInterval time.Duration
	now         func() time.Time
	reclaimFn   func()
	afterFunc   func(time.Duration, func()) *time.Timer
}

var defaultArgonReclaimer = newArgonMemoryReclaimer(
	argonReclaimMinInterval,
	time.Now,
	debug.FreeOSMemory,
	time.AfterFunc,
)

func newArgonMemoryReclaimer(
	minInterval time.Duration,
	now func() time.Time,
	reclaimFn func(),
	afterFunc func(time.Duration, func()) *time.Timer,
) *argonMemoryReclaimer {
	return &argonMemoryReclaimer{
		minInterval: minInterval,
		now:         now,
		reclaimFn:   reclaimFn,
		afterFunc:   afterFunc,
	}
}

func (r *argonMemoryReclaimer) enter(memoryKB uint32) bool {
	if memoryKB < argonReclaimMinMemoryKB {
		return false
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
	return true
}

func (r *argonMemoryReclaimer) leave() {
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	if r.inFlight > 0 {
		r.mu.Unlock()
		return
	}
	if r.running {
		r.pending = true
		r.mu.Unlock()
		return
	}
	if r.minInterval > 0 && !r.lastReclaim.IsZero() {
		elapsed := r.now().Sub(r.lastReclaim)
		if elapsed < r.minInterval {
			if r.timer == nil {
				r.timer = r.afterFunc(r.minInterval-elapsed, r.onTimer)
			}
			r.mu.Unlock()
			return
		}
	}
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.running = true
	r.mu.Unlock()

	r.runReclaim()
}

func (r *argonMemoryReclaimer) onTimer() {
	r.mu.Lock()
	r.timer = nil
	if r.inFlight > 0 {
		r.mu.Unlock()
		return
	}
	if r.running {
		r.pending = true
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	r.runReclaim()
}

func (r *argonMemoryReclaimer) runReclaim() {
	for {
		func() {
			completed := false
			defer func() {
				if !completed {
					r.mu.Lock()
					r.running = false
					r.mu.Unlock()
				}
			}()
			if r.reclaimFn != nil {
				r.reclaimFn()
			}
			completed = true
		}()

		r.mu.Lock()
		r.lastReclaim = r.now()
		pending := r.pending
		r.pending = false
		if !pending || r.inFlight > 0 {
			r.running = false
			r.mu.Unlock()
			return
		}
		if r.minInterval > 0 {
			r.running = false
			if r.timer == nil {
				r.timer = r.afterFunc(r.minInterval, r.onTimer)
			}
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
	}
}

func (r *argonMemoryReclaimer) stop() {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.mu.Unlock()
}
