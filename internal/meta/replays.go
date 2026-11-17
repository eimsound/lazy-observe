package meta

import (
	"context"
	"fmt"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/replaysession"
)

type ReplayListOptions struct {
	HasError *bool
	Start    *time.Time
	End      *time.Time
	Page     int
	Limit    int
}

type ExpiredReplaySession struct {
	ID          int
	SessionID   string
	ProjectID   int
	ProjectSlug string
}

func (s *Store) UpsertReplaySession(
	ctx context.Context,
	projectID int,
	sessionID string,
	startedAt, lastSeenAt time.Time,
	segmentCount int,
	totalBytes int64,
	hasError bool,
	release, pageURL string,
) error {
	_, err := s.db.ExecContext(ctx, bind(s.dialect, `
		INSERT INTO replay_sessions(session_id, started_at, last_seen_at, segment_count, total_bytes, has_error, release, page_url, project_id)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)
		ON CONFLICT(project_id, session_id) DO UPDATE SET
			started_at = CASE WHEN excluded.started_at < replay_sessions.started_at THEN excluded.started_at ELSE replay_sessions.started_at END,
			last_seen_at = CASE WHEN excluded.last_seen_at > replay_sessions.last_seen_at THEN excluded.last_seen_at ELSE replay_sessions.last_seen_at END,
			segment_count = excluded.segment_count,
			total_bytes = excluded.total_bytes,
			has_error = replay_sessions.has_error OR excluded.has_error,
			release = COALESCE(replay_sessions.release, excluded.release),
			page_url = COALESCE(replay_sessions.page_url, excluded.page_url)`),
		sessionID, startedAt, lastSeenAt, segmentCount, totalBytes, hasError, release, pageURL, projectID)
	if err != nil {
		return fmt.Errorf("upsert replay session: %w", err)
	}
	return nil
}

func (s *Store) ListReplaySessions(
	ctx context.Context,
	projectID int,
	opts ReplayListOptions,
) ([]*ReplaySession, error) {
	q := s.replaySessionsQuery(projectID, opts)
	page, limit := opts.Page, opts.Limit
	if page < 1 {
		page = 1
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return q.Order(orderDesc(replaysession.FieldLastSeenAt), orderDesc(replaysession.FieldID)).
		Offset((page - 1) * limit).
		Limit(limit).
		All(ctx)
}

func (s *Store) CountReplaySessions(ctx context.Context, projectID int, opts ReplayListOptions) (int, error) {
	return s.replaySessionsQuery(projectID, opts).Count(ctx)
}

func (s *Store) replaySessionsQuery(projectID int, opts ReplayListOptions) *dbent.ReplaySessionQuery {
	q := s.client.ReplaySession.Query().Where(replaysession.ProjectIDEQ(projectID))
	if opts.HasError != nil {
		q = q.Where(replaysession.HasErrorEQ(*opts.HasError))
	}
	if opts.Start != nil {
		q = q.Where(replaysession.StartedAtGTE(*opts.Start))
	}
	if opts.End != nil {
		q = q.Where(replaysession.StartedAtLTE(*opts.End))
	}
	return q
}

func (s *Store) GetReplaySession(ctx context.Context, projectID int, sessionID string) (*ReplaySession, error) {
	return s.client.ReplaySession.Query().
		Where(replaysession.ProjectIDEQ(projectID), replaysession.SessionIDEQ(sessionID)).
		Only(ctx)
}

func (s *Store) ListExpiredReplaySessions(
	ctx context.Context,
	cutoff time.Time,
	limit int,
) ([]ExpiredReplaySession, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.client.ReplaySession.Query().
		Where(replaysession.LastSeenAtLT(cutoff)).
		WithProject().
		Order(orderAsc(replaysession.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ExpiredReplaySession, 0, len(rows))
	for _, row := range rows {
		project, err := row.Edges.ProjectOrErr()
		if err != nil {
			return nil, err
		}
		result = append(
			result,
			ExpiredReplaySession{
				ID:          row.ID,
				SessionID:   row.SessionID,
				ProjectID:   row.ProjectID,
				ProjectSlug: project.Slug,
			},
		)
	}
	return result, nil
}

func (s *Store) DeleteReplaySession(ctx context.Context, id int) error {
	return s.client.ReplaySession.DeleteOneID(id).Exec(ctx)
}
