package ingestlimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestLimiterDisabledAlwaysAllows(t *testing.T) {
	l := New(Config{
		Enabled:           false,
		RequestsPerSecond: 1,
		Burst:             1,
		BytesPerSecond:    1,
	})
	if l.Enabled() || l.ByteLimitEnabled() {
		t.Fatal("disabled limiter must report Enabled()=false and ByteLimitEnabled()=false")
	}
	for i := range 20 {
		d := l.Allow(1, 1024*1024)
		if !d.Allowed || d.RetryAfter != 0 {
			t.Fatalf("call %d unexpectedly rejected: %+v", i, d)
		}
	}
	if l.TrackedProjects() != 0 {
		t.Fatalf("TrackedProjects()=%d want 0", l.TrackedProjects())
	}
}

func TestLimiterRequestBurstRefillAndRetryAfter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := New(Config{
		Enabled:           true,
		RequestsPerSecond: 2,
		Burst:             3,
		Now:               clock.Now,
	})

	for i := range 3 {
		if d := l.Allow(10, 0); !d.Allowed {
			t.Fatalf("burst request %d rejected: %+v", i, d)
		}
	}

	// Fourth request immediately exceeds burst: deficit is 1 token at 2 req/s = 0.5s -> ceil = 1s.
	d := l.Allow(10, 0)
	if d.Allowed {
		t.Fatal("expected rejection after burst exhausted")
	}
	if d.RetryAfter != time.Second {
		t.Fatalf("RetryAfter=%s want 1s", d.RetryAfter)
	}

	// Advance 250ms: only 0.5 tokens refilled, still rejected.
	clock.Advance(250 * time.Millisecond)
	if d := l.Allow(10, 0); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("at +250ms: got %+v want rejected 1s", d)
	}

	// Advance another 250ms (total 500ms): 1 token refilled -> allowed.
	clock.Advance(250 * time.Millisecond)
	if d := l.Allow(10, 0); !d.Allowed {
		t.Fatalf("at +500ms: expected allowed, got %+v", d)
	}

	// Slow rate computes multi-second Retry-After and respects MaxRetryAfter cap.
	slow := New(Config{
		Enabled:           true,
		RequestsPerSecond: 0.01, // 100s per token
		Burst:             1,
		MaxRetryAfter:     30 * time.Second,
		Now:               clock.Now,
	})
	if !slow.Allow(1, 0).Allowed {
		t.Fatal("first slow request must pass")
	}
	rejected := slow.Allow(1, 0)
	if rejected.Allowed || rejected.RetryAfter != 30*time.Second {
		t.Fatalf("capped rejection=%+v want RetryAfter=30s", rejected)
	}
}

func TestLimiterProjectIsolation(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := New(Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             2,
		Now:               clock.Now,
	})

	// Exhaust project 1.
	for range 2 {
		if !l.Allow(1, 0).Allowed {
			t.Fatal("project 1 initial burst must pass")
		}
	}
	if l.Allow(1, 0).Allowed {
		t.Fatal("project 1 must be rate limited")
	}

	// Project 2 must be completely unaffected.
	for i := range 2 {
		if !l.Allow(2, 0).Allowed {
			t.Fatalf("project 2 request %d must not be affected by project 1", i)
		}
	}
}

func TestLimiterByteRateAndAtomicDeduction(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := New(Config{
		Enabled:           true,
		RequestsPerSecond: 100,
		Burst:             10,
		BytesPerSecond:    1000,
		ByteBurst:         1000,
		Now:               clock.Now,
	})

	if !l.ByteLimitEnabled() {
		t.Fatal("expected ByteLimitEnabled()=true")
	}

	// Consume 600 bytes (leaves 400 bytes, 9 request tokens).
	if d := l.Allow(7, 600); !d.Allowed {
		t.Fatalf("first 600B request failed: %+v", d)
	}

	// 800B request needs 400B more -> 0.4s -> ceil 1s; must NOT deduct request token!
	if d := l.Allow(7, 800); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("800B request=%+v want rejected 1s", d)
	}

	// Small 400B request must still succeed immediately because the rejected request did not drain tokens.
	if d := l.Allow(7, 400); !d.Allowed {
		t.Fatalf("400B request after byte rejection failed: %+v", d)
	}

	// Refill completely and test a single payload larger than byteBurst.
	clock.Advance(2 * time.Second)
	if d := l.Allow(7, 2500); !d.Allowed {
		t.Fatalf("oversized single payload on full bucket must pass: %+v", d)
	}
	// Bucket is now in debt (-1500B), so immediate next byte request is rejected with Retry-After >= 2s.
	d := l.Allow(7, 500)
	if d.Allowed || d.RetryAfter < 2*time.Second {
		t.Fatalf("post-debt request=%+v want rejected >= 2s", d)
	}
}

func TestLimiterBoundedTrackedProjectsAndIdleEviction(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := New(Config{
		Enabled:            true,
		RequestsPerSecond:  10,
		Burst:              10,
		MaxTrackedProjects: 4,
		IdleTTL:            5 * time.Minute,
		Now:                clock.Now,
	})

	for id := 1; id <= 20; id++ {
		if !l.Allow(id, 0).Allowed {
			t.Fatalf("project %d unexpectedly rejected", id)
		}
		if got := l.TrackedProjects(); got > 4 {
			t.Fatalf("TrackedProjects()=%d exceeded max 4", got)
		}
	}
	if got := l.TrackedProjects(); got != 4 {
		t.Fatalf("TrackedProjects()=%d want 4", got)
	}

	// Advance past IdleTTL and insert a new project: expired entries are evicted.
	clock.Advance(6 * time.Minute)
	if !l.Allow(999, 0).Allowed {
		t.Fatal("project 999 rejected")
	}
	if got := l.TrackedProjects(); got != 1 {
		t.Fatalf("TrackedProjects()=%d want 1 after idle eviction", got)
	}
}

func TestLimiterClockRollbackDoesNotBreak(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC))
	l := New(Config{
		Enabled:           true,
		RequestsPerSecond: 1,
		Burst:             2,
		Now:               clock.Now,
	})

	if !l.Allow(1, 0).Allowed {
		t.Fatal("first request must pass")
	}
	// Simulate NTP step backwards.
	clock.Advance(-5 * time.Minute)
	if !l.Allow(1, 0).Allowed {
		t.Fatal("second burst token must still be available after clock step back")
	}
	// Advance forward 1s from rolled-back clock -> refills 1 token.
	clock.Advance(time.Second)
	if !l.Allow(1, 0).Allowed {
		t.Fatal("token must refill normally after clock resumes advancing")
	}
}

func TestLimiterConcurrentRaceSafety(t *testing.T) {
	l := New(Config{
		Enabled:            true,
		RequestsPerSecond:  1000,
		Burst:              50,
		BytesPerSecond:     1 << 20,
		MaxTrackedProjects: 8,
	})

	var wg sync.WaitGroup
	var allowed atomic.Int64
	var rejected atomic.Int64

	for g := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 200 {
				projectID := (worker+i)%12 + 1
				d := l.Allow(projectID, 512)
				if d.Allowed {
					allowed.Add(1)
				} else {
					if d.RetryAfter < time.Second {
						t.Errorf("invalid RetryAfter %s", d.RetryAfter)
					}
					rejected.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()

	if allowed.Load() == 0 {
		t.Fatal("expected some requests to be allowed")
	}
	if l.TrackedProjects() > 8 {
		t.Fatalf("TrackedProjects()=%d exceeded max 8", l.TrackedProjects())
	}
}
