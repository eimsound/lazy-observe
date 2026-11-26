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
//
// A state whose stream still has unacknowledged platform_alert_transitions is
// left out: those rows must be planned first, in order, or replaying the
// final state could enqueue firing(C+1) ahead of an earlier resolved(C). The
// state row and its transition rows commit together, so one statement sees
// both or neither.
func (s *Store) RecentPlatformAlertStates(
	ctx context.Context,
	kind string,
	resolvedSince time.Time,
	projectID int,
) ([]PlatformAlertState, error) {
	query := `SELECT ` + platformAlertStateColumns + `
		 FROM platform_alert_states AS st
		 JOIN projects AS p ON p.id = st.project_id
		 WHERE st.kind = ? AND (st.status = 'firing' OR (st.status = 'resolved' AND st.ends_at >= ?))
		 AND NOT EXISTS (
			SELECT 1 FROM platform_alert_transitions AS tr WHERE tr.project_id = st.project_id AND tr.kind = st.kind
		 )`
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
// have produced; only the final state row is written. Every one of those
// transitions is also logged in platform_alert_transitions in the same
// transaction (see PendingPlatformAlertTransitions), so an intermediate
// resolved(C) of a resolved(C) -> firing(C+1) batch survives a crash before
// the outbox enqueue even though the state row only keeps cycle C+1.
// Occurrences that are zero or older than now-window are ignored, and a
// project with none left is not touched. Projects are processed in ascending project_id order and
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
			for _, tr := range transitions {
				count := tr.Count
				if err := s.insertPlatformAlertTransition(
					ctx,
					tx,
					projectID,
					tr,
					&count,
					thresholdPerMin,
					now,
				); err != nil {
					return PlatformErrorRateBatchResult{}, err
				}
			}
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
// resolved. Each resolution is logged in platform_alert_transitions in the
// transaction that resolves it.
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
	if err := s.insertPlatformAlertTransition(ctx, tx, c.projectID, PlatformErrorSpikeTransition{
		Transition: platformAlertStatusResolved,
		Cycle:      c.cycle,
		StartsAt:   state.startsAt,
		EndsAt:     now,
		Count:      totalCount,
	}, &totalCount, c.threshold, now); err != nil {
		return PlatformErrorSpikeRecovery{}, false, err
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
// error_rate_spike alert is currently firing, transitions it to resolved. The
// resolution is logged in platform_alert_transitions in the same transaction
// with no window count, which marks it as a configuration reset. The state
// row is locked after the buckets are cleared, keeping the buckets-then-state
// lock order of recordProjectErrorRate.
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

	state, err := s.lockPlatformErrorSpikeState(ctx, tx, projectID)
	if err != nil {
		return PlatformErrorSpikeTransition{}, err
	}
	var transition PlatformErrorSpikeTransition
	if state.exists && state.status == platformAlertStatusFiring {
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE platform_alert_states SET status = 'resolved', ends_at = ?, updated_at = ?
				 WHERE project_id = ? AND kind = ? AND status = 'firing' AND active_cycle = ?`,
			),
			now, now, projectID, platformAlertKindErrorRateSpike, state.cycle,
		); err != nil {
			return PlatformErrorSpikeTransition{}, fmt.Errorf("resolve cleared platform alert state: %w", err)
		}
		transition = PlatformErrorSpikeTransition{
			Transition: platformAlertStatusResolved,
			Cycle:      state.cycle,
			StartsAt:   state.startsAt,
			EndsAt:     now,
		}
		var threshold int
		if err := tx.QueryRowContext(
			ctx,
			bind(
				s.dialect,
				`SELECT COALESCE(MAX(error_rate_threshold_per_min), 0) FROM project_alert_configs WHERE project_id = ?`,
			),
			projectID,
		).Scan(&threshold); err != nil {
			return PlatformErrorSpikeTransition{}, fmt.Errorf("read platform alert threshold for clear: %w", err)
		}
		if err := s.insertPlatformAlertTransition(ctx, tx, projectID, transition, nil, threshold, now); err != nil {
			return PlatformErrorSpikeTransition{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("commit clear platform error rate state: %w", err)
	}
	return transition, nil
}

// ErrStalePlatformTransitionClaim reports that a platform_alert_transitions
// row is no longer claimed by the caller's token (its lease expired and
// another worker took over, or it was already committed).
var ErrStalePlatformTransitionClaim = errors.New("platform alert transition claim is no longer owned")

// PlatformAlertTransition is one durable platform_alert_transitions row: a
// firing or resolved step of a platform alert, recorded in the same
// transaction that moved platform_alert_states. The row stays until a
// notifier claims it, writes its notification_outbox rows, and commits the
// deletion atomically under its claim_token, so a crash between the state
// commit and the outbox enqueue cannot lose the transition, even when the state
// row has since moved to a later cycle (issue #386).
type PlatformAlertTransition struct {
	ID              int64
	ProjectID       int
	ProjectSlug     string
	Kind            string
	Status          string
	Cycle           int64
	StartsAt        time.Time
	EndsAt          *time.Time
	WindowCount     *int // nil when a configuration reset resolved the alert
	ThresholdPerMin int
	RecordedAt      time.Time
	ClaimToken      string
}

const platformAlertTransitionIsStreamHead = `NOT EXISTS (
	SELECT 1 FROM platform_alert_transitions AS prior
	WHERE prior.project_id = tr.project_id AND prior.kind = tr.kind AND prior.id < tr.id
)`

func (s *Store) insertPlatformAlertTransition(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
	tr PlatformErrorSpikeTransition,
	windowCount *int,
	thresholdPerMin int,
	now time.Time,
) error {
	var startsAt, endsAt any
	if !tr.StartsAt.IsZero() {
		startsAt = tr.StartsAt.UTC()
	}
	if tr.Transition == platformAlertStatusResolved && !tr.EndsAt.IsZero() {
		endsAt = tr.EndsAt.UTC()
	}
	var count any
	if windowCount != nil {
		count = *windowCount
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`INSERT INTO platform_alert_transitions(project_id, kind, status, cycle, starts_at, ends_at, window_count,
				threshold_per_min, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		),
		projectID, platformAlertKindErrorRateSpike, tr.Transition, tr.Cycle, startsAt, endsAt, count,
		thresholdPerMin, now.UTC(),
	); err != nil {
		return fmt.Errorf("record platform alert transition: %w", err)
	}
	return nil
}

// PendingPlatformAlertTransitions lists unacknowledged platform alert
// transitions in commit order per stream: ordered by project, kind and id.
// projectID > 0 filters to one project. Production notifiers must claim head
// rows via ClaimPlatformAlertTransitions rather than planning from this view.
func (s *Store) PendingPlatformAlertTransitions(
	ctx context.Context,
	projectID int,
	limit int,
) ([]PlatformAlertTransition, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	query := `SELECT tr.id, tr.project_id, p.slug, tr.kind, tr.status, tr.cycle, tr.starts_at, tr.ends_at,
		tr.window_count, tr.threshold_per_min, tr.recorded_at, tr.claim_token
		FROM platform_alert_transitions AS tr
		JOIN projects AS p ON p.id = tr.project_id`
	var args []any
	if projectID > 0 {
		query += ` WHERE tr.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY tr.project_id, tr.kind, tr.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("list pending platform alert transitions: %w", err)
	}
	defer rows.Close()
	return scanPlatformAlertTransitions(rows)
}

func scanPlatformAlertTransitionRow(scanner rowScanner, slugOverride string) (PlatformAlertTransition, error) {
	var (
		tr         PlatformAlertTransition
		startsAt   sql.NullTime
		endsAt     sql.NullTime
		count      sql.NullInt64
		claimToken sql.NullString
	)
	var err error
	if slugOverride == "" {
		err = scanner.Scan(&tr.ID, &tr.ProjectID, &tr.ProjectSlug, &tr.Kind, &tr.Status, &tr.Cycle, &startsAt,
			&endsAt, &count, &tr.ThresholdPerMin, &tr.RecordedAt, &claimToken)
	} else {
		tr.ProjectSlug = slugOverride
		err = scanner.Scan(&tr.ID, &tr.ProjectID, &tr.Kind, &tr.Status, &tr.Cycle, &startsAt,
			&endsAt, &count, &tr.ThresholdPerMin, &tr.RecordedAt, &claimToken)
	}
	if err != nil {
		return PlatformAlertTransition{}, err
	}
	if startsAt.Valid {
		tr.StartsAt = startsAt.Time.UTC()
	}
	if endsAt.Valid {
		ts := endsAt.Time.UTC()
		tr.EndsAt = &ts
	}
	if count.Valid {
		value := int(count.Int64)
		tr.WindowCount = &value
	}
	tr.RecordedAt = tr.RecordedAt.UTC()
	tr.ClaimToken = claimToken.String
	return tr, nil
}

func scanPlatformAlertTransitions(rows *sql.Rows) ([]PlatformAlertTransition, error) {
	var out []PlatformAlertTransition
	for rows.Next() {
		tr, err := scanPlatformAlertTransitionRow(rows, "")
		if err != nil {
			return nil, fmt.Errorf("scan platform alert transition: %w", err)
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate platform alert transitions: %w", err)
	}
	return out, nil
}

// ClaimPlatformAlertTransitions exclusively leases at most the oldest
// unacknowledged transition row per (project_id, kind) stream using the shared
// database millisecond clock (ADR 0011), preventing concurrent replicas from
// planning different transitions of the same stream against diverging policy
// snapshots. A leased head row blocks its successors until it is committed via
// CommitPlatformAlertTransition, released, or its claim_until expires.
// projectID > 0 restricts claiming to that project.
func (s *Store) ClaimPlatformAlertTransitions(
	ctx context.Context,
	projectID int,
	claimToken string,
	ttl time.Duration,
	limit int,
) ([]PlatformAlertTransition, error) {
	if claimToken == "" || ttl <= 0 {
		return nil, errors.New("platform alert transition claim requires token and positive TTL")
	}
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if s.dialect == dialect.Postgres {
		return s.claimPostgresPlatformAlertTransitions(ctx, projectID, claimToken, ttl, limit)
	}
	return s.claimSQLitePlatformAlertTransitions(ctx, projectID, claimToken, ttl, limit)
}

func (s *Store) claimPostgresPlatformAlertTransitions(
	ctx context.Context,
	projectID int,
	claimToken string,
	ttl time.Duration,
	limit int,
) ([]PlatformAlertTransition, error) {
	expiry, expiryArg := s.alertLeaseExpiry(ttl)
	projectFilter := ""
	var args []any
	if projectID > 0 {
		projectFilter = ` AND tr.project_id = ?`
		args = append(args, projectID)
	}
	args = append(args, limit, claimToken, expiryArg)
	query := `WITH picked AS (
		SELECT id FROM platform_alert_transitions AS tr
		WHERE (tr.claim_until IS NULL OR tr.claim_until <= CURRENT_TIMESTAMP)
			AND ` + platformAlertTransitionIsStreamHead + projectFilter + `
		ORDER BY tr.project_id, tr.kind, tr.id
		FOR UPDATE SKIP LOCKED
		LIMIT ?
	), updated AS (
		UPDATE platform_alert_transitions AS tr
		SET claim_token = ?, claim_until = ` + expiry + `
		FROM picked
		WHERE tr.id = picked.id
		RETURNING tr.id, tr.project_id, tr.kind, tr.status, tr.cycle, tr.starts_at, tr.ends_at,
			tr.window_count, tr.threshold_per_min, tr.recorded_at, tr.claim_token
	)
	SELECT updated.id, updated.project_id, p.slug, updated.kind, updated.status, updated.cycle,
		updated.starts_at, updated.ends_at, updated.window_count, updated.threshold_per_min,
		updated.recorded_at, updated.claim_token
	FROM updated
	JOIN projects AS p ON p.id = updated.project_id
	ORDER BY updated.project_id, updated.kind, updated.id`
	rows, err := s.db.QueryContext(ctx, s.alertClockSQL(query), args...)
	if err != nil {
		return nil, fmt.Errorf("claim PostgreSQL platform alert transitions: %w", err)
	}
	defer rows.Close()
	return scanPlatformAlertTransitions(rows)
}

func (s *Store) selectClaimableSQLitePlatformTransitionIDsTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
	limit int,
) ([]int64, error) {
	query := `SELECT id FROM platform_alert_transitions AS tr
		WHERE (tr.claim_until IS NULL OR tr.claim_until <= CURRENT_TIMESTAMP)
			AND ` + platformAlertTransitionIsStreamHead
	var args []any
	if projectID > 0 {
		query += ` AND tr.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY tr.project_id, tr.kind, tr.id LIMIT ?`
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, s.alertClockSQL(query), args...)
	if err != nil {
		return nil, fmt.Errorf("select SQLite platform alert transitions: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan SQLite platform alert transition ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite platform alert transition IDs: %w", err)
	}
	return ids, nil
}

func (s *Store) claimSQLitePlatformAlertTransitions(
	ctx context.Context,
	projectID int,
	claimToken string,
	ttl time.Duration,
	limit int,
) ([]PlatformAlertTransition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite platform alert transition claim: %w", err)
	}
	defer tx.Rollback()
	ids, err := s.selectClaimableSQLitePlatformTransitionIDsTx(ctx, tx, projectID, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	claimed := make([]PlatformAlertTransition, 0, len(ids))
	slugs := make(map[int]string)
	expiry, expiryArg := s.alertLeaseExpiry(ttl)
	updateQuery := s.alertClockSQL(
		`UPDATE platform_alert_transitions AS tr
		SET claim_token = ?, claim_until = ` + expiry + `
		WHERE tr.id = ?
			AND (tr.claim_until IS NULL OR tr.claim_until <= CURRENT_TIMESTAMP)
			AND ` + platformAlertTransitionIsStreamHead + `
		RETURNING id, project_id, kind, status, cycle, starts_at, ends_at,
			window_count, threshold_per_min, recorded_at, claim_token`,
	)
	for _, id := range ids {
		tr, err := scanPlatformAlertTransitionRow(
			tx.QueryRowContext(ctx, updateQuery, claimToken, expiryArg, id),
			"_pending_",
		)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("claim SQLite platform alert transition %d: %w", id, err)
		}
		slug, cached := slugs[tr.ProjectID]
		if !cached {
			if err := tx.QueryRowContext(ctx, `SELECT slug FROM projects WHERE id = ?`, tr.ProjectID).
				Scan(&slug); err != nil {
				return nil, fmt.Errorf("read project slug for platform alert transition %d: %w", id, err)
			}
			slugs[tr.ProjectID] = slug
		}
		tr.ProjectSlug = slug
		claimed = append(claimed, tr)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite platform alert transition claim: %w", err)
	}
	return claimed, nil
}

// CommitPlatformAlertTransition atomically deletes a claimed transition row
// (fenced by claimToken) and inserts any planned notification_outbox entries in
// one transaction. Returns ErrStalePlatformTransitionClaim with zero outbox
// rows written if another worker took over the lease or already committed it.
func (s *Store) CommitPlatformAlertTransition(
	ctx context.Context,
	id int64,
	claimToken string,
	entries []NotificationOutboxEnqueue,
) (int, error) {
	if id <= 0 || claimToken == "" {
		return 0, errors.New("commit platform alert transition requires positive ID and claim token")
	}
	for _, entry := range entries {
		if err := validateNotificationOutboxEnqueue(entry); err != nil {
			return 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin commit platform alert transition %d: %w", id, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM platform_alert_transitions WHERE id = ? AND claim_token = ?`),
		id,
		claimToken,
	)
	if err != nil {
		return 0, fmt.Errorf("delete claimed platform alert transition %d: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read deleted platform alert transition %d rows: %w", id, err)
	}
	if affected == 0 {
		return 0, ErrStalePlatformTransitionClaim
	}

	inserted := 0
	for _, entry := range entries {
		_, ok, err := s.insertNotificationOutbox(ctx, tx, entry)
		if err != nil {
			return 0, err
		}
		if ok {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit platform alert transition %d: %w", id, err)
	}
	return inserted, nil
}

// ReleasePlatformAlertTransition clears a transition's lease if claimToken
// still owns it, allowing another pass or replica to pick it up immediately
// instead of waiting for claim_until to expire.
func (s *Store) ReleasePlatformAlertTransition(ctx context.Context, id int64, claimToken string) error {
	if id <= 0 || claimToken == "" {
		return errors.New("release platform alert transition requires positive ID and claim token")
	}
	if _, err := s.db.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE platform_alert_transitions SET claim_token = NULL, claim_until = NULL
			 WHERE id = ? AND claim_token = ?`,
		),
		id,
		claimToken,
	); err != nil {
		return fmt.Errorf("release platform alert transition %d: %w", id, err)
	}
	return nil
}
