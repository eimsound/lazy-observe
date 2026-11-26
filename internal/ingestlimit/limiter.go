// Package ingestlimit provides process-local, bounded, concurrency-safe ingest
// rate limiters: a per-project token bucket shared by HTTP and OTLP/gRPC ingest
// entrypoints (ingest_rate_limit) and a per-key fixed window used by endpoint
// anti-abuse guardrails. Neither performs network I/O; every limit applies per
// application replica (ADR 0020).
package ingestlimit

import (
	"math"
	"sync"
	"time"
)

const (
	// DefaultRequestsPerSecond is the default steady-state request rate per project.
	DefaultRequestsPerSecond = 500.0
	// DefaultBurst is the default request token-bucket burst capacity per project.
	DefaultBurst = 1000
	// DefaultMaxTrackedProjects bounds the in-memory per-project state map so
	// arbitrary project churn cannot cause unbounded memory growth.
	DefaultMaxTrackedProjects = 4096
	// DefaultMinRetryAfter is the minimum Retry-After duration reported on rejection.
	DefaultMinRetryAfter = time.Second
	// DefaultMaxRetryAfter caps Retry-After durations returned to clients.
	DefaultMaxRetryAfter = 5 * time.Minute

	defaultIdleBucketTTL = 15 * time.Minute
)

// Config configures a per-project token-bucket ingest rate limiter.
type Config struct {
	Enabled            bool
	RequestsPerSecond  float64
	Burst              int
	BytesPerSecond     int64
	ByteBurst          int64
	MaxTrackedProjects int
	MaxRetryAfter      time.Duration
	IdleTTL            time.Duration
	Now                func() time.Time
}

// Decision describes the outcome of one rate-limit admission check.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type projectBucket struct {
	projectID  int
	reqTokens  float64
	byteTokens float64
	lastRefill time.Time
	lastSeen   time.Time
	prev       *projectBucket
	next       *projectBucket
}

// Limiter enforces per-project request and optional byte token buckets with
// bounded LRU + idle eviction and zero background goroutines.
type Limiter struct {
	enabled            bool
	reqRate            float64
	burst              float64
	byteRate           float64
	byteBurst          float64
	maxTrackedProjects int
	maxRetryAfter      time.Duration
	idleTTL            time.Duration
	now                func() time.Time

	mu      sync.Mutex
	buckets map[int]*projectBucket
	head    *projectBucket
	tail    *projectBucket
}

