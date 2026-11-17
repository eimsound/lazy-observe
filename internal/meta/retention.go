package meta

import (
	"context"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/alertevent"
	"github.com/eimsound/lazy-observe/internal/db/ent/issue"
)

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

func (s *Store) DeleteExpiredAlertEvents(ctx context.Context, cutoff time.Time) (int, error) {
	return s.client.AlertEvent.Delete().Where(alertevent.UpdatedAtLT(cutoff)).Exec(ctx)
}

func (s *Store) DeleteExpiredClosedIssues(ctx context.Context, cutoff time.Time) (int, error) {
	return s.client.Issue.Delete().Where(
		issue.StatusIn(issue.StatusResolved, issue.StatusIgnored),
		issue.LastSeenLT(cutoff),
	).Exec(ctx)
}
