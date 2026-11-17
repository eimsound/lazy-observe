package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"github.com/eimsound/lazy-observe/internal/errortrack"
)

func (s *Store) AcceptErrorEvents(
	ctx context.Context,
	entries []errortrack.InboxEntry,
	capacity errortrack.InboxCapacity,
) error {
	if len(entries) == 0 || capacity.MaxItems <= 0 || capacity.MaxBytes <= 0 {
		return fmt.Errorf("%w: error batch and capacity are required", errortrack.ErrInvalid)
	}
	type eventKey struct {
		projectID int
		eventID   string
	}
	seen := make(map[eventKey]string, len(entries))
	unique := make([]errortrack.InboxEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.ProjectID <= 0 || entry.EventID == "" || entry.CanonicalDigest == "" || len(entry.Payload) == 0 ||
			entry.PayloadBytes != int64(len(entry.Payload)) || entry.OccurredAt.IsZero() || entry.AcceptedAt.IsZero() {
			return fmt.Errorf("%w: incomplete error Inbox entry", errortrack.ErrInvalid)
		}
		key := eventKey{projectID: entry.ProjectID, eventID: entry.EventID}
		if digest, ok := seen[key]; ok {
			if digest != entry.CanonicalDigest {
				return fmt.Errorf("%w: project %d event %q", errortrack.ErrConflict, entry.ProjectID, entry.EventID)
			}
			continue
		}
		seen[key] = entry.CanonicalDigest
		unique = append(unique, entry)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin error Inbox acceptance: %w", err)
	}
	defer tx.Rollback()
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("lock error Inbox capacity: %w", err)
	}
	newEntries := make([]errortrack.InboxEntry, 0, len(unique))
	for _, entry := range unique {
		var digest string
		err := tx.QueryRowContext(
			ctx,
			bind(s.dialect, `SELECT canonical_digest FROM telemetry_inbox WHERE project_id = ? AND event_id = ?`),
			entry.ProjectID,
			entry.EventID,
		).Scan(&digest)
		if err == nil {
			if digest != entry.CanonicalDigest {
				return fmt.Errorf("%w: project %d event %q", errortrack.ErrConflict, entry.ProjectID, entry.EventID)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read accepted error event %q: %w", entry.EventID, err)
		}
		newEntries = append(newEntries, entry)
	}
	var newBytes int64
	capacityExceeded := int64(len(newEntries)) > capacity.MaxItems-state.activeItems
	for _, entry := range newEntries {
		if entry.PayloadBytes > capacity.MaxBytes || newBytes > capacity.MaxBytes-entry.PayloadBytes {
			capacityExceeded = true
			break
		}
		newBytes += entry.PayloadBytes
	}
	if state.activeBytes > capacity.MaxBytes-newBytes {
		capacityExceeded = true
	}
	if capacityExceeded {
		if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
			rejections_total = rejections_total + 1, updated_at = ? WHERE id = 1`), unique[0].AcceptedAt); err != nil {
			return fmt.Errorf("record error Inbox capacity rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit error Inbox capacity rejection: %w", err)
		}
		return errortrack.ErrCapacity
	}
	snapshots := make(map[string]string)
	for _, entry := range newEntries {
		snapshotKey := fmt.Sprintf("%d\x00%s", entry.ProjectID, entry.Release)
		snapshot, ok := snapshots[snapshotKey]
		if !ok {
			snapshotJSON, snapshotErr := s.sourceMapSnapshotTx(ctx, tx, entry.ProjectID, entry.Release)
			if snapshotErr != nil {
				return fmt.Errorf("capture source map snapshot for event %q: %w", entry.EventID, snapshotErr)
			}
			snapshot = string(snapshotJSON)
			snapshots[snapshotKey] = snapshot
		}
		query := bind(s.dialect, `INSERT INTO telemetry_inbox(
			event_id, canonical_digest, payload, payload_bytes, occurred_at, accepted_at, source_map_snapshot,
			status, available_at, claim_owner, attempts, last_error, outcome, created_at, updated_at, project_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, '', 0, '', '', ?, ?, ?)
		RETURNING id`)
		var insertedID int64
		if err := tx.QueryRowContext(
			ctx,
			query,
			entry.EventID,
			entry.CanonicalDigest,
			entry.Payload,
			entry.PayloadBytes,
			entry.OccurredAt,
			entry.AcceptedAt,
			snapshot,
			entry.AcceptedAt,
			entry.AcceptedAt,
			entry.AcceptedAt,
			entry.ProjectID,
		).Scan(&insertedID); err != nil {
			return fmt.Errorf("accept error event %q: %w", entry.EventID, err)
		}
	}
	if len(newEntries) > 0 {
		if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
			active_items = active_items + ?, active_bytes = active_bytes + ?, updated_at = ? WHERE id = 1`),
			len(newEntries), newBytes, unique[0].AcceptedAt); err != nil {
			return fmt.Errorf("reserve error Inbox capacity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit error Inbox acceptance: %w", err)
	}
	return nil
}

