package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"entgo.io/ent/dialect"
)

const (
	platformAlertKindErrorRateSpike = "error_rate_spike"
	platformErrorRateBucketSeconds  = 10
)

type PlatformErrorSpikeTransition struct {
	Transition string // "", "firing", "resolved"
	Cycle      int64
	StartsAt   time.Time
	EndsAt     time.Time
	Count      int
}

type PlatformErrorSpikeRecovery struct {
	ProjectID   int
	ProjectSlug string
	Cycle       int64
	StartsAt    time.Time
	EndsAt      time.Time
	Count       int
	Threshold   int
}

// PlatformAlertState is the durable state snapshot of a project's platform alert.
type PlatformAlertState struct {
	ProjectID   int
	ProjectSlug string
	Kind        string
	Status      string
	ActiveCycle int64
	StartsAt    time.Time
	EndsAt      *time.Time
	UpdatedAt   time.Time
}

const platformAlertStateColumns = `st.project_id, p.slug, st.kind, st.status, st.active_cycle, st.starts_at, st.ends_at, st.updated_at`

// ProjectFiringPlatformAlertStates lists active firing durable platform alerts
// for projectID so native and platform policy planners share a single view of
// active inhibition sources and storm groups across replicas.
func (s *Store) ProjectFiringPlatformAlertStates(ctx context.Context, projectID int) ([]PlatformAlertState, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(
			s.dialect,
			`SELECT `+platformAlertStateColumns+`
			 FROM platform_alert_states AS st
			 JOIN projects AS p ON p.id = st.project_id
			 WHERE st.project_id = ? AND st.status = 'firing'
			 ORDER BY st.kind`,
		),
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list firing platform alert states: %w", err)
	}
	return scanPlatformAlertStates(rows)
}

// RecentPlatformAlertStates lists every firing platform alert state of kind
// plus resolved ones whose ends_at is at or after resolvedSince. When
// projectID > 0, results are filtered to that project in SQL. Platform alert
// notifiers replay these durable transitions into notification_outbox with
// deterministic idempotency keys, so a transition committed by a replica that
// crashed before enqueueing is still delivered by any surviving replica.
func (s *Store) RecentPlatformAlertStates(
	ctx context.Context,
	kind string,
	resolvedSince time.Time,
	projectID int,
) ([]PlatformAlertState, error) {
	query := `SELECT ` + platformAlertStateColumns + `
		 FROM platform_alert_states AS st
		 JOIN projects AS p ON p.id = st.project_id
		 WHERE st.kind = ? AND (st.status = 'firing' OR (st.status = 'resolved' AND st.ends_at >= ?))`
	args := []any{kind, resolvedSince.UTC()}
	if projectID > 0 {
		query += ` AND st.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY st.project_id`
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("list recent platform alert states: %w", err)
	}
	return scanPlatformAlertStates(rows)
}

