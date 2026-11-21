// Package janitor schedules retention cleanup tasks for platform-owned data.
package janitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	DefaultInterval    = time.Hour
	DefaultTaskTimeout = 5 * time.Minute
)

type Task interface {
	Clean(context.Context, time.Time) (int, error)
}

// Coordinator arbitrates cluster-wide janitor ownership (for example via a
// PostgreSQL advisory lock) so HA replicas do not run full retention sweeps
// simultaneously.
type Coordinator interface {
	TryAcquireJanitorLock(context.Context) (func() error, bool, error)
}

type registration struct {
	name      string
	retention time.Duration
	interval  time.Duration
	lastRun   time.Time
	task      Task
}

type Result struct {
	Name     string
	Deleted  int
	Duration time.Duration
	Err      error
}

type Janitor struct {
	logger       *slog.Logger
	interval     time.Duration
	taskTimeout  time.Duration
	initialDelay time.Duration
	coordinator  Coordinator
	now          func() time.Time
	tasks        []registration
}

func New(logger *slog.Logger, interval time.Duration) *Janitor {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Janitor{
		logger:      logger,
		interval:    interval,
		taskTimeout: DefaultTaskTimeout,
		now:         time.Now,
	}
}

func (j *Janitor) SetCoordinator(coordinator Coordinator) {
	j.coordinator = coordinator
}

func (j *Janitor) SetTaskTimeout(timeout time.Duration) {
	if timeout > 0 {
		j.taskTimeout = timeout
	}
}

func (j *Janitor) SetInitialDelay(delay time.Duration) {
	if delay >= 0 {
		j.initialDelay = delay
	}
}

func (j *Janitor) Register(name string, retention time.Duration, task Task) {
	j.RegisterWithInterval(name, retention, j.interval, task)
}

func (j *Janitor) RegisterWithInterval(name string, retention, interval time.Duration, task Task) {
	if name == "" || retention <= 0 || task == nil {
		return
	}
	if interval <= 0 {
		interval = j.interval
	}
	j.tasks = append(j.tasks, registration{name: name, retention: retention, interval: interval, task: task})
}

func (j *Janitor) RunOnce(ctx context.Context) []Result {
	return j.run(ctx, true)
}

func (j *Janitor) run(ctx context.Context, forceAll bool) []Result {
	now := j.now().UTC()
	dueIndices := make([]int, 0, len(j.tasks))
	for i := range j.tasks {
		if forceAll || j.tasks[i].lastRun.IsZero() || now.Sub(j.tasks[i].lastRun) >= j.tasks[i].interval {
			dueIndices = append(dueIndices, i)
		}
	}
	if len(dueIndices) == 0 {
		return nil
	}
	if j.coordinator != nil {
		release, acquired, err := j.coordinator.TryAcquireJanitorLock(ctx)
		if err != nil {
			j.logger.Warn("retention janitor lock acquisition failed", "error", err)
			return nil
		}
		if !acquired {
			j.logger.Debug("skipping retention cleanup; another replica holds the janitor lock")
			return nil
		}
		defer func() {
			if releaseErr := release(); releaseErr != nil {
				j.logger.Warn("release retention janitor lock failed", "error", releaseErr)
			}
		}()
	}

	results := make([]Result, 0, len(dueIndices))
	for _, idx := range dueIndices {
		if ctx.Err() != nil {
			break
		}
		registered := &j.tasks[idx]
		started := time.Now()
		taskCtx := ctx
		var cancel context.CancelFunc
		if j.taskTimeout > 0 {
			taskCtx, cancel = context.WithTimeout(ctx, j.taskTimeout)
		}
		deleted, err := runTaskSafely(taskCtx, registered.task, now.Add(-registered.retention))
		if cancel != nil {
			cancel()
		}
		registered.lastRun = now
		result := Result{Name: registered.name, Deleted: deleted, Duration: time.Since(started), Err: err}
		results = append(results, result)
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
			continue
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
	}
	return results
}

func runTaskSafely(ctx context.Context, task Task, cutoff time.Time) (deleted int, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("janitor task panic: %v", recovered)
		}
	}()
	return task.Clean(ctx, cutoff)
}

func (j *Janitor) tickInterval() time.Duration {
	minInterval := j.interval
	for _, task := range j.tasks {
		if task.interval > 0 && task.interval < minInterval {
			minInterval = task.interval
		}
	}
	if minInterval <= 0 {
		return DefaultInterval
	}
	return minInterval
}

// Run executes registered tasks after an optional startup delay and then on
// their configured schedule.
func (j *Janitor) Run(ctx context.Context) {
	if j.initialDelay > 0 {
		timer := time.NewTimer(j.initialDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	j.run(ctx, false)
	ticker := time.NewTicker(j.tickInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.run(ctx, false)
		}
	}
}