type errorInboxState struct {
	activeItems int64
	activeBytes int64
}

func (s *Store) lockErrorInboxStateTx(ctx context.Context, tx *sql.Tx) (errorInboxState, error) {
	if s.dialect != dialect.Postgres {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE telemetry_inbox_state SET updated_at = updated_at WHERE id = 1`,
		); err != nil {
			return errorInboxState{}, err
		}
	}
	query := `SELECT active_items, active_bytes FROM telemetry_inbox_state WHERE id = 1`
	if s.dialect == dialect.Postgres {
		query += " FOR UPDATE"
	}
	var state errorInboxState
	if err := tx.QueryRowContext(ctx, query).Scan(&state.activeItems, &state.activeBytes); err != nil {
		return errorInboxState{}, err
	}
	return state, nil
}

func (s *Store) sourceMapSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int,
	release string,
) (json.RawMessage, error) {
	snapshot := map[string]string{"kind": "none"}
	if release == "" {
		return json.Marshal(snapshot)
	}
	query := `SELECT filename, sha256 FROM source_map_artifacts
		WHERE project_id = ? AND release = ? ORDER BY filename`
	if s.dialect == dialect.Postgres {
		query += " FOR SHARE"
	}
	rows, err := tx.QueryContext(ctx, bind(s.dialect, query), projectID, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var filename, digest string
		if err := rows.Scan(&filename, &digest); err != nil {
			return nil, err
		}
		snapshot["file:"+filename] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshot) > 1 {
		snapshot["kind"] = "content-addressed-v1"
		snapshot["release"] = release
	}
	return json.Marshal(snapshot)
}

func (s *Store) ClaimErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	if owner == "" || limit <= 0 || now.IsZero() || ttl <= 0 {
		return nil, fmt.Errorf("%w: owner, limit, current time, and TTL are required", errortrack.ErrInvalid)
	}
	if limit > 100 {
		limit = 100
	}
	if s.dialect == dialect.Postgres {
		return s.claimPostgresErrorEvents(ctx, owner, limit, now.UTC(), ttl)
	}
	return s.claimSQLiteErrorEvents(ctx, owner, limit, now.UTC(), ttl)
}

func (s *Store) claimPostgresErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	query := `WITH picked AS (
		SELECT id FROM telemetry_inbox
		WHERE (status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND claim_until <= ?)
		ORDER BY available_at, id FOR UPDATE SKIP LOCKED LIMIT ?
	), claimed AS (
		UPDATE telemetry_inbox AS item SET status = 'processing', claim_owner = ?,
			claim_until = ?, attempts = attempts + 1, updated_at = ?
		FROM picked WHERE item.id = picked.id
		RETURNING item.id, item.project_id, item.event_id, item.payload, item.occurred_at, item.accepted_at,
			item.source_map_snapshot, item.attempts
	)
	SELECT claimed.id, claimed.project_id, projects.slug, claimed.event_id, claimed.payload,
		claimed.occurred_at, claimed.accepted_at, claimed.source_map_snapshot, claimed.attempts
	FROM claimed JOIN projects ON projects.id = claimed.project_id ORDER BY claimed.id`
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, query), now, now, limit, owner, now.Add(ttl), now)
	if err != nil {
		return nil, fmt.Errorf("claim PostgreSQL error Inbox: %w", err)
	}
	defer rows.Close()
	return scanErrorInboxRows(rows)
}

func (s *Store) claimSQLiteErrorEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.InboxItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite error Inbox claim: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM telemetry_inbox
		WHERE (status = 'pending' AND available_at <= ?)
		   OR (status = 'processing' AND claim_until <= ?)
		ORDER BY available_at, id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select SQLite error Inbox: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan SQLite error Inbox ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite error Inbox IDs: %w", err)
	}
	items := make([]errortrack.InboxItem, 0, len(ids))
	for _, id := range ids {
		query := `UPDATE telemetry_inbox SET status = 'processing', claim_owner = ?,
			claim_until = ?, attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND ((status = 'pending' AND available_at <= ?)
				OR (status = 'processing' AND claim_until <= ?))
			RETURNING id, project_id, event_id, payload, occurred_at, accepted_at, source_map_snapshot, attempts`
		var item errortrack.InboxItem
		var snapshot string
		if err := tx.QueryRowContext(ctx, query, owner, now.Add(ttl), now, id, now, now).
			Scan(
				&item.ID,
				&item.ProjectID,
				&item.EventID,
				&item.Payload,
				&item.OccurredAt,
				&item.AcceptedAt,
				&snapshot,
				&item.Attempts,
			); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("claim SQLite error Inbox item %d: %w", id, err)
		}
		item.SourceMapSnapshot = json.RawMessage(snapshot)
		if err := tx.QueryRowContext(ctx, `SELECT slug FROM projects WHERE id = ?`, item.ProjectID).
			Scan(&item.ProjectSlug); err != nil {
			return nil, fmt.Errorf("read error Inbox project %d: %w", item.ProjectID, err)
		}
		items = append(items, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite error Inbox claim: %w", err)
	}
	return items, nil
}

func (s *Store) CompleteErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	delta errortrack.IssueDelta,
	processedAt time.Time,
) ([]errortrack.IssueChange, error) {
	if id <= 0 || owner == "" || delta.ProjectID <= 0 || delta.LastEventID == "" || processedAt.IsZero() {
		return nil, fmt.Errorf("%w: incomplete completion input", errortrack.ErrInvalid)
	}
	s.issueMu.Lock()
	defer s.issueMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin error Inbox completion: %w", err)
	}
	defer tx.Rollback()
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("lock error Inbox completion capacity: %w", err)
	}
	query := `SELECT project_id, event_id, payload_bytes, accepted_at FROM telemetry_inbox
		WHERE id = ? AND status = 'processing' AND claim_owner = ?`
	if s.dialect == dialect.Postgres {
		query += " FOR UPDATE"
	}
	var projectID int
	var eventID string
	var payloadBytes int64
	var acceptedAt time.Time
	if err := tx.QueryRowContext(ctx, bind(s.dialect, query), id, owner).
		Scan(&projectID, &eventID, &payloadBytes, &acceptedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errortrack.ErrClaimLost
		}
		return nil, fmt.Errorf("read error Inbox claim %d: %w", id, err)
	}
	if projectID != delta.ProjectID || eventID != delta.LastEventID {
		return nil, fmt.Errorf("%w: completion identity mismatch", errortrack.ErrInvalid)
	}
	if state.activeItems < 1 || state.activeBytes < payloadBytes {
		return nil, errors.New("error Inbox capacity counters are inconsistent")
	}
	delta.AcceptedAt = acceptedAt
	delta.AppliedAt = processedAt
	changes, err := s.applyIssueDeltasTx(ctx, tx, []errortrack.IssueDelta{delta})
	if err != nil {
		return nil, fmt.Errorf("apply accepted error event %q: %w", eventID, err)
	}
	var issueID int
	if err := tx.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT id FROM issues WHERE project_id = ? AND fingerprint = ?`),
		delta.ProjectID,
		delta.Fingerprint,
	).Scan(&issueID); err != nil {
		return nil, fmt.Errorf("read completed error event issue %q: %w", eventID, err)
	}
	var lifecycle *errortrack.IssueChange
	lifecycleKind := ""
	if len(changes) > 0 {
		lifecycle = &changes[0]
		lifecycleKind = changes[0].Kind
	}
	outcome, err := json.Marshal(struct {
		Fingerprint   string                  `json:"fingerprint"`
		IssueID       int                     `json:"issue_id"`
		LifecycleKind string                  `json:"lifecycle_kind,omitempty"`
		Lifecycle     *errortrack.IssueChange `json:"lifecycle,omitempty"`
	}{
		Fingerprint: delta.Fingerprint, IssueID: issueID, LifecycleKind: lifecycleKind, Lifecycle: lifecycle,
	})
	if err != nil {
		return nil, fmt.Errorf("encode completed error event outcome %q: %w", eventID, err)
	}
	result, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'completed', payload = NULL, payload_bytes = 0, claim_owner = '', claim_until = NULL,
		outcome = ?, processed_at = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ?`),
		string(outcome), processedAt, processedAt, processedAt, id, owner)
	if err != nil {
		return nil, fmt.Errorf("complete error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if updated != 1 {
		return nil, errortrack.ErrClaimLost
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		active_items = active_items - 1, active_bytes = active_bytes - ?, updated_at = ? WHERE id = 1`),
		payloadBytes, processedAt); err != nil {
		return nil, fmt.Errorf("release completed error Inbox capacity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit error Inbox item %d: %w", id, err)
	}
	return changes, nil
}

