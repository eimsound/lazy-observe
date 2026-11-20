package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
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

const replayIssueLookupBatchSize = 200

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
	startedAt = startedAt.UTC().Round(0)
	lastSeenAt = lastSeenAt.UTC().Round(0)
	_, err := s.db.ExecContext(ctx, bind(s.dialect, `
		INSERT INTO replay_sessions(session_id, started_at, last_seen_at, segment_count, total_bytes, has_error, release, page_url, project_id)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)
		ON CONFLICT(project_id, session_id) DO UPDATE SET
			started_at = CASE WHEN excluded.started_at < replay_sessions.started_at THEN excluded.started_at ELSE replay_sessions.started_at END,
			last_seen_at = CASE WHEN excluded.last_seen_at > replay_sessions.last_seen_at THEN excluded.last_seen_at ELSE replay_sessions.last_seen_at END,
			segment_count = CASE WHEN excluded.segment_count > replay_sessions.segment_count THEN excluded.segment_count ELSE replay_sessions.segment_count END,
			total_bytes = CASE WHEN excluded.total_bytes > replay_sessions.total_bytes THEN excluded.total_bytes ELSE replay_sessions.total_bytes END,
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
	if page-1 > math.MaxInt/limit {
		return []*ReplaySession{}, nil
	}
	offset := (page - 1) * limit
	if offset < 0 || offset > math.MaxInt32 {
		return []*ReplaySession{}, nil
	}
	return q.Order(orderDesc(replaysession.FieldLastSeenAt), orderDesc(replaysession.FieldID)).
		Offset(offset).
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
		q = q.Where(replaysession.StartedAtGTE(opts.Start.UTC().Round(0)))
	}
	if opts.End != nil {
		q = q.Where(replaysession.StartedAtLTE(opts.End.UTC().Round(0)))
	}
	return q
}

func (s *Store) GetReplaySession(ctx context.Context, projectID int, sessionID string) (*ReplaySession, error) {
	return s.client.ReplaySession.Query().
		Where(replaysession.ProjectIDEQ(projectID), replaysession.SessionIDEQ(sessionID)).
		Only(ctx)
}

// IssueIDsForErrorEvents resolves issue IDs for completed error events in bulk
// so replay detail views avoid N+1 metadata lookups.
func (s *Store) IssueIDsForErrorEvents(
	ctx context.Context,
	projectID int,
	eventIDs []string,
) (map[string]int, error) {
	result := make(map[string]int)
	if len(eventIDs) == 0 {
		return result, nil
	}
	unique := make([]string, 0, len(eventIDs))
	seen := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += replayIssueLookupBatchSize {
		end := min(start+replayIssueLookupBatchSize, len(unique))
		batch := unique[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch)+1)
		args = append(args, projectID)
		for _, id := range batch {
			args = append(args, id)
		}
		query := bind(
			s.dialect,
			`SELECT event_id, outcome FROM telemetry_inbox WHERE project_id = ? AND status = 'completed' AND event_id IN (`+placeholders+`)`,
		)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eventID, outcome string
			if err := rows.Scan(&eventID, &outcome); err != nil {
				_ = rows.Close()
				return nil, err
			}
			var receipt struct {
				IssueID int `json:"issue_id"`
			}
			if json.Unmarshal([]byte(outcome), &receipt) == nil && receipt.IssueID > 0 {
				result[eventID] = receipt.IssueID
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
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
		Where(replaysession.LastSeenAtLT(cutoff.UTC().Round(0))).
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

// DeleteExpiredReplaySession removes a replay session row only if it is still
// older than cutoff, preventing a cleanup pass from deleting a session that
// received a new segment after the candidate scan.
func (s *Store) DeleteExpiredReplaySession(ctx context.Context, id int, cutoff time.Time) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM replay_sessions WHERE id = ? AND last_seen_at < ?`),
		id,
		cutoff.UTC().Round(0),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