func scanPlatformAlertStates(rows *sql.Rows) ([]PlatformAlertState, error) {
	defer rows.Close()
	var states []PlatformAlertState
	for rows.Next() {
		var (
			st       PlatformAlertState
			startsAt sql.NullTime
			endsAt   sql.NullTime
		)
		if err := rows.Scan(
			&st.ProjectID,
			&st.ProjectSlug,
			&st.Kind,
			&st.Status,
			&st.ActiveCycle,
			&startsAt,
			&endsAt,
			&st.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan platform alert state: %w", err)
		}
		if startsAt.Valid {
			st.StartsAt = startsAt.Time.UTC()
		}
		if endsAt.Valid {
			ts := endsAt.Time.UTC()
			st.EndsAt = &ts
		}
		st.UpdatedAt = st.UpdatedAt.UTC()
		states = append(states, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate platform alert states: %w", err)
	}
	return states, nil
}

func alignPlatformErrorBucket(t time.Time) int64 {
	sec := t.UTC().Unix()
	return (sec / platformErrorRateBucketSeconds) * platformErrorRateBucketSeconds
}

// PlatformErrorRateBatch is one project's share of a completed error-inbox
// batch: every occurrence the batch committed for ProjectID, evaluated against
// that project's error_rate_spike threshold.
type PlatformErrorRateBatch struct {
	ProjectID       int
	ThresholdPerMin int
	OccurredAt      []time.Time
}

// PlatformErrorRateBatchResult reports how one project's durable
// error_rate_spike state moved while its batch was applied.
//
// Transitions lists every firing/resolved step in the order N sequential
// single-event records would have produced them (at most a resolved followed
// by a re-fire of the next cycle). Status, Cycle and StartsAt describe the
// final durable state; Count is the sliding-window total after the batch.
type PlatformErrorRateBatchResult struct {
	ProjectID   int
	Count       int
	Status      string
	Cycle       int64
	StartsAt    time.Time
	Transitions []PlatformErrorSpikeTransition
}

// RecordPlatformErrorRate persists one error occurrence into a 10-second bucket
// for projectID, prunes expired buckets outside window, and advances durable
// platform_alert_states for error_rate_spike when crossing the threshold.
func (s *Store) RecordPlatformErrorRate(
	ctx context.Context,
	projectID int,
	occurredAt, now time.Time,
	window time.Duration,
	thresholdPerMin int,
) (PlatformErrorSpikeTransition, error) {
	if projectID <= 0 || window <= 0 {
		return PlatformErrorSpikeTransition{}, errors.New("platform error rate requires project and positive window")
	}
	results, err := s.RecordPlatformErrorRates(ctx, now, window, []PlatformErrorRateBatch{{
		ProjectID:       projectID,
		ThresholdPerMin: thresholdPerMin,
		OccurredAt:      []time.Time{occurredAt},
	}})
	if err != nil || len(results) == 0 {
		return PlatformErrorSpikeTransition{}, err
	}
	res := results[0]
	if len(res.Transitions) > 0 {
		return res.Transitions[len(res.Transitions)-1], nil
	}
	out := PlatformErrorSpikeTransition{Count: res.Count}
	if res.Status == platformAlertStatusFiring {
		out.Cycle = res.Cycle
		out.StartsAt = res.StartsAt
	}
	return out, nil
}

// RecordPlatformErrorRates applies a whole batch of error occurrences in one
// metadata write transaction, so an error storm costs one SQLite writer-lock
// acquisition per inbox batch instead of one per event (issue #375).
//
// Per project it prunes buckets older than window, upserts the in-window
// occurrences aggregated by (project_id, bucket_epoch), and replays the
// single-event threshold rule in memory with a running window count, so the
// returned transitions and the durable platform_alert_states row are exactly
// what len(OccurredAt) sequential RecordPlatformErrorRate calls at now would
// have produced; only the final state row is written. Occurrences that are
// zero or older than now-window are ignored, and a project with none left is
// not touched. Projects are processed in ascending project_id order and
// buckets in ascending bucket_epoch order so concurrent PostgreSQL writers
// lock rows in one global order. Results are ordered by project_id.
//
// Each project runs under its own savepoint: a project whose accounting fails
// is rolled back alone and reported in the joined error, while the results
// returned alongside that error are committed and must still be acted on.
func (s *Store) RecordPlatformErrorRates(
	ctx context.Context,
	now time.Time,
	window time.Duration,
	batches []PlatformErrorRateBatch,
) ([]PlatformErrorRateBatchResult, error) {
	if window <= 0 {
		return nil, errors.New("platform error rate requires positive window")
	}
	now = now.UTC()
	cutoff := now.Add(-window)
	type projectBatch struct {
		threshold int
		events    int
		buckets   map[int64]int
	}
	byProject := make(map[int]*projectBatch, len(batches))
	for _, batch := range batches {
		if batch.ProjectID <= 0 {
			return nil, errors.New("platform error rate requires project and positive window")
		}
		pb := byProject[batch.ProjectID]
		if pb == nil {
			pb = &projectBatch{threshold: batch.ThresholdPerMin, buckets: make(map[int64]int)}
			byProject[batch.ProjectID] = pb
		}
		for _, occurredAt := range batch.OccurredAt {
			occurredAt = occurredAt.UTC()
			if occurredAt.IsZero() || occurredAt.Before(cutoff) {
				continue
			}
			pb.buckets[alignPlatformErrorBucket(occurredAt)]++
			pb.events++
		}
	}
	projectIDs := make([]int, 0, len(byProject))
	for projectID, pb := range byProject {
		if pb.events > 0 {
			projectIDs = append(projectIDs, projectID)
		}
	}
	if len(projectIDs) == 0 {
		return nil, nil
	}
	slices.Sort(projectIDs)
	cutoffBucket := alignPlatformErrorBucket(cutoff)

	s.platformErrorRateTxs.Add(1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin platform error rate record: %w", err)
	}
	defer tx.Rollback()

	results := make([]PlatformErrorRateBatchResult, 0, len(projectIDs))
	var projectErrs []error
	for _, projectID := range projectIDs {
		pb := byProject[projectID]
		if _, err := tx.ExecContext(ctx, `SAVEPOINT `+platformErrorRateSavepoint); err != nil {
			return nil, fmt.Errorf("open platform error rate savepoint: %w", err)
		}
		res, err := s.recordProjectErrorRate(ctx, tx, projectID, pb.threshold, pb.events, pb.buckets, cutoffBucket,
			now, window)
		if err != nil {
			// Isolate the failing project (for example one deleted mid-batch, whose
			// bucket insert violates the projects foreign key) so it cannot void
			// the accounting of every other project in the batch.
			if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+platformErrorRateSavepoint); rbErr != nil {
				return nil, errors.Join(
					fmt.Errorf("project %d: %w", projectID, err),
					fmt.Errorf("roll back platform error rate savepoint: %w", rbErr),
				)
			}
			projectErrs = append(projectErrs, fmt.Errorf("project %d: %w", projectID, err))
		} else {
			results = append(results, res)
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+platformErrorRateSavepoint); err != nil {
			return nil, fmt.Errorf("release platform error rate savepoint: %w", err)
		}
	}
	if len(results) == 0 {
		return nil, errors.Join(projectErrs...)
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.Join(append(projectErrs, fmt.Errorf("commit platform error rate record: %w", err))...)
	}
	return results, errors.Join(projectErrs...)
}

