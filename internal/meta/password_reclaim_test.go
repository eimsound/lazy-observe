package meta

import (
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTimerController struct {
	mu      sync.Mutex
	entries []fakeTimerEntry
}

type fakeTimerEntry struct {
	delay time.Duration
	fn    func()
	timer *time.Timer
	fired bool
}

func (f *fakeTimerController) afterFunc(d time.Duration, fn func()) *time.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := time.NewTimer(time.Hour)
	f.entries = append(f.entries, fakeTimerEntry{delay: d, fn: fn, timer: t})
	return t
}

func (f *fakeTimerController) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		if !e.fired {
			n++
		}
	}
	return n
}

func (f *fakeTimerController) fireNext() (time.Duration, bool) {
	f.mu.Lock()
	for i := range f.entries {
		if f.entries[i].fired {
			continue
		}
		f.entries[i].fired = true
		stopped := !f.entries[i].timer.Stop()
		fn := f.entries[i].fn
		delay := f.entries[i].delay
		f.mu.Unlock()
		if stopped {
			return 0, false
		}
		fn()
		return delay, true
	}
	f.mu.Unlock()
	return 0, false
}

func TestArgonMemoryReclaimerImmediateAndCooldownCoalescing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ctrl := &fakeTimerController{}
	var reclaims int
	r := newArgonMemoryReclaimer(
		250*time.Millisecond,
		func() time.Time { return now },
		func() { reclaims++ },
		ctrl.afterFunc,
	)
	t.Cleanup(r.stop)

	// First completion triggers immediate reclaim.
	if !r.enter(argonMemory) {
		t.Fatal("expected 64 MiB Argon2id call to be tracked")
	}
	r.leave()
	if reclaims != 1 {
		t.Fatalf("reclaims after first completion = %d, want 1", reclaims)
	}
	if ctrl.pendingCount() != 0 {
		t.Fatalf("pending timers after immediate reclaim = %d, want 0", ctrl.pendingCount())
	}

	// Multiple completions within cooldown coalesce into one trailing timer.
	now = now.Add(50 * time.Millisecond)
	for range 3 {
		if !r.enter(argonMemory) {
			t.Fatal("expected tracked call")
		}
		r.leave()
	}
	if reclaims != 1 {
		t.Fatalf("reclaims during cooldown = %d, want 1", reclaims)
	}
	if ctrl.pendingCount() != 1 {
		t.Fatalf("pending timers during cooldown = %d, want 1", ctrl.pendingCount())
	}

	now = now.Add(200 * time.Millisecond)
	delay, fired := ctrl.fireNext()
	if !fired || delay != 200*time.Millisecond {
		t.Fatalf("trailing timer fired=%v delay=%v, want fired=true delay=200ms", fired, delay)
	}
	if reclaims != 2 {
		t.Fatalf("reclaims after trailing timer = %d, want 2", reclaims)
	}

	// After cooldown elapses, the next completion reclaims immediately again.
	now = now.Add(300 * time.Millisecond)
	r.enter(argonMemory)
	r.leave()
	if reclaims != 3 {
		t.Fatalf("reclaims after cooldown elapsed = %d, want 3", reclaims)
	}
}

func TestArgonMemoryReclaimerDefersWhileInFlight(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ctrl := &fakeTimerController{}
	var reclaims int
	r := newArgonMemoryReclaimer(
		250*time.Millisecond,
		func() time.Time { return now },
		func() { reclaims++ },
		ctrl.afterFunc,
	)
	t.Cleanup(r.stop)

	r.enter(argonMemory)
	r.enter(argonMemory)
	r.leave()
	if reclaims != 0 {
		t.Fatalf("reclaims while second call still in flight = %d, want 0", reclaims)
	}
	r.leave()
	if reclaims != 1 {
		t.Fatalf("reclaims after all in-flight calls finish = %d, want 1", reclaims)
	}

	// Schedule a cooldown timer, then start an in-flight call before it fires.
	now = now.Add(50 * time.Millisecond)
	r.enter(argonMemory)
	r.leave()
	if ctrl.pendingCount() != 1 {
		t.Fatalf("pending timers = %d, want 1", ctrl.pendingCount())
	}

	r.enter(argonMemory)
	now = now.Add(200 * time.Millisecond)
	if _, fired := ctrl.fireNext(); !fired {
		t.Fatal("expected timer callback to run")
	}
	if reclaims != 1 {
		t.Fatalf("reclaims while new call in flight = %d, want 1", reclaims)
	}

	// Finishing the in-flight call sees elapsed >= minInterval and reclaims at once.
	r.leave()
	if reclaims != 2 {
		t.Fatalf("reclaims after deferred in-flight call completes = %d, want 2", reclaims)
	}
}

func TestArgonMemoryReclaimerPendingWhileRunning(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ctrl := &fakeTimerController{}
	var reclaims int
	started := make(chan struct{})
	releaseReclaim := make(chan struct{})
	var blockOnce sync.Once

	r := newArgonMemoryReclaimer(
		250*time.Millisecond,
		func() time.Time { return now },
		func() {
			reclaims++
			blockOnce.Do(func() {
				close(started)
				<-releaseReclaim
			})
		},
		ctrl.afterFunc,
	)
	t.Cleanup(r.stop)

	r.enter(argonMemory)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.leave()
	}()

	<-started
	// Complete another Argon2id call while reclaimFn is running.
	r.enter(argonMemory)
	r.leave()
	close(releaseReclaim)
	<-done

	if reclaims != 1 {
		t.Fatalf("reclaims right after first run = %d, want 1", reclaims)
	}
	if ctrl.pendingCount() != 1 {
		t.Fatalf("pending timers after overlapping leave = %d, want 1", ctrl.pendingCount())
	}
	now = now.Add(250 * time.Millisecond)
	if _, fired := ctrl.fireNext(); !fired {
		t.Fatal("expected trailing timer to fire for pending run")
	}
	if reclaims != 2 {
		t.Fatalf("reclaims after trailing run = %d, want 2", reclaims)
	}
}

func TestArgonMemoryReclaimerSkipsSmallMemory(t *testing.T) {
	ctrl := &fakeTimerController{}
	var reclaims atomic.Int32
	r := newArgonMemoryReclaimer(
		250*time.Millisecond,
		time.Now,
		func() { reclaims.Add(1) },
		ctrl.afterFunc,
	)
	t.Cleanup(r.stop)

	if r.enter(argonReclaimMinMemoryKB - 1) {
		t.Fatal("expected small Argon2id memory parameter not to be tracked")
	}
	if reclaims.Load() != 0 {
		t.Fatalf("unexpected reclaim for small memory: %d", reclaims.Load())
	}
}

func TestArgon2IDPasswordReclaimsIdleHeap(t *testing.T) {
	// Start from a clean OS memory baseline in case earlier package tests
	// left unreleased spans behind.
	debug.FreeOSMemory()

	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct-horse-battery-staple") {
		t.Fatal("VerifyPassword rejected valid password")
	}

	// Wait slightly past argonReclaimMinInterval so any coalesced trailing timer
	// has finished returning the second 64 MiB temporary slice to the OS.
	deadline := time.Now().Add(2 * time.Second)
	var unreleased uint64
	for {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		unreleased = stats.HeapIdle - stats.HeapReleased
		if unreleased < 32*1024*1024 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if unreleased >= 32*1024*1024 {
		t.Fatalf("unreleased idle heap after Argon2id = %d bytes, want < 32 MiB", unreleased)
	}
}