// IssueIDForErrorEvent resolves the stable event-to-Issue relation retained in
// a completed durable acceptance receipt. Replay uses it to turn an error
// marker's event_id into an exact product link without guessing from the
// Issue's mutable last_event_id.
func (s *Store) IssueIDForErrorEvent(ctx context.Context, projectID int, eventID string) (int, bool, error) {
	var outcome string
	err := s.db.QueryRowContext(
		ctx,
		bind(s.dialect, `SELECT outcome FROM telemetry_inbox
			WHERE project_id = ? AND event_id = ? AND status = 'completed'`),
		projectID,
		eventID,
	).Scan(&outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var receipt struct {
		IssueID int `json:"issue_id"`
	}
	if json.Unmarshal([]byte(outcome), &receipt) != nil || receipt.IssueID <= 0 {
		return 0, false, nil
	}
	return receipt.IssueID, true, nil
}

func (s *Store) RetryErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	retriedAt time.Time,
	availableAt time.Time,
	reason string,
) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin error Inbox retry: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ?`),
		availableAt, reason, retriedAt, id, owner)
	if err != nil {
		return fmt.Errorf("retry error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		retries_total = retries_total + 1, updated_at = ? WHERE id = 1`), retriedAt); err != nil {
		return fmt.Errorf("record error Inbox retry %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit error Inbox retry %d: %w", id, err)
	}
	return nil
}

