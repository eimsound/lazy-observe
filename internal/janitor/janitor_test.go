package janitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingTask struct {
	mu      sync.Mutex
	cutoffs []time.Time
	deleted int
	err     error
}

func (t *recordingTask) Clean(_ context.Context, cutoff time.Time) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cutoffs = append(t.cutoffs, cutoff)
	return t.deleted, t.err
}

func (t *recordingTask) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.cutoffs)
}

func TestJanitorRegistrationCutoffAndFailureIsolation(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	failed := &recordingTask{err: errors.New("boom")}
	healthy := &recordingTask{deleted: 2}
	j := New(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	j.now = func() time.Time { return now }
	j.Register("failed", 24*time.Hour, failed)
	j.Register("healthy", 48*time.Hour, healthy)
	j.Register("ignored", 0, healthy)

	results := j.RunOnce(context.Background())
	if len(results) != 2 {
		t.Fatalf("results=%d, want 2", len(results))
	}
	if results[0].Err == nil || results[1].Err != nil || results[1].Deleted != 2 {
		t.Fatalf("failure isolation results: %#v", results)
	}
	if got := failed.cutoffs[0]; !got.Equal(now.Add(-24 * time.Hour)) {
		t.Fatalf("failed cutoff=%s", got)
	}
	if got := healthy.cutoffs[0]; !got.Equal(now.Add(-48 * time.Hour)) {
		t.Fatalf("healthy cutoff=%s", got)
	}
}

func TestJanitorRunsOnStartupAndInterval(t *testing.T) {
	task := &recordingTask{}
	j := New(slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond)
	j.Register("task", time.Hour, task)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()
	deadline := time.After(time.Second)
	for task.calls() < 2 {
		select {
		case <-deadline:
			t.Fatal("janitor did not run on startup and interval")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

type fakeCoordinator struct {
	acquired bool
	releases atomic.Int32
}

func (c *fakeCoordinator) TryAcquireJanitorLock(context.Context) (func() error, bool, error) {
	if !c.acquired {
		return nil, false, nil
	}
	return func() error {
		c.releases.Add(1)
		return nil
	}, true, nil
}

type concurrencyCheckTask struct {
	active    *atomic.Int32
	maxActive *atomic.Int32
	calls     atomic.Int32
}

func (t *concurrencyCheckTask) Clean(context.Context, time.Time) (int, error) {
	t.calls.Add(1)
	cur := t.active.Add(1)
	for {
		prev := t.maxActive.Load()
		if cur <= prev || t.maxActive.CompareAndSwap(prev, cur) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	t.active.Add(-1)
	return 1, nil
}

func TestJanitorCoordinatorLeadershipSequentialExecutionAndPerTaskInterval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	j := New(logger, time.Hour)

	// When coordinator reports lock held by another replica, skip all tasks.
	coord := &fakeCoordinator{acquired: false}
	j.SetCoordinator(coord)
	var active, maxActive atomic.Int32
	t1 := &concurrencyCheckTask{active: &active, maxActive: &maxActive}
	t2 := &concurrencyCheckTask{active: &active, maxActive: &maxActive}
	j.Register("t1", time.Hour, t1)
	j.Register("t2", time.Hour, t2)
	if results := j.RunOnce(context.Background()); len(results) != 0 {
		t.Fatalf("expected skipped cycle when lock unavailable, got %+v", results)
	}
	if t1.calls.Load() != 0 || t2.calls.Load() != 0 {
		t.Fatalf("tasks ran without lock: t1=%d t2=%d", t1.calls.Load(), t2.calls.Load())
	}

	// When lock is acquired, tasks run sequentially and lock is released.
	coord.acquired = true
	results := j.RunOnce(context.Background())
	if len(results) != 2 || coord.releases.Load() != 1 {
		t.Fatalf("results=%d releases=%d", len(results), coord.releases.Load())
	}
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent tasks=%d want 1 (sequential)", maxActive.Load())
	}

	// Per-task interval allows replay cleanup to run more frequently than other tasks.
	fast := &recordingTask{}
	slow := &recordingTask{}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	j2 := New(logger, time.Hour)
	j2.now = func() time.Time { return now }
	j2.RegisterWithInterval("fast", time.Hour, 5*time.Minute, fast)
	j2.Register("slow", time.Hour, slow)
	j2.run(context.Background(), false)
	now = now.Add(10 * time.Minute)
	j2.run(context.Background(), false)
	if fast.calls() != 2 || slow.calls() != 1 {
		t.Fatalf("per-task interval calls: fast=%d slow=%d want 2,1", fast.calls(), slow.calls())
	}
}

type blockingTask struct {
	observedErr error
}

func (b *blockingTask) Clean(ctx context.Context, _ time.Time) (int, error) {
	<-ctx.Done()
	b.observedErr = ctx.Err()
	return 0, ctx.Err()
}

type panickingTask struct{}

func (panickingTask) Clean(context.Context, time.Time) (int, error) {
	panic("simulated janitor panic")
}

func TestJanitorPerTaskTimeoutAndPanicRecovery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	coord := &fakeCoordinator{acquired: true}
	j := New(logger, time.Hour)
	j.SetCoordinator(coord)
	j.SetTaskTimeout(30 * time.Millisecond)

	slow := &blockingTask{}
	after := &recordingTask{deleted: 4}
	j.Register("slow", time.Hour, slow)
	j.Register("panicky", time.Hour, panickingTask{})
	j.Register("after", time.Hour, after)

	results := j.RunOnce(context.Background())
	if len(results) != 3 {
		t.Fatalf("results=%d want 3: %+v", len(results), results)
	}
	if !errors.Is(results[0].Err, context.DeadlineExceeded) || !errors.Is(slow.observedErr, context.DeadlineExceeded) {
		t.Fatalf("slow task timeout err=%v observed=%v want DeadlineExceeded", results[0].Err, slow.observedErr)
	}
	if results[1].Err == nil || !errors.Is(results[1].Err, results[1].Err) ||
		results[1].Err.Error() != "janitor task panic: simulated janitor panic" {
		t.Fatalf("panicking task err=%v", results[1].Err)
	}
	if results[2].Err != nil || results[2].Deleted != 4 {
		t.Fatalf("subsequent task did not complete cleanly: %+v", results[2])
	}
	if coord.releases.Load() != 1 {
		t.Fatalf("coordinator lock releases=%d want 1", coord.releases.Load())
	}
}
