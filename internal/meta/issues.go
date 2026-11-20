package meta

import (
	"context"
	"database/sql"
	"errors"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"math"
	"math/bits"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/issue"
	"github.com/eimsound/lazy-observe/internal/errortrack"
)

type IssueListOptions struct {
	Status string
	Level  string
	Query  string
	Sort   string
	Page   int
}

const (
	IssuePageSize = 50
	MaxIssuePage  = 10_000
)

type IssueView struct {
	ID              int            `json:"id"`
	Fingerprint     string         `json:"fingerprint"`
	Title           string         `json:"title"`
	Level           string         `json:"level"`
	Status          string         `json:"status"`
	Count           int64          `json:"count"`
	UserCount       int64          `json:"user_count"`
	FirstSeen       time.Time      `json:"first_seen"`
	LastSeen        time.Time      `json:"last_seen"`
	StatusChangedAt time.Time      `json:"status_changed_at"`
	RegressedAt     *time.Time     `json:"regressed_at,omitempty"`
	LastEventID     string         `json:"last_event_id"`
	AIAnalysis      map[string]any `json:"ai_analysis,omitempty"`
	AIAnalysisAt    *time.Time     `json:"ai_analysis_at,omitempty"`
	ProjectID       int            `json:"project_id"`
}

func (s *Store) ApplyIssueDeltas(
	ctx context.Context,
	deltas []errortrack.IssueDelta,
) ([]errortrack.IssueChange, error) {
	// SQLite remains serialized inside one process. PostgreSQL correctness does
	// not depend on this mutex: each existing fingerprint row is locked below,
	// and count is updated as count + delta in the database.
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	changes, err := s.applyIssueDeltasTx(ctx, tx, deltas)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
}

func (s *Store) applyIssueDeltasTx(
	ctx context.Context,
	tx *sql.Tx,
	deltas []errortrack.IssueDelta,
) ([]errortrack.IssueChange, error) {
	changes := make([]errortrack.IssueChange, 0)
	for _, delta := range deltas {
		if delta.AcceptedAt.IsZero() || delta.AppliedAt.IsZero() {
			return nil, errors.New("issue delta acceptance and application times are required")
		}
		insert := bind(
			s.dialect,
			`INSERT INTO issues(fingerprint, title, level, status, count, user_count, user_bitmap, first_seen, last_seen, status_changed_at, last_event_id, project_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(project_id, fingerprint) DO NOTHING`,
		)
		result, execErr := tx.ExecContext(
			ctx,
			insert,
			delta.Fingerprint,
			delta.Title,
			delta.Level,
			string(issue.StatusOpen),
			delta.Count,
			estimateUsers(delta.SessionBits),
			int64(delta.SessionBits),
			delta.FirstSeen,
			delta.LastSeen,
			delta.AppliedAt,
			delta.LastEventID,
			delta.ProjectID,
		)
		if execErr != nil {
			return nil, execErr
		}
		inserted, execErr := result.RowsAffected()
		if execErr != nil {
			return nil, execErr
		}
		if inserted == 1 {
			var id int
			if scanErr := tx.QueryRowContext(ctx, bind(s.dialect, `SELECT id FROM issues WHERE project_id = ? AND fingerprint = ?`), delta.ProjectID, delta.Fingerprint).
				Scan(&id); scanErr != nil {
				return nil, scanErr
			}
			changes = append(
				changes,
				errortrack.IssueChange{
					Kind:      "new_issue",
					ProjectID: delta.ProjectID,
					IssueID:   id,
					Title:     delta.Title,
					Level:     delta.Level,
					SeenAt:    delta.LastSeen,
				},
			)
			continue
		}

		query := `SELECT id, status, user_bitmap, first_seen, last_seen, status_changed_at, title, level, last_event_id FROM issues WHERE project_id = ? AND fingerprint = ?`
		if s.dialect == "postgres" {
			query += " FOR UPDATE"
		}
		var id int
		var status string
		var oldBitmap int64
		var firstSeen, lastSeen, statusChangedAt time.Time
		var title, level, lastEventID string
		if scanErr := tx.QueryRowContext(ctx, bind(s.dialect, query), delta.ProjectID, delta.Fingerprint).
			Scan(&id, &status, &oldBitmap, &firstSeen, &lastSeen, &statusChangedAt, &title, &level, &lastEventID); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return nil, sql.ErrNoRows
			}
			return nil, scanErr
		}
		bitmap := uint64(oldBitmap) | delta.SessionBits
		if delta.FirstSeen.Before(firstSeen) {
			firstSeen = delta.FirstSeen
		}
		if !delta.LastSeen.Before(lastSeen) {
			lastSeen, lastEventID, title, level = delta.LastSeen, delta.LastEventID, delta.Title, delta.Level
		}
		var regressedAt any
		nextStatus := status
		regression := status == string(issue.StatusResolved) && delta.AcceptedAt.After(statusChangedAt)
		if regression {
			regressedAt = delta.LastSeen
			nextStatus = string(issue.StatusOpen)
			statusChangedAt = delta.AppliedAt
		}
		update := bind(
			s.dialect,
			`UPDATE issues SET count = count + ?, user_bitmap = ?, user_count = ?, first_seen = ?, last_seen = ?, last_event_id = ?, title = ?, level = ?, status = ?, status_changed_at = ?, regressed_at = COALESCE(?, regressed_at) WHERE id = ?`,
		)
		if _, execErr := tx.ExecContext(
			ctx,
			update,
			delta.Count,
			int64(bitmap),
			estimateUsers(bitmap),
			firstSeen,
			lastSeen,
			lastEventID,
			title,
			level,
			nextStatus,
			statusChangedAt,
			regressedAt,
			id,
		); execErr != nil {
			return nil, execErr
		}
		if regression {
			changes = append(
				changes,
				errortrack.IssueChange{
					Kind:      "regression",
					ProjectID: delta.ProjectID,
					IssueID:   id,
					Title:     title,
					Level:     level,
					SeenAt:    delta.LastSeen,
				},
			)
		}
	}
	return changes, nil
}

