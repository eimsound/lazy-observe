// Package janitor schedules retention cleanup tasks for platform-owned data.
package janitor

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const DefaultInterval = time.Hour

type Task interface {
	Clean(context.Context, time.Time) (int, error)
}

type registration struct {
	name      string
	retention time.Duration
	task      Task
}

type Result struct {
	Name     string
	Deleted  int
	Duration time.Duration
	Err      error
}

type Janitor struct {
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time
	tasks    []registration
}

func New(logger *slog.Logger, interval time.Duration) *Janitor {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Janitor{logger: logger, interval: interval, now: time.Now}
}

func (j *Janitor) Register(name string, retention time.Duration, task Task) {
	if name == "" || retention <= 0 || task == nil {
		return
	}
	j.tasks = append(j.tasks, registration{name: name, retention: retention, task: task})
}

func (j *Janitor) RunOnce(ctx context.Context) []Result {
	results := make([]Result, len(j.tasks))
	now := j.now().UTC()
	var wg sync.WaitGroup
	for i, registered := range j.tasks {
		wg.Add(1)
		go func(i int, registered registration) {
			defer wg.Done()
			started := time.Now()
			deleted, err := registered.task.Clean(ctx, now.Add(-registered.retention))
			result := Result{Name: registered.name, Deleted: deleted, Duration: time.Since(started), Err: err}
			results[i] = result
			if err != nil {
				j.logger.Error(
					"retention cleanup failed",
					"task",
					result.Name,
					"deleted",
					result.Deleted,
					"duration",
					result.Duration,
					"error",
					err,
				)
				return
			}
			j.logger.Info(
				"retention cleanup completed",
				"task",
				result.Name,
				"deleted",
				result.Deleted,
				"duration",
				result.Duration,
			)
		}(i, registered)
	}
	wg.Wait()
	return results
}

// Run executes all registered tasks on startup and then on a shared interval.
func (j *Janitor) Run(ctx context.Context) {
	j.RunOnce(ctx)
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.RunOnce(ctx)
		}
	}
}
