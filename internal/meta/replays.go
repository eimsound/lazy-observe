package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/replaysegment"
	"github.com/eimsound/lazy-observe/internal/db/ent/replaysession"
)

const (
	replayIssueLookupBatchSize         = 200
	replaySegmentAdvisoryLockNamespace = 7302
)

var (
	ErrReplaySegmentConflict = errors.New("replay segment already exists with different content")
	ErrReplayQuotaExceeded   = errors.New("replay session quota exceeded")
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

type ReplaySegmentInput struct {
	ProjectID     int
	SessionID     string
	Seq           int
	StartedAt     time.Time
	LastSeenAt    time.Time
	EventCount    int
	SizeBytes     int64
	PayloadSHA256 string
	MarkersJSON   string
	HasError      bool
	Release       string
	PageURL       string
	MaxSegments   int
	MaxBytes      int64
}

type ReplaySegmentUpsertResult struct {
	Duplicate bool
}

func hashReplaySessionLock(projectID int, sessionID string) int32 {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%d/%s", projectID, sessionID)
	return int32(h.Sum32())
}

// UpsertReplaySegment atomically checks idempotency/conflict and per-session
// quotas, inserts the segment summary into replay_segments, and updates the
// aggregated replay_sessions row inside one database transaction.
func (s *Store) UpsertReplaySegment(
	ctx context.Context,
	input ReplaySegmentInput,
) (ReplaySegmentUpsertResult, error) {
	startedAt := input.StartedAt.UTC().Round(0)
	lastSeenAt := input.LastSeenAt.UTC().Round(0)
	createdAt := time.Now().UTC().Round(0)
	markersJSON := input.MarkersJSON
	if markersJSON == "" {
		markersJSON = "[]"
	}

	if s.dialect != dialect.Postgres {
		s.replayMu.Lock()
		defer s.replayMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("begin replay segment upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			replaySegmentAdvisoryLockNamespace,
			hashReplaySessionLock(input.ProjectID, input.SessionID),
		); err != nil {
			return ReplaySegmentUpsertResult{}, fmt.Errorf("lock replay session: %w", err)
		}
	}

	var existingSHA string
	err = tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT payload_sha256 FROM replay_segments WHERE project_id = ? AND session_id = ? AND seq = ?`,
		),
		input.ProjectID,
		input.SessionID,
		input.Seq,
	).Scan(&existingSHA)
	if err == nil {
		if existingSHA != "" && input.PayloadSHA256 != "" && existingSHA == input.PayloadSHA256 {
			return ReplaySegmentUpsertResult{Duplicate: true}, nil
		}
		return ReplaySegmentUpsertResult{}, ErrReplaySegmentConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("check existing replay segment: %w", err)
	}

	var segCount int
	var segBytes int64
	if err := tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM replay_segments WHERE project_id = ? AND session_id = ?`,
		),
		input.ProjectID,
		input.SessionID,
	).Scan(&segCount, &segBytes); err != nil {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("aggregate replay segments: %w", err)
	}

	var sessCount int
	var sessBytes int64
	err = tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT segment_count, total_bytes FROM replay_sessions WHERE project_id = ? AND session_id = ?`,
		),
		input.ProjectID,
		input.SessionID,
	).Scan(&sessCount, &sessBytes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("read replay session quota: %w", err)
	}

	effectiveCount := max(segCount, sessCount)
	effectiveBytes := max(segBytes, sessBytes)
	if (input.MaxSegments > 0 && effectiveCount >= input.MaxSegments) ||
		(input.MaxBytes > 0 && effectiveBytes+input.SizeBytes > input.MaxBytes) {
		return ReplaySegmentUpsertResult{}, ErrReplayQuotaExceeded
	}

	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `
			INSERT INTO replay_segments(
				session_id, seq, started_at, last_seen_at, event_count,
				size_bytes, payload_sha256, markers_json, created_at, project_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		input.SessionID,
		input.Seq,
		startedAt,
		lastSeenAt,
		input.EventCount,
		input.SizeBytes,
		input.PayloadSHA256,
		markersJSON,
		createdAt,
		input.ProjectID,
	); err != nil {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("insert replay segment: %w", err)
	}

	newCount := effectiveCount + 1
	newBytes := effectiveBytes + input.SizeBytes
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `
			INSERT INTO replay_sessions(
				session_id, started_at, last_seen_at, segment_count, total_bytes,
				has_error, release, page_url, project_id
			)
			VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)
			ON CONFLICT(project_id, session_id) DO UPDATE SET
				started_at = CASE WHEN excluded.started_at < replay_sessions.started_at THEN excluded.started_at ELSE replay_sessions.started_at END,
				last_seen_at = CASE WHEN excluded.last_seen_at > replay_sessions.last_seen_at THEN excluded.last_seen_at ELSE replay_sessions.last_seen_at END,
				segment_count = CASE WHEN excluded.segment_count > replay_sessions.segment_count THEN excluded.segment_count ELSE replay_sessions.segment_count END,
				total_bytes = CASE WHEN excluded.total_bytes > replay_sessions.total_bytes THEN excluded.total_bytes ELSE replay_sessions.total_bytes END,
				has_error = replay_sessions.has_error OR excluded.has_error,
				release = COALESCE(replay_sessions.release, excluded.release),
				page_url = COALESCE(replay_sessions.page_url, excluded.page_url)`),
		input.SessionID,
		startedAt,
		lastSeenAt,
		newCount,
		newBytes,
		input.HasError,
		input.Release,
		input.PageURL,
		input.ProjectID,
	); err != nil {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("upsert replay session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ReplaySegmentUpsertResult{}, fmt.Errorf("commit replay segment upsert: %w", err)
	}
	return ReplaySegmentUpsertResult{}, nil
}