func (s *Store) ListIssues(ctx context.Context, projectID int, opts IssueListOptions) ([]IssueView, error) {
	q := s.issueListQuery(projectID, opts)
	if opts.Sort == "count" {
		q.Order(orderDesc(issue.FieldCount))
	} else {
		q.Order(orderDesc(issue.FieldLastSeen))
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	if page > MaxIssuePage || page-1 > math.MaxInt/IssuePageSize {
		return []IssueView{}, nil
	}
	rows, err := q.Offset((page - 1) * IssuePageSize).Limit(IssuePageSize).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]IssueView, 0, len(rows))
	for _, row := range rows {
		result = append(result, issueView(row))
	}
	return result, nil
}

// CountIssues returns the total number of issues matching the same
// project-scoped filters used by ListIssues. Keeping the project predicate in
// this shared builder prevents pagination metadata from leaking another
// project's rows.
func (s *Store) CountIssues(ctx context.Context, projectID int, opts IssueListOptions) (int, error) {
	return s.issueListQuery(projectID, opts).Count(ctx)
}

func (s *Store) issueListQuery(projectID int, opts IssueListOptions) *dbent.IssueQuery {
	q := s.client.Issue.Query().Where(issue.ProjectIDEQ(projectID))
	if opts.Status != "" {
		q.Where(issue.StatusEQ(issue.Status(opts.Status)))
	}
	if opts.Level != "" {
		q.Where(issue.LevelEQ(opts.Level))
	}
	if strings.TrimSpace(opts.Query) != "" {
		q.Where(issue.TitleContainsFold(strings.TrimSpace(opts.Query)))
	}
	return q
}

func (s *Store) GetIssue(ctx context.Context, projectID, id int) (IssueView, error) {
	row, err := s.client.Issue.Query().Where(issue.IDEQ(id), issue.ProjectIDEQ(projectID)).Only(ctx)
	if err != nil {
		return IssueView{}, err
	}
	return issueView(row), nil
}

func (s *Store) SetIssueStatus(
	ctx context.Context,
	projectID, id int,
	status string,
	changedAt time.Time,
) (IssueView, error) {
	if changedAt.IsZero() {
		return IssueView{}, errors.New("issue status change time is required")
	}
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	result, err := s.db.ExecContext(
		ctx,
		bind(s.dialect, `UPDATE issues SET status = ?, status_changed_at = ? WHERE project_id = ? AND id = ?`),
		status,
		changedAt,
		projectID,
		id,
	)
	if err != nil {
		return IssueView{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return IssueView{}, err
	}
	if updated == 0 {
		return IssueView{}, &NotFoundError{Label: "issue"}
	}
	row, err := s.client.Issue.Query().Where(issue.IDEQ(id), issue.ProjectIDEQ(projectID)).Only(ctx)
	if err != nil {
		return IssueView{}, err
	}
	return issueView(row), nil
}

// SetIssuesStatus applies one status to several issues of one project and
// returns how many rows changed. IDs from other projects are ignored.
func (s *Store) SetIssuesStatus(
	ctx context.Context,
	projectID int,
	ids []int,
	status string,
	changedAt time.Time,
) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	validIDs := make([]int, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			validIDs = append(validIDs, id)
		}
	}
	if len(validIDs) == 0 {
		return 0, nil
	}
	if changedAt.IsZero() {
		return 0, errors.New("issue status change time is required")
	}
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	return s.client.Issue.Update().
		Where(issue.ProjectIDEQ(projectID), issue.IDIn(validIDs...)).
		SetStatus(issue.Status(status)).
		SetStatusChangedAt(changedAt).
		Save(ctx)
}

func (s *Store) SaveIssueAnalysis(
	ctx context.Context,
	projectID, id int,
	analysis map[string]any,
	at time.Time,
) (IssueView, error) {
	row, err := s.client.Issue.Query().Where(issue.IDEQ(id), issue.ProjectIDEQ(projectID)).Only(ctx)
	if err != nil {
		return IssueView{}, err
	}
	row, err = row.Update().SetAiAnalysis(analysis).SetAiAnalysisAt(at).Save(ctx)
	if err != nil {
		return IssueView{}, err
	}
	return issueView(row), nil
}

func issueView(row *Issue) IssueView {
	return IssueView{
		ID:              row.ID,
		Fingerprint:     row.Fingerprint,
		Title:           row.Title,
		Level:           row.Level,
		Status:          string(row.Status),
		Count:           row.Count,
		UserCount:       row.UserCount,
		FirstSeen:       row.FirstSeen,
		LastSeen:        row.LastSeen,
		StatusChangedAt: row.StatusChangedAt,
		RegressedAt:     row.RegressedAt,
		LastEventID:     row.LastEventID,
		AIAnalysis:      row.AiAnalysis,
		AIAnalysisAt:    row.AiAnalysisAt,
		ProjectID:       row.ProjectID,
	}
}

func estimateUsers(bitmap uint64) int64 {
	const buckets = 63.0
	occupied := bits.OnesCount64(bitmap)
	if occupied == 0 {
		return 0
	}
	if occupied >= 63 {
		return 263
	}
	return int64(math.Round(-buckets * math.Log(float64(63-occupied)/buckets)))
}
