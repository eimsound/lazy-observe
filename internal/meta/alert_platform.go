package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	now = now.UTC()
	occurredAt = occurredAt.UTC()
	cutoff := now.Add(-window)
	if occurredAt.IsZero() || occurredAt.Before(cutoff) {
		return PlatformErrorSpikeTransition{}, nil
	}
	bucket := alignPlatformErrorBucket(occurredAt)
	cutoffBucket := alignPlatformErrorBucket(cutoff)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("begin platform error rate record: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch < ?`),
		projectID,
		cutoffBucket,
	); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("prune expired platform error rate buckets: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`INSERT INTO platform_error_rate_buckets(project_id, bucket_epoch, count) VALUES (?, ?, 1)
			 ON CONFLICT(project_id, bucket_epoch) DO UPDATE SET count = platform_error_rate_buckets.count + 1`,
		),
		projectID,
		bucket,
	); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("record platform error rate bucket: %w", err)
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
		return PlatformErrorSpikeTransition{}, fmt.Errorf("sum platform error rate buckets: %w", err)
	}

	limit := thresholdPerMin * int(window/time.Minute)
	spike := thresholdPerMin > 0 && totalCount > limit

	selectState := `SELECT status, active_cycle, starts_at FROM platform_alert_states WHERE project_id = ? AND kind = ?`
	if s.dialect == dialect.Postgres {
		selectState += ` FOR UPDATE`
	}
	var (
		prevStatus string
		prevCycle  int64
		prevStart  sql.NullTime
	)
	stateErr := tx.QueryRowContext(ctx, bind(s.dialect, selectState), projectID, platformAlertKindErrorRateSpike).
		Scan(&prevStatus, &prevCycle, &prevStart)
	hasState := true
	if errors.Is(stateErr, sql.ErrNoRows) {
		hasState = false
	} else if stateErr != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("read platform alert state: %w", stateErr)
	}

	result := PlatformErrorSpikeTransition{Count: totalCount}
	if spike {
		if !hasState {
			const firstCycle int64 = 1
			if _, err := tx.ExecContext(
				ctx,
				bind(
					s.dialect,
					`INSERT INTO platform_alert_states(project_id, kind, status, active_cycle, starts_at, ends_at, updated_at)
					 VALUES (?, ?, 'firing', ?, ?, NULL, ?)`,
				),
				projectID, platformAlertKindErrorRateSpike, firstCycle, now, now,
			); err != nil {
				return PlatformErrorSpikeTransition{}, fmt.Errorf("insert firing platform alert state: %w", err)
			}
			result.Transition = "firing"
			result.Cycle = firstCycle
			result.StartsAt = now
		} else if prevStatus != "firing" {
			nextCycle := prevCycle + 1
			if _, err := tx.ExecContext(
				ctx,
				bind(
					s.dialect,
					`UPDATE platform_alert_states SET status = 'firing', active_cycle = ?, starts_at = ?, ends_at = NULL, updated_at = ?
					 WHERE project_id = ? AND kind = ?`,
				),
				nextCycle, now, now, projectID, platformAlertKindErrorRateSpike,
			); err != nil {
				return PlatformErrorSpikeTransition{}, fmt.Errorf("activate firing platform alert state: %w", err)
			}
			result.Transition = "firing"
			result.Cycle = nextCycle
			result.StartsAt = now
		} else {
			result.Cycle = prevCycle
			if prevStart.Valid {
				result.StartsAt = prevStart.Time.UTC()
			}
		}
	} else if hasState && prevStatus == "firing" {
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE platform_alert_states SET status = 'resolved', ends_at = ?, updated_at = ?
				 WHERE project_id = ? AND kind = ? AND status = 'firing'`,
			),
			now, now, projectID, platformAlertKindErrorRateSpike,
		); err != nil {
			return PlatformErrorSpikeTransition{}, fmt.Errorf("resolve platform alert state: %w", err)
		}
		result.Transition = "resolved"
		result.Cycle = prevCycle
		if prevStart.Valid {
			result.StartsAt = prevStart.Time.UTC()
		}
		result.EndsAt = now
	}

	if err := tx.Commit(); err != nil {
		return PlatformErrorSpikeTransition{}, fmt.Errorf("commit platform error rate record: %w", err)
	}
	return result, nil
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
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("begin platform spike recovery tx: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, `DELETE FROM platform_error_rate_buckets WHERE project_id = ? AND bucket_epoch < ?`),
			c.projectID,
			cutoffBucket,
		); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("prune expired platform buckets on recovery: %w", err)
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
			_ = tx.Rollback()
			return nil, fmt.Errorf("sum platform buckets on recovery: %w", err)
		}
		limit := c.threshold * int(window/time.Minute)
		stillSpiking := c.threshold > 0 && totalCount > limit
		if stillSpiking {
			_ = tx.Rollback()
			continue
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
			_ = tx.Rollback()
			return nil, fmt.Errorf("resolve platform spike state: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("read platform spike recovery result: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit platform spike recovery: %w", err)
		}
		if affected > 0 {
			recoveries = append(recoveries, PlatformErrorSpikeRecovery{
				ProjectID:   c.projectID,
				ProjectSlug: c.projectSlug,
				Cycle:       c.cycle,
				StartsAt:    c.startsAt,
				EndsAt:      now,
				Count:       totalCount,
				Threshold:   c.threshold,
			})
		}
	}
	return recoveries, nil
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
