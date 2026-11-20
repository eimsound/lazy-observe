package meta

import (
	"context"
	"fmt"
	"time"
)

const retentionDeleteBatchSize = 500

type ExpiredSourceMapRelease struct {
	ProjectID   int
	ProjectSlug string
	Release     string
}

// ListExpiredSourceMapReleases returns whole releases whose newest artifact is
// older than cutoff, so cleanup never leaves a partially retained release.
func (s *Store) ListExpiredSourceMapReleases(
	ctx context.Context,
	cutoff time.Time,
	limit int,
) ([]ExpiredSourceMapRelease, error) {
	if limit <= 0 {
		limit = 100
	}
	cutoff = cutoff.UTC().Round(0)
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, `
		SELECT a.project_id, p.slug, a.release
		FROM source_map_artifacts AS a
		JOIN projects AS p ON p.id = a.project_id
		GROUP BY a.project_id, p.slug, a.release
		HAVING MAX(a.created_at) < ?
		ORDER BY a.project_id, a.release
		LIMIT ?`), cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ExpiredSourceMapRelease, 0)
	for rows.Next() {
		var release ExpiredSourceMapRelease
		if err := rows.Scan(&release.ProjectID, &release.ProjectSlug, &release.Release); err != nil {
			return nil, err
		}
		result = append(result, release)
	}
	return result, rows.Err()
}

func (s *Store) deleteInBatches(
	ctx context.Context,
	label string,
	query string,
	args ...any,
) (int, error) {
	total := 0
	stmt := bind(s.dialect, query)
	batchArgs := append(append([]any(nil), args...), retentionDeleteBatchSize)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result, err := s.db.ExecContext(ctx, stmt, batchArgs...)
		if err != nil {
			return total, fmt.Errorf("delete %s: %w", label, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("count deleted %s: %w", label, err)
		}
		total += int(affected)
		if affected < retentionDeleteBatchSize {
			return total, nil
		}
	}
}

func (s *Store) DeleteExpiredAlertEvents(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired alert events",
		`DELETE FROM alert_events WHERE id IN (SELECT id FROM alert_events WHERE updated_at < ? ORDER BY id LIMIT ?)`,
		cutoff.UTC().Round(0),
	)
}

func (s *Store) DeleteExpiredClosedIssues(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired closed issues",
		`DELETE FROM issues WHERE id IN (
			SELECT id FROM issues WHERE status IN ('resolved', 'ignored') AND last_seen < ? ORDER BY id LIMIT ?
		)`,
		cutoff.UTC().Round(0),
	)
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired sessions",
		`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE expires_at < ? ORDER BY id LIMIT ?)`,
		cutoff.UTC().Round(0),
	)
}

func (s *Store) DeleteExpiredNotificationDeliveries(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired notification deliveries",
		`DELETE FROM notification_deliveries WHERE id IN (
			SELECT id FROM notification_deliveries WHERE created_at < ? ORDER BY id LIMIT ?
		)`,
		cutoff.UTC().Round(0),
	)
}

func (s *Store) DeleteExpiredNotificationOutbox(ctx context.Context, cutoff time.Time) (int, error) {
	return s.deleteInBatches(
		ctx,
		"expired notification outbox",
		`DELETE FROM notification_outbox WHERE id IN (
			SELECT id FROM notification_outbox
			WHERE status IN ('delivered', 'dead') AND updated_at < ?
			ORDER BY id LIMIT ?
		)`,
		cutoff.UTC().Round(0),
	)
}