// platformErrorRateSavepoint scopes one project's accounting inside the batch
// transaction; SQLite and PostgreSQL share the SAVEPOINT syntax.
const platformErrorRateSavepoint = "platform_error_rate_project"

const (
	platformAlertStatusFiring   = "firing"
	platformAlertStatusResolved = "resolved"
)

// platformErrorSpikeStateAttempts bounds how often one project's accounting
// reads and writes its error_rate_spike state: the first insert of a missing
// state row may lose to a concurrent PostgreSQL writer once, after which the
// row exists and is locked by this transaction.
const platformErrorSpikeStateAttempts = 2

// platformErrorSpikeState is a project's durable error_rate_spike state as
// read (and, on PostgreSQL, row-locked) inside the accounting transaction.
type platformErrorSpikeState struct {
	exists   bool
	status   string
	cycle    int64
	startsAt time.Time
}

func (s *Store) recordProjectErrorRate(
	ctx context.Context,
	tx *sql.Tx,
	projectID, thresholdPerMin, events int,
	buckets map[int64]int,
	cutoffBucket int64,
	now time.Time,
	window time.Duration,
) (PlatformErrorRateBatchResult, error) {
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch < ?`),
		projectID,
		cutoffBucket,
	); err != nil {
		return PlatformErrorRateBatchResult{}, fmt.Errorf("prune expired platform error rate buckets: %w", err)
	}
	epochs := make([]int64, 0, len(buckets))
	for epoch := range buckets {
		epochs = append(epochs, epoch)
	}
	slices.Sort(epochs)
	upsert := bind(
		s.dialect,
		`INSERT INTO platform_error_rate_buckets(project_id, bucket_epoch, count) VALUES (?, ?, ?)
		 ON CONFLICT(project_id, bucket_epoch) DO UPDATE SET count = platform_error_rate_buckets.count + excluded.count`,
	)
	for _, epoch := range epochs {
		if _, err := tx.ExecContext(ctx, upsert, projectID, epoch, buckets[epoch]); err != nil {
			return PlatformErrorRateBatchResult{}, fmt.Errorf("record platform error rate bucket: %w", err)
		}
	}

	// The buckets above are written exactly once; only the state read, window
	// sum and replay below repeat. The state row is locked before the window is
	// summed, so on PostgreSQL the sum includes every writer that committed
	// ahead of this one on the row and a stale count cannot resolve a spike a
	// concurrent writer just fired. A missing row takes no lock: concurrent
	// first spikes race on the insert, and the loser (issue #387) re-reads the
	// committed winner row under its lock and replays its own occurrences on
	// top of it instead of failing with 23505.
	for range platformErrorSpikeStateAttempts {
		state, err := s.lockPlatformErrorSpikeState(ctx, tx, projectID)
		if err != nil {
			return PlatformErrorRateBatchResult{}, err
		}
		var totalCount int
		if err := tx.QueryRowContext(
			ctx,
			bind(
				s.dialect,
				`SELECT COALESCE(SUM(count), 0) FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch >= ?`,
			),
			projectID,
			cutoffBucket,
		).Scan(&totalCount); err != nil {
			return PlatformErrorRateBatchResult{}, fmt.Errorf("sum platform error rate buckets: %w", err)
		}
		final, transitions := replayPlatformErrorSpike(state, thresholdPerMin, events, totalCount, now, window)
		written, err := s.writePlatformErrorSpikeState(ctx, tx, projectID, state.exists, final, transitions, now)
		if err != nil {
			return PlatformErrorRateBatchResult{}, err
		}
		if written {
			return PlatformErrorRateBatchResult{
				ProjectID:   projectID,
				Count:       totalCount,
				Status:      final.status,
				Cycle:       final.cycle,
				StartsAt:    final.startsAt,
				Transitions: transitions,
			}, nil
		}
	}
	return PlatformErrorRateBatchResult{}, errors.New("insert platform alert state: concurrent insert conflicted again")
}

func (s *Store) lockPlatformErrorSpikeState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
) (platformErrorSpikeState, error) {
	query := `SELECT status, active_cycle, starts_at FROM platform_alert_states WHERE project_id = ? AND kind = ?`
	if s.dialect == dialect.Postgres {
		query += ` FOR UPDATE`
	}
	var (
		state     platformErrorSpikeState
		prevStart sql.NullTime
	)
	err := tx.QueryRowContext(ctx, bind(s.dialect, query), projectID, platformAlertKindErrorRateSpike).
		Scan(&state.status, &state.cycle, &prevStart)
	if errors.Is(err, sql.ErrNoRows) {
		return platformErrorSpikeState{}, nil
	}
	if err != nil {
		return platformErrorSpikeState{}, fmt.Errorf("read platform alert state: %w", err)
	}
	state.exists = true
	if prevStart.Valid {
		state.startsAt = prevStart.Time.UTC()
	}
	return state, nil
}

// replayPlatformErrorSpike applies the single-event threshold rule once per
// in-window occurrence of the batch, starting from state: the window count
// after the k-th occurrence is the pre-batch total plus k.
func replayPlatformErrorSpike(
	state platformErrorSpikeState,
	thresholdPerMin, events, totalCount int,
	now time.Time,
	window time.Duration,
) (platformErrorSpikeState, []PlatformErrorSpikeTransition) {
	limit := thresholdPerMin * int(window/time.Minute)
	var transitions []PlatformErrorSpikeTransition
	for count := totalCount - events + 1; count <= totalCount; count++ {
		spike := thresholdPerMin > 0 && count > limit
		switch {
		case spike && !state.exists:
			state = platformErrorSpikeState{exists: true, status: platformAlertStatusFiring, cycle: 1, startsAt: now}
		case spike && state.status != platformAlertStatusFiring:
			state.status, state.cycle, state.startsAt = platformAlertStatusFiring, state.cycle+1, now
		case !spike && state.exists && state.status == platformAlertStatusFiring:
			state.status = platformAlertStatusResolved
		default:
			continue
		}
		tr := PlatformErrorSpikeTransition{
			Transition: state.status,
			Cycle:      state.cycle,
			StartsAt:   state.startsAt,
			Count:      count,
		}
		if state.status == platformAlertStatusResolved {
			tr.EndsAt = now
		}
		transitions = append(transitions, tr)
	}
	return state, transitions
}

// writePlatformErrorSpikeState persists the replayed final state. It reports
// false only when the row was missing when read and a concurrent transaction
// inserted it first; nothing was written and the caller must re-read it.
func (s *Store) writePlatformErrorSpikeState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
	hadState bool,
	final platformErrorSpikeState,
	transitions []PlatformErrorSpikeTransition,
	now time.Time,
) (bool, error) {
	switch {
	case len(transitions) == 0:
	case !hadState:
		// On PostgreSQL an insert racing an uncommitted insert of the same key
		// waits for that transaction and, once it commits, does nothing.
		res, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`INSERT INTO platform_alert_states(project_id, kind, status, active_cycle, starts_at, ends_at, updated_at)
				 VALUES (?, ?, 'firing', ?, ?, NULL, ?)
				 ON CONFLICT(project_id, kind) DO NOTHING`,
			),
			projectID, platformAlertKindErrorRateSpike, final.cycle, final.startsAt, now,
		)
		if err != nil {
			return false, fmt.Errorf("insert firing platform alert state: %w", err)
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("read inserted platform alert state: %w", err)
		}
		return inserted > 0, nil
	case final.status == platformAlertStatusFiring:
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE platform_alert_states SET status = 'firing', active_cycle = ?, starts_at = ?, ends_at = NULL, updated_at = ?
				 WHERE project_id = ? AND kind = ?`,
			),
			final.cycle, final.startsAt, now, projectID, platformAlertKindErrorRateSpike,
		); err != nil {
			return false, fmt.Errorf("activate firing platform alert state: %w", err)
		}
	default:
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE platform_alert_states SET status = 'resolved', ends_at = ?, updated_at = ?
				 WHERE project_id = ? AND kind = ? AND status = 'firing'`,
			),
			now, now, projectID, platformAlertKindErrorRateSpike,
		); err != nil {
			return false, fmt.Errorf("resolve platform alert state: %w", err)
		}
	}
	return true, nil
}

type platformErrorSpikeCandidate struct {
	projectID   int
	projectSlug string
	cycle       int64
	startsAt    time.Time
	threshold   int
}

func (s *Store) listFiringErrorSpikeCandidates(ctx context.Context) ([]platformErrorSpikeCandidate, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(
			s.dialect,
			`SELECT st.project_id, p.slug, st.active_cycle, st.starts_at,
				COALESCE(cfg.error_rate_threshold_per_min, 0)
			 FROM platform_alert_states AS st
			 JOIN projects AS p ON p.id = st.project_id
			 LEFT JOIN project_alert_configs AS cfg ON cfg.project_id = st.project_id
			 WHERE st.kind = ? AND st.status = 'firing'`,
		),
		platformAlertKindErrorRateSpike,
	)
	if err != nil {
		return nil, fmt.Errorf("list firing platform alert states: %w", err)
	}
	defer rows.Close()
	var candidates []platformErrorSpikeCandidate
	for rows.Next() {
		var c platformErrorSpikeCandidate
		var startsAt sql.NullTime
		if scanErr := rows.Scan(&c.projectID, &c.projectSlug, &c.cycle, &startsAt, &c.threshold); scanErr != nil {
			return nil, fmt.Errorf("scan firing platform alert state: %w", scanErr)
		}
		if startsAt.Valid {
			c.startsAt = startsAt.Time.UTC()
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate firing platform alert states: %w", err)
	}
	return candidates, nil
}

// CheckPlatformErrorRateRecoveries inspects all projects with a firing
// error_rate_spike state and transitions any whose sliding-window error count
// has fallen at or below threshold (or whose threshold was disabled) to
// resolved.
func (s *Store) CheckPlatformErrorRateRecoveries(
	ctx context.Context,
	now time.Time,
	window time.Duration,
) ([]PlatformErrorSpikeRecovery, error) {
	if window <= 0 {
		return nil, errors.New("platform error rate recovery check requires positive window")
	}
	now = now.UTC()
	cutoffBucket := alignPlatformErrorBucket(now.Add(-window))

	candidates, err := s.listFiringErrorSpikeCandidates(ctx)
	if err != nil {
		return nil, err
	}

	var recoveries []PlatformErrorSpikeRecovery
	for _, c := range candidates {
		recovery, resolved, err := s.recoverPlatformErrorSpike(ctx, c, cutoffBucket, now, window)
		if err != nil {
			return nil, err
		}
		if resolved {
			recoveries = append(recoveries, recovery)
		}
	}
	return recoveries, nil
}

// recoverPlatformErrorSpike resolves one listed firing candidate if its
// window count is at or below threshold. The state row is locked after the
// expired buckets are pruned and before the window is summed, keeping the
// buckets-then-state lock order of recordProjectErrorRate, so on PostgreSQL
// the sum includes every writer that committed ahead of this one on the row
// and a stale count cannot resolve a spike a concurrent writer just kept
// firing (issue #392). A candidate whose row is gone, no longer firing or on
// another cycle by the time the lock is taken is left untouched.
func (s *Store) recoverPlatformErrorSpike(
	ctx context.Context,
	c platformErrorSpikeCandidate,
	cutoffBucket int64,
	now time.Time,
	window time.Duration,
) (PlatformErrorSpikeRecovery, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("begin platform spike recovery tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch < ?`),
		c.projectID,
		cutoffBucket,
	); err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("prune expired platform buckets on recovery: %w", err)
	}
	state, err := s.lockPlatformErrorSpikeState(ctx, tx, c.projectID)
	if err != nil {
		return PlatformErrorSpikeRecovery{}, false, err
	}
	if !state.exists || state.status != platformAlertStatusFiring || state.cycle != c.cycle {
		return PlatformErrorSpikeRecovery{}, false, nil
	}
	var totalCount int
	if err := tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT COALESCE(SUM(count), 0) FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch >= ?`,
		),
		c.projectID,
		cutoffBucket,
	).Scan(&totalCount); err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("sum platform buckets on recovery: %w", err)
	}
	limit := c.threshold * int(window/time.Minute)
	if c.threshold > 0 && totalCount > limit {
		return PlatformErrorSpikeRecovery{}, false, nil
	}
	res, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE platform_alert_states SET status = 'resolved', ends_at = ?, updated_at = ?
			 WHERE project_id = ? AND kind = ? AND status = 'firing' AND active_cycle = ?`,
		),
		now, now, c.projectID, platformAlertKindErrorRateSpike, c.cycle,
	)
	if err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("resolve platform spike state: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("read platform spike recovery result: %w", err)
	}
	if affected == 0 {
		return PlatformErrorSpikeRecovery{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return PlatformErrorSpikeRecovery{}, false, fmt.Errorf("commit platform spike recovery: %w", err)
	}
	return PlatformErrorSpikeRecovery{
		ProjectID:   c.projectID,
		ProjectSlug: c.projectSlug,
		Cycle:       c.cycle,
		StartsAt:    state.startsAt,
		EndsAt:      now,
		Count:       totalCount,
		Threshold:   c.threshold,
	}, true, nil
}

