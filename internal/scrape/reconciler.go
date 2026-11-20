package scrape

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"time"
)

const (
	DefaultLeaderSyncInterval = 5 * time.Second
	DefaultReconcileRetryBase = time.Second
	DefaultReconcileRetryMax  = 30 * time.Second
)

// SyncStatus is a read-only snapshot of the scrape reconciler's closed-loop
// synchronization state with vmagent.
type SyncStatus struct {
	Leader          bool       `json:"leader"`
	DesiredRevision uint64     `json:"desired_revision"`
	AppliedRevision uint64     `json:"applied_revision"`
	InSync          bool       `json:"in_sync"`
	LastSyncAt      *time.Time `json:"last_sync_at,omitempty"`
	LastSyncError   string     `json:"last_sync_error,omitempty"`
}

// Status returns a copy of the reconciler's current synchronization state.
func (s *Service) Status() SyncStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	var lastSync *time.Time
	if s.lastSyncAt != nil {
		ts := *s.lastSyncAt
		lastSync = &ts
	}
	return SyncStatus{
		Leader:          s.isLeader,
		DesiredRevision: s.desiredRevision,
		AppliedRevision: s.appliedRevision,
		InSync:          s.appliedRevision >= s.desiredRevision && s.lastSyncError == "",
		LastSyncAt:      lastSync,
		LastSyncError:   s.lastSyncError,
	}
}

func (s *Service) IsLeader() bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.isLeader
}

func (s *Service) setLeader(leader bool) {
	s.statusMu.Lock()
	s.isLeader = leader
	s.statusMu.Unlock()
}

func (s *Service) bumpDesiredRevision() uint64 {
	s.statusMu.Lock()
	s.desiredRevision++
	rev := s.desiredRevision
	s.statusMu.Unlock()
	return rev
}

func (s *Service) desiredRevisionSnapshot() uint64 {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.desiredRevision
}

func (s *Service) needsReconcile() bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.appliedRevision < s.desiredRevision || s.lastSyncError != ""
}

func (s *Service) recordSyncSuccess(appliedRev uint64) {
	now := s.now().UTC()
	s.statusMu.Lock()
	if appliedRev > s.appliedRevision {
		s.appliedRevision = appliedRev
	}
	s.lastSyncAt = &now
	s.lastSyncError = ""
	s.statusMu.Unlock()
}

func (s *Service) recordSyncError(err error) {
	if err == nil {
		return
	}
	s.statusMu.Lock()
	s.lastSyncError = err.Error()
	s.statusMu.Unlock()
}

func (s *Service) signalWake() {
	if s.wakeCh == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// NotifyChange advances desiredRevision and issues a non-blocking, coalescing
// wakeup to the background reconciler.
func (s *Service) NotifyChange() uint64 {
	rev := s.bumpDesiredRevision()
	s.signalWake()
	return rev
}

// NotifyProjectDeleted signals that a project (and any cascaded scrape targets)
// was deleted, performs an immediate best-effort sync when active, and leaves
// pending retry state for the background reconciler if vmagent reload fails.
func (s *Service) NotifyProjectDeleted(ctx context.Context) error {
	s.bumpDesiredRevision()
	err := s.Sync(ctx)
	if err != nil {
		s.signalWake()
	}
	return err
}

// Sync renders the current database target snapshot, writes the config file
// atomically, and confirms reload via the configured ReloadFunc (/-/reload).
func (s *Service) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLocked(ctx)
}

func (s *Service) syncLocked(ctx context.Context) error {
	if s.store != nil && s.store.IsClustered() && !s.IsLeader() {
		// In HA mode, only the replica holding the scrape leader lock renders
		// targets to vmagent. The leader's reconciler picks up DB changes
		// written by non-leader replicas automatically.
		return nil
	}
	return s.syncActiveLocked(ctx, true)
}

func (s *Service) syncActiveLocked(ctx context.Context, forceReload bool) error {
	// Capture desiredRevision before loading DB state so any mutation arriving
	// concurrently while we write/reload keeps appliedRevision < desiredRevision
	// and triggers a follow-up reconciliation pass.
	targetRev := s.desiredRevisionSnapshot()
	data, err := renderConfig(ctx, s.store, s.logger)
	if err != nil {
		s.recordSyncError(err)
		return err
	}
	sum := sha256.Sum256(data)
	if !forceReload && s.hasHash && s.lastHash == sum {
		s.recordSyncSuccess(targetRev)
		return nil
	}
	if err := writeAtomicFile(s.configPath, data); err != nil {
		s.recordSyncError(err)
		return err
	}
	if s.reload != nil {
		if err := s.reload(); err != nil {
			s.recordSyncError(err)
			return err
		}
	}
	s.lastHash = sum
	s.hasHash = true
	s.recordSyncSuccess(targetRev)
	return nil
}