func (s *Store) ReleaseErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	releasedAt time.Time,
) error {
	result, err := s.db.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'pending', available_at = ?, claim_owner = '', claim_until = NULL,
		attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
		last_error = 'worker_shutdown: released', updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ?`),
		releasedAt, releasedAt, id, owner)
	if err != nil {
		return fmt.Errorf("release error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	return nil
}

func (s *Store) TerminalErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	completedAt time.Time,
	attempts int,
	reason string,
) error {
	if attempts <= 0 {
		return fmt.Errorf("%w: terminal attempt count is required", errortrack.ErrInvalid)
	}
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin terminal error Inbox item: %w", err)
	}
	defer tx.Rollback()
	state, err := s.lockErrorInboxStateTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("lock terminal error Inbox capacity: %w", err)
	}
	query := `SELECT payload_bytes FROM telemetry_inbox WHERE id = ? AND status = 'processing' AND claim_owner = ?`
	if s.dialect == dialect.Postgres {
		query += " FOR UPDATE"
	}
	var payloadBytes int64
	if err := tx.QueryRowContext(ctx, bind(s.dialect, query), id, owner).Scan(&payloadBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errortrack.ErrClaimLost
		}
		return fmt.Errorf("read terminal error Inbox item %d: %w", id, err)
	}
	if state.activeItems < 1 || state.activeBytes < payloadBytes {
		return errors.New("error Inbox capacity counters are inconsistent")
	}
	outcome, _ := json.Marshal(map[string]string{"status": "terminal", "reason": reason})
	result, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox SET
		status = 'terminal', payload = NULL, payload_bytes = 0, claim_owner = '', claim_until = NULL,
		attempts = ?, last_error = ?, outcome = ?, processed_at = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'processing' AND claim_owner = ?`),
		attempts, reason, string(outcome), completedAt, completedAt, completedAt, id, owner)
	if err != nil {
		return fmt.Errorf("mark terminal error Inbox item %d: %w", id, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errortrack.ErrClaimLost
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `UPDATE telemetry_inbox_state SET
		active_items = active_items - 1, active_bytes = active_bytes - ?,
		terminal_failures_total = terminal_failures_total + 1, drops_total = drops_total + 1, updated_at = ?
		WHERE id = 1`), payloadBytes, completedAt); err != nil {
		return fmt.Errorf("record terminal error Inbox item %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit terminal error Inbox item %d: %w", id, err)
	}
	return nil
}

