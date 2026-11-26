package ingestlimit

import (
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWindowLimitRetryAfterAndReset(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	w := NewWindow(WindowConfig{Limit: 3, Window: time.Minute, Now: clock.Now})

	for i := range 3 {
		if d := w.Allow("k"); !d.Allowed {
			t.Fatalf("request %d rejected within limit", i)
		}
	}
	clock.Advance(20*time.Second + 300*time.Millisecond)
	d := w.Allow("k")
	if d.Allowed {
		t.Fatal("request over limit admitted")
	}
	if d.RetryAfter != 40*time.Second {
		t.Fatalf("RetryAfter=%s want 40s (ceil of remaining window)", d.RetryAfter)
	}
	if other := w.Allow("other"); !other.Allowed {
		t.Fatal("independent key must not share quota")
	}

	clock.Advance(39*time.Second + 500*time.Millisecond)
	if d := w.Allow("k"); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("near reset decision=%+v want rejected with 1s floor", d)
	}
	clock.Advance(200 * time.Millisecond)
	if d := w.Allow("k"); !d.Allowed {
		t.Fatal("request after window reset rejected")
	}
}

func TestWindowClockStepBackKeepsCounting(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	w := NewWindow(WindowConfig{Limit: 1, Window: time.Minute, Now: clock.Now})
	if !w.Allow("k").Allowed {
		t.Fatal("first request rejected")
	}
	clock.Advance(-10 * time.Minute)
	if d := w.Allow("k"); d.Allowed || d.RetryAfter != time.Minute {
		t.Fatalf("clock step back decision=%+v want rejected with 60s", d)
	}
}

func TestWindowDisabledAndNil(t *testing.T) {
	var nilWindow *Window
	if !nilWindow.Allow("k").Allowed || nilWindow.Enabled() || nilWindow.TrackedKeys() != 0 {
		t.Fatal("nil window must admit everything")
	}
	w := NewWindow(WindowConfig{Limit: 0})
	for range 10 {
		if !w.Allow("k").Allowed {
			t.Fatal("disabled window rejected request")
		}
	}
	if w.TrackedKeys() != 0 {
		t.Fatalf("disabled window tracked %d keys", w.TrackedKeys())
	}
}

func TestWindowBoundsHighCardinalityKeys(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	w := NewWindow(WindowConfig{Limit: 1, Window: time.Minute, MaxKeys: 64, Now: clock.Now})

	if !w.Allow("victim").Allowed {
		t.Fatal("first victim request rejected")
	}
	for i := range 10_000 {
		clock.Advance(time.Millisecond)
		w.Allow("ip-" + strconv.Itoa(i))
		if n := w.TrackedKeys(); n > 64 {
			t.Fatalf("tracked keys=%d exceeds bound 64", n)
		}
	}
	if n := w.TrackedKeys(); n != 64 {
		t.Fatalf("tracked keys=%d want 64 after flood", n)
	}
	// The oldest window (victim) was evicted first; recent keys stay limited.
	if !w.Allow("victim").Allowed {
		t.Fatal("evicted victim key should start a fresh window")
	}
	if w.Allow("ip-9999").Allowed {
		t.Fatal("most recent key lost its counter under eviction")
	}
}

func TestWindowSweepsExpiredEntries(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	w := NewWindow(WindowConfig{Limit: 5, Window: time.Minute, MaxKeys: 1024, Now: clock.Now})
	for i := range 100 {
		w.Allow("s-" + strconv.Itoa(i))
	}
	clock.Advance(time.Minute)
	w.Allow("fresh")
	if n := w.TrackedKeys(); n != 1 {
		t.Fatalf("tracked keys=%d want 1 after expired sweep", n)
	}
}

func TestWindowConcurrentAdmissionIsExact(t *testing.T) {
	w := NewWindow(WindowConfig{Limit: 100, Window: time.Hour})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				if w.Allow("shared").Allowed {
					admitted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if got := admitted.Load(); got != 100 {
		t.Fatalf("admitted=%d want exactly 100", got)
	}
}

func TestRetryAfterWindow(t *testing.T) {
	maxTruncated := time.Duration(math.MaxInt64).Truncate(time.Second)
	cases := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{name: "min_int64", remaining: math.MinInt64, want: time.Second},
		{name: "negative", remaining: -5 * time.Second, want: time.Second},
		{name: "zero", remaining: 0, want: time.Second},
		{name: "one_ns", remaining: time.Nanosecond, want: time.Second},
		{name: "half_second", remaining: 500 * time.Millisecond, want: time.Second},
		{name: "just_under_1s", remaining: time.Second - 1, want: time.Second},
		{name: "exact_1s", remaining: time.Second, want: time.Second},
		{name: "just_over_1s", remaining: time.Second + 1, want: 2 * time.Second},
		{name: "fractional_39_7s", remaining: 39*time.Second + 700*time.Millisecond, want: 40 * time.Second},
		{name: "exact_40s", remaining: 40 * time.Second, want: 40 * time.Second},
		{name: "just_under_max_whole_sec", remaining: maxTruncated - 1, want: maxTruncated},
		{name: "exact_max_whole_sec", remaining: maxTruncated, want: maxTruncated},
		{name: "max_int64_saturates", remaining: math.MaxInt64, want: maxTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterWindow(tc.remaining); got != tc.want {
				t.Fatalf("retryAfterWindow(%s)=%s want %s", tc.remaining, got, tc.want)
			}
		})
	}
}