func (s *Service) syncStandbyLocked() error {
	data := []byte(emptyScrapeConfigYAML)
	sum := sha256.Sum256(data)
	if s.hasHash && s.lastHash == sum {
		return nil
	}
	if err := writeAtomicFile(s.configPath, data); err != nil {
		s.recordSyncError(err)
		return err
	}
	if s.reload != nil {
		if err := s.reload(); err != nil {
			s.recordSyncError(err)
			return err
		}
	}
	s.lastHash = sum
	s.hasHash = true
	return nil
}

// RunLeaderLoop coordinates single-owner scraping across HA PostgreSQL replicas
// and periodically reconciles target changes written through other replicas.
// In single-node (non-clustered) mode it returns immediately for backwards
// compatibility; use RunReconciler to run event-driven reconciliation in all
// deployment modes.
func (s *Service) RunLeaderLoop(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if s.store == nil || !s.store.IsClustered() {
		return
	}
	s.RunReconciler(ctx, logger, interval)
}

// RunReconciler runs the event-driven scrape reconciler with exponential
// backoff retry on failure and HA leader election when backed by a clustered
// PostgreSQL store.
func (s *Service) RunReconciler(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if logger == nil {
		logger = s.logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultLeaderSyncInterval
	}
	clustered := s.store != nil && s.store.IsClustered()

	var release func() error
	releaseLeader := func() {
		if !clustered {
			return
		}
		s.setLeader(false)
		if release != nil {
			if err := release(); err != nil {
				logger.Warn("release scrape leader lock failed", "error", err)
			}
			release = nil
		}
	}
	defer releaseLeader()

	var (
		consecutiveFailures int
		retryTimer          *time.Timer
		retryCh             <-chan time.Time
	)
	stopRetry := func() {
		if retryTimer != nil {
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			retryTimer = nil
			retryCh = nil
		}
	}
	defer stopRetry()

	scheduleRetry := func() {
		consecutiveFailures++
		delay := s.retryBackoff(consecutiveFailures)
		stopRetry()
		retryTimer = time.NewTimer(delay)
		retryCh = retryTimer.C
	}

	reconcileActive := func(forceReload bool) {
		s.mu.Lock()
		err := s.syncActiveLocked(ctx, forceReload)
		s.mu.Unlock()
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn(
					"reconcile scrape config failed",
					"attempt",
					consecutiveFailures+1,
					"error",
					err,
				)
				scheduleRetry()
			}
			return
		}
		consecutiveFailures = 0
		stopRetry()
		if s.needsReconcile() {
			s.signalWake()
		}
	}

	stepClustered := func() {
		if !s.IsLeader() {
			rel, acquired, err := s.store.TryAcquireScrapeLeader(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("acquire scrape leader lock failed", "error", err)
					scheduleRetry()
				}
				return
			}
			if !acquired {
				s.mu.Lock()
				err = s.syncStandbyLocked()
				s.mu.Unlock()
				if err != nil {
					logger.Warn("clear standby scrape config failed", "error", err)
					scheduleRetry()
					return
				}
				consecutiveFailures = 0
				stopRetry()
				return
			}
			release = rel
			s.setLeader(true)
			logger.Info("acquired HA scrape leader lock")
			reconcileActive(true)
			return
		}
		reconcileActive(false)
	}

	if clustered {
		stepClustered()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wakeCh:
				if s.IsLeader() {
					reconcileActive(false)
				} else {
					stepClustered()
				}
			case <-retryCh:
				retryTimer = nil
				retryCh = nil
				stepClustered()
			case <-ticker.C:
				stepClustered()
			}
		}
	}

	// Single-node mode: event-driven wakeup with exponential backoff on failure.
	if s.needsReconcile() {
		reconcileActive(false)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wakeCh:
			reconcileActive(false)
		case <-retryCh:
			retryTimer = nil
			retryCh = nil
			reconcileActive(false)
		}
	}
}

func (s *Service) retryBackoff(attempt int) time.Duration {
	base := s.retryBaseDelay
	if base <= 0 {
		base = DefaultReconcileRetryBase
	}
	maxDelay := s.retryMaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultReconcileRetryMax
	}
	if maxDelay < base {
		maxDelay = base
	}
	if attempt <= 1 {
		return base
	}
	shift := min(attempt-1, 10)
	delay := base << shift
	if delay <= 0 || delay > maxDelay {
		return maxDelay
	}
	return delay
}