func (s *Store) ErrorInboxDiagnostics(ctx context.Context, recentLimit int) (errortrack.InboxDiagnostics, error) {
	if recentLimit <= 0 || recentLimit > 100 {
		recentLimit = 20
	}
	var diagnostics errortrack.InboxDiagnostics
	if err := s.db.QueryRowContext(ctx, `SELECT active_items, active_bytes, rejections_total, retries_total,
		terminal_failures_total, drops_total FROM telemetry_inbox_state WHERE id = 1`).Scan(
		&diagnostics.ActiveItems,
		&diagnostics.ActiveBytes,
		&diagnostics.RejectionsTotal,
		&diagnostics.RetriesTotal,
		&diagnostics.TerminalFailuresTotal,
		&diagnostics.DropsTotal,
	); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read error Inbox diagnostics: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'processing' THEN 1 ELSE 0 END), 0)
		FROM telemetry_inbox`).Scan(&diagnostics.PendingItems, &diagnostics.ProcessingItems); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read error Inbox backlog: %w", err)
	}
	var oldest time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT accepted_at FROM telemetry_inbox
		WHERE status IN ('pending', 'processing') ORDER BY accepted_at, id LIMIT 1`).Scan(&oldest); err == nil {
		diagnostics.OldestAcceptedAt = &oldest
	} else if !errors.Is(err, sql.ErrNoRows) {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("read oldest error Inbox item: %w", err)
	}
	diagnostics.RecentTerminal = make([]errortrack.TerminalDiagnostic, 0)
	rows, err := s.db.QueryContext(ctx, bind(s.dialect, `SELECT project_id, event_id, attempts, payload_bytes,
		last_error, completed_at FROM telemetry_inbox WHERE status = 'terminal'
		ORDER BY completed_at DESC, id DESC LIMIT ?`), recentLimit)
	if err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("list terminal error Inbox diagnostics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item errortrack.TerminalDiagnostic
		if err := rows.Scan(
			&item.ProjectID,
			&item.EventID,
			&item.Attempts,
			&item.PayloadBytes,
			&item.LastError,
			&item.CompletedAt,
		); err != nil {
			return errortrack.InboxDiagnostics{}, fmt.Errorf("scan terminal error Inbox diagnostic: %w", err)
		}
		diagnostics.RecentTerminal = append(diagnostics.RecentTerminal, item)
	}
	if err := rows.Err(); err != nil {
		return errortrack.InboxDiagnostics{}, fmt.Errorf("iterate terminal error Inbox diagnostics: %w", err)
	}
	return diagnostics, nil
}

func (s *Store) DeleteCompletedErrorReceipts(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin completed error receipt cleanup: %w", err)
	}
	defer tx.Rollback()
	if _, err := s.lockErrorInboxStateTx(ctx, tx); err != nil {
		return 0, fmt.Errorf("lock completed error receipt cleanup: %w", err)
	}
	result, err := tx.ExecContext(ctx, bind(s.dialect, `DELETE FROM telemetry_inbox
		WHERE status IN ('completed', 'terminal') AND completed_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete completed error receipts: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted error receipts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit completed error receipt cleanup: %w", err)
	}
	return int(deleted), nil
}

type errorInboxRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanErrorInboxRows(rows errorInboxRows) ([]errortrack.InboxItem, error) {
	var items []errortrack.InboxItem
	for rows.Next() {
		var item errortrack.InboxItem
		var snapshot string
		if err := rows.Scan(
			&item.ID,
			&item.ProjectID,
			&item.ProjectSlug,
			&item.EventID,
			&item.Payload,
			&item.OccurredAt,
			&item.AcceptedAt,
			&snapshot,
			&item.Attempts,
		); err != nil {
			return nil, fmt.Errorf("scan error Inbox item: %w", err)
		}
		item.SourceMapSnapshot = json.RawMessage(snapshot)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate error Inbox items: %w", err)
	}
	return items, nil
}
