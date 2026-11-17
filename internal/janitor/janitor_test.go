package janitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
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