func (s *Store) GetReplaySegment(
	ctx context.Context,
	projectID int,
	sessionID string,
	seq int,
) (*ReplaySegment, error) {
	return s.client.ReplaySegment.Query().
		Where(
			replaysegment.ProjectIDEQ(projectID),
			replaysegment.SessionIDEQ(sessionID),
			replaysegment.SeqEQ(seq),
		).
		Only(ctx)
}

func (s *Store) ListReplaySegments(
	ctx context.Context,
	projectID int,
	sessionID string,
	limit int,
) ([]*ReplaySegment, error) {
	q := s.client.ReplaySegment.Query().
		Where(
			replaysegment.ProjectIDEQ(projectID),
			replaysegment.SessionIDEQ(sessionID),
		).
		Order(orderAsc(replaysegment.FieldSeq))
	if limit > 0 {
		q = q.Limit(limit)
	}
	return q.All(ctx)
}

// ListReplaySessionIDs returns the active replay session IDs for a project so
// retention sweeps can detect orphaned session prefixes in blob storage.
func (s *Store) ListReplaySessionIDs(ctx context.Context, projectID int) (map[string]bool, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(s.dialect, `SELECT session_id FROM replay_sessions WHERE project_id = ?`),
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		result[sessionID] = true
	}
	return result, rows.Err()
}

// ReferencedReplaySegmentSeqs returns the set of committed segment sequence
// numbers for a session. An empty map indicates a legacy session with no
// replay_segments rows.
func (s *Store) ReferencedReplaySegmentSeqs(
	ctx context.Context,
	projectID int,
	sessionID string,
) (map[int]bool, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(s.dialect, `SELECT seq FROM replay_segments WHERE project_id = ? AND session_id = ?`),
		projectID,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int]bool)
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		result[seq] = true
	}
	return result, rows.Err()
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
	if s.dialect != dialect.Postgres {
		s.replayMu.Lock()
		defer s.replayMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID int
	var sessionID string
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT project_id, session_id FROM replay_sessions WHERE id = ?`),
		id,
	).Scan(&projectID, &sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{replaysession.Label}
		}
		return err
	}
	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			replaySegmentAdvisoryLockNamespace,
			hashReplaySessionLock(projectID, sessionID),
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM replay_segments WHERE project_id = ? AND session_id = ?`),
		projectID,
		sessionID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM replay_sessions WHERE id = ?`),
		id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteExpiredReplaySession removes a replay session row and its replay_segments
// rows only if the session is still older than cutoff, preventing a cleanup pass
// from deleting a session that received a new segment after the candidate scan.
func (s *Store) DeleteExpiredReplaySession(ctx context.Context, id int, cutoff time.Time) (bool, error) {
	cutoff = cutoff.UTC().Round(0)
	if s.dialect != dialect.Postgres {
		s.replayMu.Lock()
		defer s.replayMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID int
	var sessionID string
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT project_id, session_id FROM replay_sessions WHERE id = ? AND last_seen_at < ?`),
		id,
		cutoff,
	).Scan(&projectID, &sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if s.dialect == dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`SELECT pg_advisory_xact_lock($1, $2)`,
			replaySegmentAdvisoryLockNamespace,
			hashReplaySessionLock(projectID, sessionID),
		); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM replay_sessions WHERE id = ? AND last_seen_at < ?`),
		id,
		cutoff,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(
		ctx,
		bind(s.dialect, `DELETE FROM replay_segments WHERE project_id = ? AND session_id = ?`),
		projectID,
		sessionID,
	); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