// New constructs a Limiter from cfg. When cfg.Enabled is false, Allow always
// permits requests with zero locking overhead.
func New(cfg Config) *Limiter {
	reqRate := cfg.RequestsPerSecond
	if math.IsNaN(reqRate) || math.IsInf(reqRate, 0) || reqRate <= 0 {
		reqRate = DefaultRequestsPerSecond
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = DefaultBurst
	}
	byteRate := cfg.BytesPerSecond
	if byteRate < 0 {
		byteRate = 0
	}
	byteBurst := cfg.ByteBurst
	if byteRate > 0 && byteBurst <= 0 {
		byteBurst = byteRate
	}
	maxProjects := cfg.MaxTrackedProjects
	if maxProjects <= 0 {
		maxProjects = DefaultMaxTrackedProjects
	}
	maxRetryAfter := cfg.MaxRetryAfter
	if maxRetryAfter < DefaultMinRetryAfter {
		maxRetryAfter = DefaultMaxRetryAfter
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = defaultIdleBucketTTL
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	l := &Limiter{
		enabled:            cfg.Enabled,
		reqRate:            reqRate,
		burst:              float64(burst),
		byteRate:           float64(byteRate),
		byteBurst:          float64(byteBurst),
		maxTrackedProjects: maxProjects,
		maxRetryAfter:      maxRetryAfter,
		idleTTL:            idleTTL,
		now:                nowFn,
	}
	if cfg.Enabled {
		l.buckets = make(map[int]*projectBucket)
	}
	return l
}

// Enabled reports whether rate limiting is active.
func (l *Limiter) Enabled() bool {
	return l != nil && l.enabled
}

// ByteLimitEnabled reports whether per-project byte-rate limiting is active.
func (l *Limiter) ByteLimitEnabled() bool {
	return l != nil && l.enabled && l.byteRate > 0
}

// TrackedProjects returns the current number of tracked project buckets.
func (l *Limiter) TrackedProjects() int {
	if !l.Enabled() {
		return 0
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	return n
}

// Allow checks whether one ingest request for projectID carrying payloadBytes
// is permitted under the project's request and byte token buckets.
//
// Tokens are deducted atomically only when both buckets allow the request.
// When rejected, Decision.RetryAfter is a positive whole-second duration in
// [1s, MaxRetryAfter].
func (l *Limiter) Allow(projectID int, payloadBytes int64) Decision {
	if !l.Enabled() {
		return Decision{Allowed: true}
	}
	if payloadBytes < 0 {
		payloadBytes = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket := l.lookupOrCreateLocked(projectID, now)
	l.refillLocked(bucket, now)

	reqOK := bucket.reqTokens >= 1.0
	var waitSec float64
	if !reqOK {
		waitSec = (1.0 - bucket.reqTokens) / l.reqRate
	}

	byteOK := true
	costBytes := float64(payloadBytes)
	if l.byteRate > 0 && costBytes > 0 {
		if costBytes <= l.byteBurst {
			if bucket.byteTokens < costBytes {
				byteOK = false
				byteWait := (costBytes - bucket.byteTokens) / l.byteRate
				if byteWait > waitSec {
					waitSec = byteWait
				}
			}
		} else {
			// A single payload larger than byteBurst is allowed once the bucket
			// has refilled to full burst capacity, borrowing the deficit so
			// subsequent requests wait until average throughput recovers.
			if bucket.byteTokens < l.byteBurst {
				byteOK = false
				byteWait := (l.byteBurst - bucket.byteTokens) / l.byteRate
				if byteWait > waitSec {
					waitSec = byteWait
				}
			}
		}
	}

	if !reqOK || !byteOK {
		return Decision{
			Allowed:    false,
			RetryAfter: clampRetryAfterSeconds(waitSec, l.maxRetryAfter),
		}
	}

	bucket.reqTokens -= 1.0
	if l.byteRate > 0 && costBytes > 0 {
		bucket.byteTokens -= costBytes
	}
	return Decision{Allowed: true}
}

// RetryAfterSeconds converts a Retry-After duration to a positive integer
// number of seconds suitable for HTTP Retry-After headers.
func RetryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

func clampRetryAfterSeconds(waitSec float64, maxDelay time.Duration) time.Duration {
	maxSecs := max(int64(maxDelay/time.Second), 1)
	if math.IsNaN(waitSec) || math.IsInf(waitSec, 1) || waitSec >= float64(maxSecs) {
		return time.Duration(maxSecs) * time.Second
	}
	if waitSec <= 1 {
		return time.Second
	}
	return time.Duration(int64(math.Ceil(waitSec))) * time.Second
}

func (l *Limiter) lookupOrCreateLocked(projectID int, now time.Time) *projectBucket {
	if bucket, ok := l.buckets[projectID]; ok {
		bucket.lastSeen = now
		l.moveToFrontLocked(bucket)
		return bucket
	}

	l.evictExpiredTailLocked(now)
	for len(l.buckets) >= l.maxTrackedProjects && l.tail != nil {
		l.removeLocked(l.tail)
	}

	bucket := &projectBucket{
		projectID:  projectID,
		reqTokens:  l.burst,
		byteTokens: l.byteBurst,
		lastRefill: now,
		lastSeen:   now,
	}
	l.buckets[projectID] = bucket
	l.pushFrontLocked(bucket)
	return bucket
}

func (l *Limiter) refillLocked(bucket *projectBucket, now time.Time) {
	elapsed := now.Sub(bucket.lastRefill).Seconds()
	if elapsed <= 0 {
		if now.Before(bucket.lastRefill) {
			bucket.lastRefill = now
		}
		return
	}
	bucket.lastRefill = now
	bucket.reqTokens = math.Min(l.burst, bucket.reqTokens+elapsed*l.reqRate)
	if l.byteRate > 0 {
		bucket.byteTokens = math.Min(l.byteBurst, bucket.byteTokens+elapsed*l.byteRate)
	}
}

func (l *Limiter) evictExpiredTailLocked(now time.Time) {
	for l.tail != nil && now.Sub(l.tail.lastSeen) >= l.idleTTL {
		l.removeLocked(l.tail)
	}
}

func (l *Limiter) pushFrontLocked(b *projectBucket) {
	b.prev = nil
	b.next = l.head
	if l.head != nil {
		l.head.prev = b
	} else {
		l.tail = b
	}
	l.head = b
}

func (l *Limiter) moveToFrontLocked(b *projectBucket) {
	if l.head == b {
		return
	}
	l.unlinkLocked(b)
	l.pushFrontLocked(b)
}

func (l *Limiter) removeLocked(b *projectBucket) {
	delete(l.buckets, b.projectID)
	l.unlinkLocked(b)
}

func (l *Limiter) unlinkLocked(b *projectBucket) {
	if b.prev != nil {
		b.prev.next = b.next
	} else {
		l.head = b.next
	}
	if b.next != nil {
		b.next.prev = b.prev
	} else {
		l.tail = b.prev
	}
	b.prev = nil
	b.next = nil
}