// ClearPlatformErrorRateState resets a project's error-rate buckets and, if an
// error_rate_spike alert is currently firing, transitions it to resolved.
func (s *Store) ClearPlatformErrorRateState(
	ctx context.Context,
	projectID int,
	now time.Time,
) (PlatformErrorSpikeTransition, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("begin clear platform error rate state: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM platform_error_rate_buckets WHERE project_id = ?`),
		projectID,
	); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("clear platform error rate buckets: %w", err)
	}

	var (
		prevStatus string
		prevCycle  int64
		prevStart  sql.NullTime
	)
	var transition PlatformErrorSpikeTransition
	err = tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT status, active_cycle, starts_at FROM platform_alert_states WHERE project_id = ? AND kind = ?`,
		),
		projectID,
		platformAlertKindErrorRateSpike,
	).Scan(&prevStatus, &prevCycle, &prevStart)
	if err == nil && prevStatus == "firing" {
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE platform_alert_states SET status = 'resolved', ends_at = ?, updated_at = ?
				 WHERE project_id = ? AND kind = ? AND status = 'firing'`,
			),
			now, now, projectID, platformAlertKindErrorRateSpike,
		); err != nil {
			return PlatformErrorSpikeTransition{}, fmt.Errorf("resolve cleared platform alert state: %w", err)
		}
		transition.Transition = "resolved"
		transition.Cycle = prevCycle
		if prevStart.Valid {
			transition.StartsAt = prevStart.Time.UTC()
		}
		transition.EndsAt = now
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("read platform alert state for clear: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("commit clear platform error rate state: %w", err)
	}
	return transition, nil
}
